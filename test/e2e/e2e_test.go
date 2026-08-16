//go:build e2e

// Package e2e exercises the sidecar against a real Kubernetes API server in a
// kind cluster.
//
// The unit suites cover the derivation and merge rules in isolation. What they
// cannot cover is whether the watches, the CRD schema and the informer cache
// actually behave as assumed, which is exactly what breaks in a real cluster.
// So these tests run the built binary against a real API server and assert on
// the file it writes.
//
// Run with: make test-e2e
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

const (
	clusterName = "gatus-sidecar-e2e"
	namespace   = "gatus-e2e"
)

var env struct {
	kubeconfig string
	binary     string
	repoRoot   string
}

func TestMain(m *testing.M) {
	code, err := setupAndRun(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e setup failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func setupAndRun(m *testing.M) (int, error) {
	root, err := repoRoot()
	if err != nil {
		return 0, err
	}
	env.repoRoot = root

	// Build first: a compile error should fail before a cluster is created.
	env.binary = filepath.Join(root, "bin", "gatus-sidecar")
	if out, err := run(root, "go", "build", "-o", env.binary, "./cmd/gatus-sidecar"); err != nil {
		return 0, fmt.Errorf("build sidecar: %w\n%s", err, out)
	}

	created, kubeconfig, err := ensureCluster(root)
	if err != nil {
		return 0, err
	}
	env.kubeconfig = kubeconfig

	// Keeping the cluster is useful when a test fails and the state matters.
	// A cluster this run did not create is never deleted either, so a developer
	// can leave one running between runs.
	if created && os.Getenv("E2E_KEEP_CLUSTER") == "" {
		teardown := func() { _, _ = run(root, "kind", "delete", "cluster", "--name", clusterName) }
		defer teardown()

		// A deferred call does not run if the process is interrupted, and a
		// leaked kind cluster keeps a control plane running until someone
		// notices. Ctrl-C and a CI job cancellation both arrive as signals.
		stop := onSignal(func() {
			teardown()
			os.Exit(130)
		})
		defer stop()
	}

	if err := installCRDs(root); err != nil {
		return 0, err
	}
	if out, err := kubectl(root, "create", "namespace", namespace); err != nil && !strings.Contains(out, "AlreadyExists") {
		return 0, fmt.Errorf("create namespace: %w\n%s", err, out)
	}

	return m.Run(), nil
}

// onSignal runs fn when the process is interrupted or terminated, and returns a
// function that stops listening.
func onSignal(fn func()) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			fn()
		case <-done:
		}
	}()

	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// ensureCluster creates the kind cluster unless it is already running, and
// returns the kubeconfig to talk to it.
func ensureCluster(root string) (created bool, kubeconfig string, err error) {
	kubeconfig = filepath.Join(root, "bin", "e2e.kubeconfig")

	existing, err := run(root, "kind", "get", "clusters")
	if err != nil {
		return false, "", fmt.Errorf("list kind clusters: %w\n%s", err, existing)
	}

	if !containsLine(existing, clusterName) {
		if out, err := run(root, "kind", "create", "cluster", "--name", clusterName, "--wait", "120s"); err != nil {
			return false, "", fmt.Errorf("create kind cluster: %w\n%s", err, out)
		}
		created = true
	}

	if out, err := run(root, "kind", "export", "kubeconfig", "--name", clusterName, "--kubeconfig", kubeconfig); err != nil {
		return created, "", fmt.Errorf("export kubeconfig: %w\n%s", err, out)
	}
	return created, kubeconfig, nil
}

func installCRDs(root string) error {
	manifests := []string{
		filepath.Join(root, "config", "crd", "bases"),
		filepath.Join(root, "test", "e2e", "testdata", "traefik-ingressroute-crd.yaml"),
	}
	for _, m := range manifests {
		if out, err := kubectl(root, "apply", "-f", m); err != nil {
			return fmt.Errorf("apply %s: %w\n%s", m, err, out)
		}
	}

	// Applying a CRD and imstorefronttely using it races the API server's discovery
	// refresh, so wait for it to be served.
	for _, name := range []string{"endpointtemplates.gatus.kalexlab.xyz", "ingressroutes.traefik.io"} {
		if out, err := kubectl(root, "wait", "--for=condition=Established", "--timeout=60s", "crd/"+name); err != nil {
			return fmt.Errorf("wait for crd %s: %w\n%s", name, err, out)
		}
	}
	return nil
}

// sidecar is a running sidecar process writing to a temp file.
type sidecar struct {
	t      *testing.T
	cmd    *exec.Cmd
	output string
	stderr *strings.Builder
}

// startSidecar runs the binary against the kind cluster. Running it as a
// process rather than in-process is deliberate: it exercises the same flag
// parsing, manager setup and signal handling that runs in production.
func startSidecar(t *testing.T, baseConfig string, extraArgs ...string) *sidecar {
	t.Helper()

	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.yaml")
	if err := os.WriteFile(basePath, []byte(baseConfig), 0o644); err != nil {
		t.Fatalf("write base config: %v", err)
	}
	output := filepath.Join(dir, "out", "config.yaml")

	args := append([]string{
		"--kubeconfig=" + env.kubeconfig,
		"--base-config=" + basePath,
		"--output=" + output,
		// Short, so tests are not dominated by the debounce window.
		"--debounce=100ms",
		// Disabled: several sidecars run across these tests and would collide.
		"--metrics-bind-address=0",
		"--health-probe-bind-address=0",
	}, extraArgs...)

	cmd := exec.Command(env.binary, args...)
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sidecar: %v", err)
	}

	s := &sidecar{t: t, cmd: cmd, output: output, stderr: stderr}
	t.Cleanup(s.stop)
	return s
}

func (s *sidecar) stop() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
}

// endpoints reads the rendered endpoints, keyed by group/name. It returns nil
// while the file does not exist, so it can be polled.
func (s *sidecar) endpoints() map[string]map[string]any {
	raw, err := os.ReadFile(s.output)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	list, ok := doc["endpoints"].([]any)
	if !ok {
		return nil
	}

	out := make(map[string]map[string]any, len(list))
	for _, item := range list {
		ep, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := ep["name"].(string)
		group, _ := ep["group"].(string)
		key := name
		if group != "" {
			key = group + "/" + name
		}
		out[key] = ep
	}
	return out
}

// document returns the whole rendered config.
func (s *sidecar) document() map[string]any {
	raw, err := os.ReadFile(s.output)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	return doc
}

// await polls until cond holds, failing with the sidecar's log on timeout.
func (s *sidecar) await(what string, cond func(map[string]map[string]any) bool) map[string]map[string]any {
	s.t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	var last map[string]map[string]any
	for time.Now().Before(deadline) {
		last = s.endpoints()
		if cond(last) {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}

	s.t.Fatalf("timed out waiting for %s\nendpoints: %v\nsidecar log:\n%s",
		what, keys(last), s.stderr.String())
	return nil
}

// apply pipes a manifest to kubectl, removing it again when the test ends.
func apply(t *testing.T, manifest string) {
	t.Helper()

	if out, err := kubectlStdin(manifest, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply manifest: %v\n%s\n%s", err, out, manifest)
	}
	t.Cleanup(func() {
		_, _ = kubectlStdin(manifest, "delete", "-f", "-", "--ignore-not-found", "--wait=false")
	})
}

const baseConfig = `
storage:
  type: memory
alerting:
  telegram:
    token: "${TELEGRAM_TOKEN}"
    id: "${TELEGRAM_PRIMARY_ID}"
ui:
  default-sort-by: group
announcements: []
`

// templates are the three that replace the YAML anchors of a hand-written config.
const templates = `
apiVersion: gatus.kalexlab.xyz/v1alpha1
kind: EndpointTemplate
metadata:
  name: e2e-common
spec:
  endpoint:
    interval: 1m
    alerts:
      - type: telegram
      - type: telegram
        provider-override:
          id: "${TELEGRAM_SECONDARY_ID}"
---
apiVersion: gatus.kalexlab.xyz/v1alpha1
kind: EndpointTemplate
metadata:
  name: e2e-http
spec:
  extends: [e2e-common]
  defaultFor: [http, https]
  scheme: http
  endpoint:
    conditions: ["[STATUS] == 200"]
---
apiVersion: gatus.kalexlab.xyz/v1alpha1
kind: EndpointTemplate
metadata:
  name: e2e-tcp
spec:
  extends: [e2e-common]
  defaultFor: [tcp]
  scheme: tcp
  endpoint:
    conditions: ["[CONNECTED] == true"]
`

func TestServiceDiscovery(t *testing.T) {
	apply(t, templates)
	apply(t, `
apiVersion: v1
kind: Service
metadata:
  name: auth
  namespace: `+namespace+`
  annotations:
    gatus.io/enabled: "true"
    gatus.io/group: Online Shop
spec:
  ports:
    - port: 9091
---
apiVersion: v1
kind: Service
metadata:
  name: database-rw
  namespace: `+namespace+`
  annotations:
    gatus.io/enabled: "true"
    gatus.io/group: Online Shop
    gatus.io/scheme: tcp
    gatus.io/name: Cluster
spec:
  ports:
    - port: 5432
---
apiVersion: v1
kind: Service
metadata:
  name: not-monitored
  namespace: `+namespace+`
spec:
  ports:
    - port: 80
`)

	s := startSidecar(t, baseConfig)
	eps := s.await("both annotated services", func(eps map[string]map[string]any) bool {
		_, a := eps["Online Shop/Auth"]
		_, b := eps["Online Shop/Cluster"]
		return a && b
	})

	http := eps["Online Shop/Auth"]
	if want := fmt.Sprintf("http://auth.%s.svc.cluster.local:9091", namespace); http["url"] != want {
		t.Errorf("url = %v, want %v", http["url"], want)
	}
	// The defaultFor template applied without the workload naming it, and its
	// extends chain came with it.
	if http["interval"] != "1m" {
		t.Errorf("interval = %v, want 1m from the template chain", http["interval"])
	}
	if alerts, _ := http["alerts"].([]any); len(alerts) != 2 {
		t.Errorf("alerts = %#v, want the two from e2e-common", http["alerts"])
	}
	if conditions, _ := http["conditions"].([]any); len(conditions) != 1 || conditions[0] != "[STATUS] == 200" {
		t.Errorf("conditions = %#v", http["conditions"])
	}

	// The scheme selected the tcp template instead.
	tcp := eps["Online Shop/Cluster"]
	if want := fmt.Sprintf("tcp://database-rw.%s.svc.cluster.local:5432", namespace); tcp["url"] != want {
		t.Errorf("url = %v, want %v", tcp["url"], want)
	}
	if conditions, _ := tcp["conditions"].([]any); len(conditions) != 1 || conditions[0] != "[CONNECTED] == true" {
		t.Errorf("conditions = %#v", tcp["conditions"])
	}

	// Opt-in is the default, so an unannotated Service must never appear.
	for key := range eps {
		if strings.Contains(key, "Not monitored") {
			t.Errorf("an unannotated Service was monitored: %v", keys(eps))
		}
	}

	// The base config is passed through, placeholders and all.
	doc := s.document()
	for _, key := range []string{"storage", "alerting", "ui", "announcements"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("base config lost %q", key)
		}
	}
	raw, _ := os.ReadFile(s.output)
	for _, placeholder := range []string{"${TELEGRAM_TOKEN}", "${TELEGRAM_SECONDARY_ID}"} {
		if !strings.Contains(string(raw), placeholder) {
			t.Errorf("output lost %s; the sidecar must never expand placeholders", placeholder)
		}
	}
}

func TestPatchAndMultipleEndpointsPerService(t *testing.T) {
	apply(t, templates)
	apply(t, `
apiVersion: v1
kind: Service
metadata:
  name: portal
  namespace: `+namespace+`
  annotations:
    gatus.io/enabled: "true"
    gatus.io/group: Platform
    gatus.io/endpoint: |
      conditions:
        - "[STATUS] > 400"
        - "[STATUS] < 500"
spec:
  ports:
    - port: 80
---
apiVersion: v1
kind: Service
metadata:
  name: object-store
  namespace: `+namespace+`
  annotations:
    gatus.io/enabled: "true"
    gatus.io/group: Platform
    gatus.io/endpoints: |
      - name: Object store
        port: 9000
        path: /health
      - name: Object store console
        port: 9001
spec:
  ports:
    - name: api
      port: 9000
    - name: ui
      port: 9001
`)

	s := startSidecar(t, baseConfig)
	eps := s.await("the patched and multi-endpoint services", func(eps map[string]map[string]any) bool {
		_, a := eps["Platform/Portal"]
		_, b := eps["Platform/Object store"]
		_, c := eps["Platform/Object store console"]
		return a && b && c
	})

	// The patch replaced the template's conditions rather than adding to them.
	conditions, _ := eps["Platform/Portal"]["conditions"].([]any)
	if len(conditions) != 2 || conditions[0] != "[STATUS] > 400" {
		t.Errorf("conditions = %#v, want the patch's two", eps["Platform/Portal"]["conditions"])
	}
	// Everything else from the template survived the patch.
	if eps["Platform/Portal"]["interval"] != "1m" {
		t.Errorf("interval = %v, want 1m", eps["Platform/Portal"]["interval"])
	}

	if want := fmt.Sprintf("http://object-store.%s.svc.cluster.local:9000/health", namespace); eps["Platform/Object store"]["url"] != want {
		t.Errorf("url = %v, want %v", eps["Platform/Object store"]["url"], want)
	}
	if want := fmt.Sprintf("http://object-store.%s.svc.cluster.local:9001", namespace); eps["Platform/Object store console"]["url"] != want {
		t.Errorf("url = %v, want %v", eps["Platform/Object store console"]["url"], want)
	}
}

func TestIngressRouteDiscovery(t *testing.T) {
	apply(t, templates)
	apply(t, `
apiVersion: v1
kind: Service
metadata:
  name: shop
  namespace: `+namespace+`
spec:
  ports:
    - port: 2283
---
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: shop
  namespace: `+namespace+`
  annotations:
    gatus.io/enabled: "true"
    gatus.io/group: Shop
spec:
  entryPoints: [websecure]
  routes:
    - kind: Rule
      match: Host(`+"`shop.example.com`"+`)
      services:
        - name: shop
          port: 2283
`)

	s := startSidecar(t, baseConfig)
	eps := s.await("both views of the ingress route", func(eps map[string]map[string]any) bool {
		_, a := eps["Shop/Shop"]
		_, b := eps["Shop/Shop (external)"]
		return a && b
	})

	if eps["Shop/Shop (external)"]["url"] != "https://shop.example.com" {
		t.Errorf("external url = %v", eps["Shop/Shop (external)"]["url"])
	}
	if want := fmt.Sprintf("http://shop.%s.svc.cluster.local:2283", namespace); eps["Shop/Shop"]["url"] != want {
		t.Errorf("internal url = %v, want %v", eps["Shop/Shop"]["url"], want)
	}
	// Both inherit the template chain.
	for _, key := range []string{"Shop/Shop", "Shop/Shop (external)"} {
		if eps[key]["interval"] != "1m" {
			t.Errorf("%s interval = %v, want 1m", key, eps[key]["interval"])
		}
	}
}

func TestTemplateEditRewritesEveryEndpoint(t *testing.T) {
	// The central claim of the design: templates resolve at render time, so
	// editing one re-renders every endpoint using it with no reverse index.
	apply(t, templates)
	apply(t, `
apiVersion: v1
kind: Service
metadata:
  name: edit-a
  namespace: `+namespace+`
  annotations:
    gatus.io/enabled: "true"
    gatus.io/group: Edit
spec:
  ports:
    - port: 80
---
apiVersion: v1
kind: Service
metadata:
  name: edit-b
  namespace: `+namespace+`
  annotations:
    gatus.io/enabled: "true"
    gatus.io/group: Edit
spec:
  ports:
    - port: 81
`)

	s := startSidecar(t, baseConfig)
	s.await("both endpoints at the original interval", func(eps map[string]map[string]any) bool {
		return eps["Edit/Edit a"]["interval"] == "1m" && eps["Edit/Edit b"]["interval"] == "1m"
	})

	// Change only the shared template. No workload is touched.
	apply(t, `
apiVersion: gatus.kalexlab.xyz/v1alpha1
kind: EndpointTemplate
metadata:
  name: e2e-common
spec:
  endpoint:
    interval: 47s
    alerts:
      - type: telegram
`)

	s.await("both endpoints to pick up the template edit", func(eps map[string]map[string]any) bool {
		return eps["Edit/Edit a"]["interval"] == "47s" && eps["Edit/Edit b"]["interval"] == "47s"
	})
}

func TestServiceDeletionRemovesTheEndpoint(t *testing.T) {
	apply(t, templates)

	manifest := `
apiVersion: v1
kind: Service
metadata:
  name: ephemeral
  namespace: ` + namespace + `
  annotations:
    gatus.io/enabled: "true"
    gatus.io/group: Ephemeral
spec:
  ports:
    - port: 80
`
	apply(t, manifest)

	s := startSidecar(t, baseConfig)
	s.await("the endpoint to appear", func(eps map[string]map[string]any) bool {
		_, ok := eps["Ephemeral/Ephemeral"]
		return ok
	})

	if out, err := kubectlStdin(manifest, "delete", "-f", "-", "--ignore-not-found"); err != nil {
		t.Fatalf("delete service: %v\n%s", err, out)
	}

	s.await("the endpoint to disappear", func(eps map[string]map[string]any) bool {
		_, ok := eps["Ephemeral/Ephemeral"]
		return !ok
	})
}

func TestAutoDiscoveryMode(t *testing.T) {
	apply(t, templates)
	apply(t, `
apiVersion: v1
kind: Service
metadata:
  name: auto-yes
  namespace: `+namespace+`
  annotations:
    gatus.io/group: Auto
spec:
  ports:
    - port: 80
---
apiVersion: v1
kind: Service
metadata:
  name: auto-no
  namespace: `+namespace+`
  annotations:
    gatus.io/group: Auto
    gatus.io/enabled: "false"
spec:
  ports:
    - port: 80
`)

	s := startSidecar(t, baseConfig, "--service-discovery=auto", "--ingressroute-discovery=disabled")
	eps := s.await("the unannotated service to be picked up", func(eps map[string]map[string]any) bool {
		_, ok := eps["Auto/Auto yes"]
		return ok
	})

	// Opting out is honoured even in auto mode.
	if _, ok := eps["Auto/Auto no"]; ok {
		t.Errorf("a Service with enabled=false was monitored: %v", keys(eps))
	}
}

func TestTemplateStatusIsReported(t *testing.T) {
	apply(t, templates)
	apply(t, `
apiVersion: gatus.kalexlab.xyz/v1alpha1
kind: EndpointTemplate
metadata:
  name: e2e-broken
spec:
  extends: [does-not-exist]
`)
	apply(t, `
apiVersion: v1
kind: Service
metadata:
  name: status-probe
  namespace: `+namespace+`
  annotations:
    gatus.io/enabled: "true"
    gatus.io/group: Status
spec:
  ports:
    - port: 80
`)

	s := startSidecar(t, baseConfig)
	s.await("the endpoint to render", func(eps map[string]map[string]any) bool {
		_, ok := eps["Status/Status probe"]
		return ok
	})

	// A template that resolves reports Ready and how many endpoints used it.
	awaitJSONPath(t, "endpointtemplate/e2e-http",
		`{.status.conditions[?(@.type=="Ready")].status}`, "True")
	awaitJSONPath(t, "endpointtemplate/e2e-http", `{.status.usedBy}`, "1")

	// A broken one is reported without stopping the rest of the config: the
	// endpoint above rendered regardless.
	awaitJSONPath(t, "endpointtemplate/e2e-broken",
		`{.status.conditions[?(@.type=="Ready")].status}`, "False")
	awaitJSONPath(t, "endpointtemplate/e2e-broken",
		`{.status.conditions[?(@.type=="Ready")].reason}`, "UnknownParent")
}

// awaitJSONPath polls a field until it holds the wanted value.
func awaitJSONPath(t *testing.T, resource, jsonpath, want string) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, err := kubectl(env.repoRoot, "get", resource, "-o", "jsonpath="+jsonpath)
		if err == nil {
			last = strings.TrimSpace(out)
			if last == want {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s %s = %q, want %q", resource, jsonpath, last, want)
}

// Helpers for running commands.

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// The test runs from test/e2e.
	return filepath.Abs(filepath.Join(wd, "..", ".."))
}

func run(dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func kubectl(dir string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "KUBECONFIG="+env.kubeconfig)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func kubectlStdin(stdin string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	cmd.Dir = env.repoRoot
	cmd.Env = append(os.Environ(), "KUBECONFIG="+env.kubeconfig)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func containsLine(haystack, want string) bool {
	for _, line := range strings.Split(haystack, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
