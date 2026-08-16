package discovery

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
)

// route builds an IngressRoute from YAML, the way an operator writes one.
func route(t *testing.T, manifest string) *unstructured.Unstructured {
	t.Helper()
	raw, err := yaml.YAMLToJSON([]byte(manifest))
	if err != nil {
		t.Fatalf("convert manifest: %v", err)
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(raw); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return obj
}

// staticResolver answers backend lookups from a fixed table.
func staticResolver(ports map[string][]NamedPort) ServiceResolver {
	return func(ns, name string) ([]NamedPort, error) {
		key := ns + "/" + name
		p, ok := ports[key]
		if !ok {
			return nil, fmt.Errorf("service %s not found", key)
		}
		return p, nil
	}
}

// byName indexes endpoints for assertions.
func byName(endpoints []config.Endpoint) map[string]config.Endpoint {
	out := make(map[string]config.Endpoint, len(endpoints))
	for _, e := range endpoints {
		out[e.Name] = e
	}
	return out
}

func TestFromIngressRouteExclude(t *testing.T) {
	manifest := func(name, exclude string) string {
		return `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: ` + name + `
  namespace: shop
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
    gatus.kalexlab.xyz/exclude: "` + exclude + `"
spec:
  routes:
    - match: Host(` + "`shop.example.com`" + `)
      kind: Rule
      services:
        - name: shop
          port: 2283
`
	}

	tests := []struct {
		name      string
		routeName string
		exclude   string
		want      bool // want the route monitored
	}{
		{"names another route", "shop", "shop-admin", true},
		{"names this route", "shop-admin", "shop-admin", false},
		{"glob", "shop-admin", "*-admin", false},
		{"bare star", "shop", "*", false},
	}

	resolver := staticResolver(map[string][]NamedPort{"shop/shop": {{Name: "http", Port: 2283}}})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Defaults().FromIngressRoute(route(t, manifest(tc.routeName, tc.exclude)), "", resolver)
			if err != nil {
				t.Fatalf("FromIngressRoute: %v", err)
			}
			if monitored := len(got) > 0; monitored != tc.want {
				t.Errorf("monitored = %v, want %v", monitored, tc.want)
			}
		})
	}
}

func TestFromIngressRouteEmitsInternalAndExternal(t *testing.T) {
	ir := route(t, `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: shop
  namespace: shop
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  routes:
    - match: Host(`+"`shop.example.com`"+`)
      kind: Rule
      services:
        - name: shop
          port: 2283
`)

	got, err := Defaults().FromIngressRoute(ir, "", staticResolver(map[string][]NamedPort{
		"shop/shop": {{Name: "http", Port: 2283}},
	}))
	if err != nil {
		t.Fatalf("FromIngressRoute: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d endpoints, want 2 (internal and external): %#v", len(got), got)
	}

	eps := byName(got)

	external, ok := eps["Shop (external)"]
	if !ok {
		t.Fatalf("no external endpoint in %v", names(got))
	}
	if external.URL != "https://shop.example.com" {
		t.Errorf("external URL = %q, want https://shop.example.com", external.URL)
	}
	if external.Scheme != "https" {
		t.Errorf("external Scheme = %q, want https", external.Scheme)
	}

	internal, ok := eps["Shop"]
	if !ok {
		t.Fatalf("no internal endpoint in %v", names(got))
	}
	if internal.Host != "shop.shop.svc.cluster.local" || internal.Port != 2283 {
		t.Errorf("internal = %s:%d, want shop.shop.svc.cluster.local:2283", internal.Host, internal.Port)
	}
	// The internal one has no URL yet: the scheme comes from a template.
	if internal.URL != "" {
		t.Errorf("internal URL = %q, want it left to the renderer", internal.URL)
	}

	for _, ep := range got {
		if ep.Source != config.SourceIngressRoute {
			t.Errorf("%s Source = %s, want IngressRoute", ep.Name, ep.Source)
		}
		if ep.Group != "Shop" {
			t.Errorf("%s Group = %q, want Shop", ep.Name, ep.Group)
		}
	}
}

func TestFromIngressRouteAppliesPathPrefix(t *testing.T) {
	ir := route(t, `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: api
  namespace: storefront
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  routes:
    - match: Host(`+"`shop.example.com`"+`) && PathPrefix(`+"`/api`"+`)
      services:
        - name: api
          port: 3000
`)

	got, err := Defaults().FromIngressRoute(ir, "", staticResolver(map[string][]NamedPort{
		"storefront/api": {{Port: 3000}},
	}))
	if err != nil {
		t.Fatalf("FromIngressRoute: %v", err)
	}

	eps := byName(got)
	if want := "https://shop.example.com/api"; eps["Api (external)"].URL != want {
		t.Errorf("external URL = %q, want %q", eps["Api (external)"].URL, want)
	}
	// The path applies to the internal endpoint too: it is the same application.
	if eps["Api"].Path != "/api" {
		t.Errorf("internal Path = %q, want /api", eps["Api"].Path)
	}
}

func TestFromIngressRouteOptInModes(t *testing.T) {
	manifest := `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: x
  namespace: ns
%s
spec:
  routes:
    - match: Host(` + "`x.example.org`" + `)
      services:
        - name: x
          port: 80
`
	withAnn := func(v string) string {
		if v == "" {
			return fmt.Sprintf(manifest, "")
		}
		return fmt.Sprintf(manifest, "  annotations:\n    gatus.kalexlab.xyz/enabled: \""+v+"\"")
	}

	tests := []struct {
		name    string
		mode    Mode
		enabled string
		want    bool
	}{
		{"opt-in without annotation", ModeOptIn, "", false},
		{"opt-in with true", ModeOptIn, "true", true},
		{"auto without annotation", ModeAuto, "", true},
		{"auto with false", ModeAuto, "false", false},
		{"disabled ignores true", ModeDisabled, "true", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := Defaults()
			o.IngressRouteMode = tc.mode

			got, err := o.FromIngressRoute(route(t, withAnn(tc.enabled)), "",
				staticResolver(map[string][]NamedPort{"ns/x": {{Port: 80}}}))
			if err != nil {
				t.Fatalf("FromIngressRoute: %v", err)
			}
			if enabled := len(got) > 0; enabled != tc.want {
				t.Errorf("enabled = %v, want %v", enabled, tc.want)
			}
		})
	}
}

func TestFromIngressRouteMultipleHosts(t *testing.T) {
	ir := route(t, `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: web
  namespace: ns
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  routes:
    - match: Host(`+"`a.example.org`"+`, `+"`b.example.org`"+`)
      services:
        - name: web
          port: 80
`)

	got, err := Defaults().FromIngressRoute(ir, "", staticResolver(map[string][]NamedPort{
		"ns/web": {{Port: 80}},
	}))
	if err != nil {
		t.Fatalf("FromIngressRoute: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d endpoints, want 2 external and 1 internal: %v", len(got), names(got))
	}

	// The host is folded into the name so the two externals do not collide.
	eps := byName(got)
	for _, want := range []string{"Web a.example.org (external)", "Web b.example.org (external)", "Web"} {
		if _, ok := eps[want]; !ok {
			t.Errorf("missing endpoint %q; got %v", want, names(got))
		}
	}
}

func TestFromIngressRouteMultipleBackends(t *testing.T) {
	ir := route(t, `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: split
  namespace: ns
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  routes:
    - match: Host(`+"`x.example.org`"+`) && PathPrefix(`+"`/api`"+`)
      services:
        - name: api
          port: 3000
    - match: Host(`+"`x.example.org`"+`)
      services:
        - name: web
          port: 80
`)

	got, err := Defaults().FromIngressRoute(ir, "", staticResolver(map[string][]NamedPort{
		"ns/api": {{Port: 3000}},
		"ns/web": {{Port: 80}},
	}))
	if err != nil {
		t.Fatalf("FromIngressRoute: %v", err)
	}

	eps := byName(got)
	// Each backend is named after itself, since one name cannot cover both.
	for _, want := range []string{"Api", "Web"} {
		if _, ok := eps[want]; !ok {
			t.Errorf("missing internal endpoint %q; got %v", want, names(got))
		}
	}
	if eps["Api"].Port != 3000 || eps["Web"].Port != 80 {
		t.Errorf("ports = %d and %d, want 3000 and 80", eps["Api"].Port, eps["Web"].Port)
	}
}

func TestFromIngressRouteNamedBackendPort(t *testing.T) {
	ir := route(t, `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: web
  namespace: ns
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  routes:
    - match: Host(`+"`x.example.org`"+`)
      services:
        - name: web
          port: http
`)

	got, err := Defaults().FromIngressRoute(ir, "", staticResolver(map[string][]NamedPort{
		"ns/web": {{Name: "metrics", Port: 9090}, {Name: "http", Port: 8080}},
	}))
	if err != nil {
		t.Fatalf("FromIngressRoute: %v", err)
	}
	if got := byName(got)["Web"].Port; got != 8080 {
		t.Errorf("Port = %d, want 8080 resolved from the port name", got)
	}
}

func TestFromIngressRouteCrossNamespaceBackend(t *testing.T) {
	ir := route(t, `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: web
  namespace: routes
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  routes:
    - match: Host(`+"`x.example.org`"+`)
      services:
        - name: web
          namespace: apps
          port: 80
`)

	got, err := Defaults().FromIngressRoute(ir, "", staticResolver(map[string][]NamedPort{
		"apps/web": {{Port: 80}},
	}))
	if err != nil {
		t.Fatalf("FromIngressRoute: %v", err)
	}
	if want := "web.apps.svc.cluster.local"; byName(got)["Web"].Host != want {
		t.Errorf("Host = %q, want %q", byName(got)["Web"].Host, want)
	}
}

func TestFromIngressRouteAnnotationOverrides(t *testing.T) {
	ir := route(t, `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: portal
  namespace: platform
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
    gatus.kalexlab.xyz/name: Portal
    gatus.kalexlab.xyz/group: Platform
    gatus.kalexlab.xyz/template: strict
    gatus.kalexlab.xyz/endpoint: |
      conditions:
        - "[STATUS] > 400"
spec:
  routes:
    - match: Host(`+"`portal.example.com`"+`)
      services:
        - name: portal
          port: 80
`)

	got, err := Defaults().FromIngressRoute(ir, "ignored", staticResolver(map[string][]NamedPort{
		"platform/portal": {{Port: 80}},
	}))
	if err != nil {
		t.Fatalf("FromIngressRoute: %v", err)
	}

	eps := byName(got)
	if _, ok := eps["Portal"]; !ok {
		t.Fatalf("internal endpoint not named from the annotation: %v", names(got))
	}
	if _, ok := eps["Portal (external)"]; !ok {
		t.Fatalf("external endpoint not named from the annotation: %v", names(got))
	}
	for _, ep := range got {
		if ep.Group != "Platform" {
			t.Errorf("%s Group = %q, want Platform", ep.Name, ep.Group)
		}
		if len(ep.Templates) != 1 || ep.Templates[0] != "strict" {
			t.Errorf("%s Templates = %v, want [strict]", ep.Name, ep.Templates)
		}
		if ep.Patch["conditions"] == nil {
			t.Errorf("%s lost the endpoint patch", ep.Name)
		}
	}
}

func TestFromIngressRouteExplicitURLSkipsDerivation(t *testing.T) {
	// The author has said exactly what to check, so the route is not consulted.
	ir := route(t, `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: web
  namespace: ns
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
    gatus.kalexlab.xyz/url: https://status.example.org/health
spec:
  routes:
    - match: Host(`+"`x.example.org`"+`)
      services:
        - name: web
          port: 80
`)

	got, err := Defaults().FromIngressRoute(ir, "", nil)
	if err != nil {
		t.Fatalf("FromIngressRoute: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d endpoints, want 1: %v", len(got), names(got))
	}
	if got[0].URL != "https://status.example.org/health" {
		t.Errorf("URL = %q, want the annotated one", got[0].URL)
	}
}

func TestFromIngressRouteEndpointsListSkipsDerivation(t *testing.T) {
	ir := route(t, `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: web
  namespace: ns
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
    gatus.kalexlab.xyz/endpoints: |
      - name: Only this
        url: https://one.example.org
spec:
  routes:
    - match: Host(`+"`x.example.org`"+`)
      services:
        - name: web
          port: 80
`)

	got, err := Defaults().FromIngressRoute(ir, "", nil)
	if err != nil {
		t.Fatalf("FromIngressRoute: %v", err)
	}
	if len(got) != 1 || got[0].Name != "Only this" {
		t.Fatalf("got %v, want a single explicitly listed endpoint", names(got))
	}
}

func TestFromIngressRouteNoHostYieldsOnlyInternal(t *testing.T) {
	// A path-only route has no external address to check.
	ir := route(t, `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: web
  namespace: ns
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  routes:
    - match: PathPrefix(`+"`/api`"+`)
      services:
        - name: web
          port: 80
`)

	got, err := Defaults().FromIngressRoute(ir, "", staticResolver(map[string][]NamedPort{
		"ns/web": {{Port: 80}},
	}))
	if err != nil {
		t.Fatalf("FromIngressRoute: %v", err)
	}
	if len(got) != 1 || got[0].URL != "" {
		t.Fatalf("got %v, want only the internal endpoint", names(got))
	}
}

func TestFromIngressRouteNegatedHostIsNotChecked(t *testing.T) {
	ir := route(t, `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: web
  namespace: ns
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  routes:
    - match: Host(`+"`good.example.org`"+`) && !Host(`+"`bad.example.org`"+`)
      services:
        - name: web
          port: 80
`)

	got, err := Defaults().FromIngressRoute(ir, "", staticResolver(map[string][]NamedPort{
		"ns/web": {{Port: 80}},
	}))
	if err != nil {
		t.Fatalf("FromIngressRoute: %v", err)
	}
	for _, ep := range got {
		if strings.Contains(ep.URL, "bad.example.org") {
			t.Errorf("endpoint %q checks an excluded host: %s", ep.Name, ep.URL)
		}
	}
	if _, ok := byName(got)["Web (external)"]; !ok {
		t.Errorf("the permitted host was not checked: %v", names(got))
	}
}

func TestFromIngressRouteErrors(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		resolver ServiceResolver
		wantErr  string
	}{
		{
			name: "unparseable match rule",
			manifest: `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: web
  namespace: ns
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  routes:
    - match: NotAMatcher(` + "`x`" + `)
      services:
        - name: web
          port: 80
`,
			resolver: staticResolver(map[string][]NamedPort{"ns/web": {{Port: 80}}}),
			wantErr:  "parse match rule",
		},
		{
			name: "backend service does not exist",
			manifest: `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: web
  namespace: ns
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  routes:
    - match: Host(` + "`x.example.org`" + `)
      services:
        - name: missing
`,
			resolver: staticResolver(nil),
			wantErr:  "resolve backend service",
		},
		{
			name: "backend port cannot be inferred",
			manifest: `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: web
  namespace: ns
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  routes:
    - match: Host(` + "`x.example.org`" + `)
      services:
        - name: web
`,
			resolver: staticResolver(map[string][]NamedPort{"ns/web": {{Name: "a", Port: 1}, {Name: "b", Port: 2}}}),
			wantErr:  "pick one with the port annotation",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Defaults().FromIngressRoute(route(t, tc.manifest), "", tc.resolver)
			if err == nil {
				t.Fatal("FromIngressRoute() = nil error, want a failure")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestFromIngressRouteNoRoutes(t *testing.T) {
	ir := route(t, `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: web
  namespace: ns
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec: {}
`)

	got, err := Defaults().FromIngressRoute(ir, "", nil)
	if err != nil {
		t.Fatalf("FromIngressRoute: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want nothing from a route with no rules", names(got))
	}
}

func TestFromIngressRouteCustomExternalSuffix(t *testing.T) {
	o := Defaults()
	o.ExternalSuffix = " [public]"

	ir := route(t, `
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: web
  namespace: ns
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  routes:
    - match: Host(`+"`x.example.org`"+`)
      services:
        - name: web
          port: 80
`)

	got, err := o.FromIngressRoute(ir, "", staticResolver(map[string][]NamedPort{"ns/web": {{Port: 80}}}))
	if err != nil {
		t.Fatalf("FromIngressRoute: %v", err)
	}
	if _, ok := byName(got)["Web [public]"]; !ok {
		t.Errorf("got %v, want the custom suffix applied", names(got))
	}
}

func names(endpoints []config.Endpoint) []string {
	out := make([]string, len(endpoints))
	for i, e := range endpoints {
		out[i] = e.Name
	}
	return out
}
