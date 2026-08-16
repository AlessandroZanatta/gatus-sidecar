package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	gatusv1alpha1 "github.com/AlessandroZanatta/gatus-sidecar/api/v1alpha1"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/registry"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := gatusv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add gatus scheme: %v", err)
	}
	return s
}

func template(t *testing.T, name, scheme string, defaultFor []string, extends []string, endpointYAML string) *gatusv1alpha1.EndpointTemplate {
	t.Helper()
	out := &gatusv1alpha1.EndpointTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gatusv1alpha1.EndpointTemplateSpec{
			Scheme: scheme, DefaultFor: defaultFor, Extends: extends,
		},
	}
	if endpointYAML != "" {
		raw, err := yaml.YAMLToJSON([]byte(endpointYAML))
		if err != nil {
			t.Fatalf("convert %s: %v", name, err)
		}
		out.Spec.Endpoint = &apiextensionsv1.JSON{Raw: raw}
	}
	return out
}

// loopFixture assembles a RenderLoop over a temp directory and a fake client.
type loopFixture struct {
	loop   *RenderLoop
	reg    *registry.Registry
	client client.Client
	output string
}

func newLoopFixture(t *testing.T, baseYAML string, objs ...client.Object) *loopFixture {
	t.Helper()
	dir := t.TempDir()

	basePath := ""
	if baseYAML != "" {
		basePath = filepath.Join(dir, "base.yaml")
		if err := os.WriteFile(basePath, []byte(baseYAML), 0o644); err != nil {
			t.Fatalf("write base: %v", err)
		}
	}

	output := filepath.Join(dir, "out", "config.yaml")
	reg := registry.New()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()

	return &loopFixture{
		loop: &RenderLoop{
			Client:         c,
			Registry:       reg,
			Renderer:       config.NewRenderer(config.RenderOptions{}),
			Writer:         config.NewWriter(output),
			BaseConfigPath: basePath,
			Debounce:       10 * time.Millisecond,
		},
		reg:    reg,
		client: c,
		output: output,
	}
}

// read returns the rendered config, parsed.
func (f *loopFixture) read(t *testing.T) config.Object {
	t.Helper()
	raw, err := os.ReadFile(f.output)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var out config.Object
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse output: %v", err)
	}
	return out
}

// endpoints returns the rendered endpoints list.
func (f *loopFixture) endpoints(t *testing.T) []any {
	t.Helper()
	doc := f.read(t)
	list, ok := doc["endpoints"].([]any)
	if !ok {
		t.Fatalf("endpoints = %#v, want a list", doc["endpoints"])
	}
	return list
}

// endpointCount is the polling-safe form of endpoints: it reports -1 while the
// file does not exist yet, so it can be called from a waitFor condition before
// the first render has landed.
func (f *loopFixture) endpointCount() int {
	raw, err := os.ReadFile(f.output)
	if err != nil {
		return -1
	}
	var doc config.Object
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return -1
	}
	list, ok := doc["endpoints"].([]any)
	if !ok {
		return -1
	}
	return len(list)
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func endpoint(name, group, host string, port int32) config.Endpoint {
	return config.Endpoint{
		Source: config.SourceService, SourceRef: "ns/" + strings.ToLower(name),
		Name: name, Group: group, Host: host, Port: port,
	}
}

func TestRenderLoopWritesOnStartupWithNoEndpoints(t *testing.T) {
	// Gatus refuses to start without a config file, so one must exist even in a
	// cluster where nothing has opted in yet.
	f := newLoopFixture(t, "ui:\n  default-sort-by: group\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.loop.Start(ctx)

	waitFor(t, "initial write", func() bool {
		_, err := os.Stat(f.output)
		return err == nil
	})

	doc := f.read(t)
	if doc["ui"] == nil {
		t.Errorf("doc = %#v, want the base config preserved", doc)
	}
	if list, ok := doc["endpoints"].([]any); !ok || len(list) != 0 {
		t.Errorf("endpoints = %#v, want an empty list", doc["endpoints"])
	}
	if !f.loop.Ready() {
		t.Error("Ready() = false after a successful render")
	}
}

func TestRenderLoopRendersRegisteredEndpoints(t *testing.T) {
	f := newLoopFixture(t, "ui:\n  default-sort-by: group\n",
		template(t, "common", "", nil, nil, "interval: 1m"),
		template(t, "default-http", "http", []string{"http"}, []string{"common"}, `conditions: ["[STATUS] == 200"]`),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.loop.Start(ctx)

	waitFor(t, "initial write", func() bool { _, err := os.Stat(f.output); return err == nil })

	f.reg.Set(registry.Key{Kind: "Service", Namespace: "storefront", Name: "web"},
		[]config.Endpoint{endpoint("Web", "Storefront", "web.storefront.svc.cluster.local", 8096)})

	waitFor(t, "endpoint to appear", func() bool { return f.endpointCount() == 1 })

	ep := f.endpoints(t)[0].(map[string]any)
	if ep["url"] != "http://web.storefront.svc.cluster.local:8096" {
		t.Errorf("url = %v", ep["url"])
	}
	// The defaultFor template applied, including its extends chain.
	if ep["interval"] != "1m" || ep["conditions"] == nil {
		t.Errorf("endpoint = %#v, want the template chain applied", ep)
	}
}

func TestRenderLoopReactsToTemplateChanges(t *testing.T) {
	// The core claim of the design: editing a template re-renders every endpoint
	// using it, with no reverse index from templates back to workloads.
	tpl := template(t, "default-http", "http", []string{"http"}, nil, "interval: 1m")
	f := newLoopFixture(t, "", tpl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.loop.Start(ctx)

	f.reg.Set(registry.Key{Kind: "Service", Namespace: "storefront", Name: "web"},
		[]config.Endpoint{endpoint("Web", "Storefront", "web.storefront.svc.cluster.local", 8096)})

	waitFor(t, "first render", func() bool {
		return f.endpointCount() == 1 && f.endpoints(t)[0].(map[string]any)["interval"] == "1m"
	})

	// Edit the template only; the registry entry is untouched.
	var live gatusv1alpha1.EndpointTemplate
	if err := f.client.Get(ctx, client.ObjectKey{Name: "default-http"}, &live); err != nil {
		t.Fatalf("get template: %v", err)
	}
	raw, err := yaml.YAMLToJSON([]byte("interval: 30s"))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	live.Spec.Endpoint = &apiextensionsv1.JSON{Raw: raw}
	if err := f.client.Update(ctx, &live); err != nil {
		t.Fatalf("update template: %v", err)
	}

	// This is what the EndpointTemplate controller does on a watch event.
	f.reg.Touch()

	waitFor(t, "the template edit to take effect", func() bool {
		return f.endpointCount() == 1 && f.endpoints(t)[0].(map[string]any)["interval"] == "30s"
	})
}

func TestRenderLoopRemovesDeletedEndpoints(t *testing.T) {
	f := newLoopFixture(t, "")
	key := registry.Key{Kind: "Service", Namespace: "storefront", Name: "web"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.loop.Start(ctx)

	f.reg.Set(key, []config.Endpoint{endpoint("Web", "Storefront", "web.storefront.svc.cluster.local", 8096)})
	waitFor(t, "endpoint to appear", func() bool { return f.endpointCount() == 1 })

	f.reg.Delete(key)
	waitFor(t, "endpoint to disappear", func() bool { return f.endpointCount() == 0 })
}

func TestRenderLoopDebouncesBursts(t *testing.T) {
	// A rollout touches many Services at once. Gatus restarts every check's
	// interval on reload, so the burst must produce one write, not twenty.
	f := newLoopFixture(t, "")
	f.loop.Debounce = 150 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.loop.Start(ctx)

	waitFor(t, "initial write", func() bool { _, err := os.Stat(f.output); return err == nil })

	before := modTime(t, f.output)
	for i := 0; i < 20; i++ {
		name := string(rune('a' + i))
		f.reg.Set(registry.Key{Kind: "Service", Namespace: "ns", Name: name},
			[]config.Endpoint{endpoint(strings.ToUpper(name), "G", name+".ns.svc.cluster.local", 80)})
		time.Sleep(2 * time.Millisecond)
	}

	// Nothing should have been written while the burst was still arriving.
	if modTime(t, f.output) != before {
		t.Error("config was written mid-burst; the debounce window did not hold")
	}

	waitFor(t, "the debounced write", func() bool { return f.endpointCount() == 20 })
}

func TestRenderLoopSkipsWritingUnchangedOutput(t *testing.T) {
	f := newLoopFixture(t, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.loop.Start(ctx)

	f.reg.Set(registry.Key{Kind: "Service", Namespace: "ns", Name: "x"},
		[]config.Endpoint{endpoint("X", "G", "x.ns.svc.cluster.local", 80)})
	waitFor(t, "first render", func() bool { return f.endpointCount() == 1 })

	before := modTime(t, f.output)
	time.Sleep(20 * time.Millisecond)

	// Touch forces a render, but the output is identical, so nothing is written.
	f.reg.Touch()
	time.Sleep(200 * time.Millisecond)

	if modTime(t, f.output) != before {
		t.Error("file rewritten despite identical content; Gatus would reload for nothing")
	}
}

func TestRenderLoopKeepsPreviousConfigWhenBaseBreaks(t *testing.T) {
	// A stale config still monitors things; a missing one does not.
	f := newLoopFixture(t, "ui:\n  default-sort-by: group\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.loop.Start(ctx)

	f.reg.Set(registry.Key{Kind: "Service", Namespace: "ns", Name: "x"},
		[]config.Endpoint{endpoint("X", "G", "x.ns.svc.cluster.local", 80)})
	waitFor(t, "first render", func() bool { return f.endpointCount() == 1 })

	if err := os.WriteFile(f.loop.BaseConfigPath, []byte("key: [unclosed\n"), 0o644); err != nil {
		t.Fatalf("corrupt base: %v", err)
	}
	f.reg.Touch()
	time.Sleep(200 * time.Millisecond)

	doc := f.read(t)
	if doc["ui"] == nil {
		t.Errorf("doc = %#v, want the last good configuration left in place", doc)
	}
	if len(f.endpoints(t)) != 1 {
		t.Error("endpoints lost after a failed render")
	}
}

func TestRenderLoopStopsOnContextCancel(t *testing.T) {
	f := newLoopFixture(t, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.loop.Start(ctx) }()

	waitFor(t, "initial write", func() bool { _, err := os.Stat(f.output); return err == nil })
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start() = %v, want nil on cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after the context was cancelled")
	}
}

func TestRenderLoopNeedsNoLeaderElection(t *testing.T) {
	// Every replica writes its own local file, so a standby would just leave
	// Gatus without a config.
	if (&RenderLoop{}).NeedLeaderElection() {
		t.Error("NeedLeaderElection() = true, want false")
	}
}

func modTime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.ModTime()
}
