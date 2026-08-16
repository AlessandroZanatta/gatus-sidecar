package discovery

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
)

func svc(ns, name string, annotations map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: annotations},
		Spec:       corev1.ServiceSpec{Ports: ports},
	}
}

func port(name string, n int32) corev1.ServicePort {
	return corev1.ServicePort{Name: name, Port: n}
}

func ann(pairs ...string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(pairs); i += 2 {
		out[AnnotationPrefix+pairs[i]] = pairs[i+1]
	}
	return out
}

func TestFromServiceDerivesClusterLocalURL(t *testing.T) {
	o := Defaults()

	got, err := o.FromService(svc("shop", "worker-queue", ann("enabled", "true"), port("http", 3003)), "")
	if err != nil {
		t.Fatalf("FromService: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(got))
	}

	ep := got[0]
	if want := "worker-queue.shop.svc.cluster.local"; ep.Host != want {
		t.Errorf("Host = %q, want %q", ep.Host, want)
	}
	if ep.Port != 3003 {
		t.Errorf("Port = %d, want 3003", ep.Port)
	}
	if ep.Path != "" {
		t.Errorf("Path = %q, want empty", ep.Path)
	}
	if ep.URL != "" {
		t.Errorf("URL = %q, want empty; assembly is the renderer's job", ep.URL)
	}
	if want := "Worker queue"; ep.Name != want {
		t.Errorf("Name = %q, want %q", ep.Name, want)
	}
	if want := "Shop"; ep.Group != want {
		t.Errorf("Group = %q, want %q", ep.Group, want)
	}
	// An unset scheme means "let the templates decide" rather than "http".
	if ep.Scheme != "" {
		t.Errorf("Scheme = %q, want empty", ep.Scheme)
	}
	if ep.Source != config.SourceService || ep.SourceRef != "shop/worker-queue" {
		t.Errorf("source = %s %s, want Service shop/worker-queue", ep.Source, ep.SourceRef)
	}
}

func TestFromServiceDiscoveryModes(t *testing.T) {
	tests := []struct {
		name    string
		mode    Mode
		enabled string // "" means the annotation is absent
		want    bool
	}{
		{"opt-in without annotation", ModeOptIn, "", false},
		{"opt-in with true", ModeOptIn, "true", true},
		{"opt-in with false", ModeOptIn, "false", false},
		{"opt-in with garbage", ModeOptIn, "maybe", false},
		{"auto without annotation", ModeAuto, "", true},
		{"auto with false", ModeAuto, "false", false},
		{"auto with true", ModeAuto, "true", true},
		{"auto with garbage stays on", ModeAuto, "maybe", true},
		{"disabled ignores true", ModeDisabled, "true", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := Defaults()
			o.ServiceMode = tc.mode

			annotations := map[string]string{}
			if tc.enabled != "" {
				annotations = ann("enabled", tc.enabled)
			}

			got, err := o.FromService(svc("storefront", "web", annotations, port("http", 8096)), "")
			if err != nil {
				t.Fatalf("FromService: %v", err)
			}
			if gotEnabled := len(got) > 0; gotEnabled != tc.want {
				t.Errorf("enabled = %v, want %v", gotEnabled, tc.want)
			}
		})
	}
}

func TestFromServiceExclude(t *testing.T) {
	tests := []struct {
		name    string
		svcName string
		exclude string
		want    bool // want the service monitored
	}{
		{"no exclude annotation", "db-primary", "", true},
		{"names another service", "db-primary", "db-replicas", true},
		{"names this service", "db-replicas", "db-replicas", false},
		{"one entry of a list", "db-replicas", "db-headless,db-replicas,db-extra", false},
		{"no entry of a list", "db-primary", "db-headless,db-replicas,db-extra", true},
		{"glob suffix", "db-headless", "*-headless", false},
		{"glob suffix misses", "db", "*-headless", true},
		{"bare star excludes everything", "db-primary", "*", false},
		{"single-character wildcard", "db-1", "db-?", false},
		{"character class", "db-ro", "db-[rn]o", false},
		{"whitespace and blanks are ignored", "db-replicas", " , db-replicas , ", false},
		{"newline separated", "db-replicas", "db-headless\ndb-replicas\n", false},
		{"unparseable pattern matches nothing", "db-replicas", "db-[replicas", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := Defaults()

			annotations := ann("enabled", "true")
			if tc.exclude != "" {
				annotations = ann("enabled", "true", "exclude", tc.exclude)
			}

			got, err := o.FromService(svc("storefront", tc.svcName, annotations, port("postgres", 5432)), "")
			if err != nil {
				t.Fatalf("FromService: %v", err)
			}
			if monitored := len(got) > 0; monitored != tc.want {
				t.Errorf("monitored = %v, want %v", monitored, tc.want)
			}
		})
	}
}

// The exclude annotation is propagated to a whole family of Services at once, so
// one shared value has to suppress the members it names and leave the rest alone.
func TestFromServiceExcludeSharedAcrossServices(t *testing.T) {
	o := Defaults()
	inherited := ann("enabled", "true", "scheme", "tcp", "exclude", "db-replicas")

	var monitored []string
	for _, name := range []string{"db-primary", "db-replicas", "db-all"} {
		got, err := o.FromService(svc("storefront", name, inherited, port("postgres", 5432)), "")
		if err != nil {
			t.Fatalf("FromService(%s): %v", name, err)
		}
		if len(got) > 0 {
			monitored = append(monitored, name)
		}
	}

	want := []string{"db-primary", "db-all"}
	if !reflect.DeepEqual(monitored, want) {
		t.Errorf("monitored = %v, want %v", monitored, want)
	}
}

// exclude is a control annotation: it configures discovery and must not reach
// the rendered Gatus endpoint.
func TestFromServiceExcludeDoesNotLeakIntoPatch(t *testing.T) {
	o := Defaults()

	got, err := o.FromService(svc("storefront", "db-primary", ann("enabled", "true", "exclude", "db-replicas"), port("postgres", 5432)), "")
	if err != nil {
		t.Fatalf("FromService: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(got))
	}
	if _, ok := got[0].Patch["exclude"]; ok {
		t.Errorf("exclude leaked into the endpoint patch: %v", got[0].Patch)
	}
}

func TestFromServicePortSelection(t *testing.T) {
	tests := []struct {
		name     string
		portAnn  string
		ports    []corev1.ServicePort
		wantPort int32
		wantErr  string
	}{
		{
			name:     "single port needs no annotation",
			ports:    []corev1.ServicePort{port("http", 8096)},
			wantPort: 8096,
		},
		{
			name:     "numeric annotation",
			portAnn:  "9001",
			ports:    []corev1.ServicePort{port("api", 9000), port("ui", 9001)},
			wantPort: 9001,
		},
		{
			name:     "named annotation",
			portAnn:  "ui",
			ports:    []corev1.ServicePort{port("api", 9000), port("ui", 9001)},
			wantPort: 9001,
		},
		{
			name:    "multi-port without annotation is an error",
			ports:   []corev1.ServicePort{port("api", 9000), port("ui", 9001)},
			wantErr: "exposes 2 ports",
		},
		{
			name:    "unknown port name",
			portAnn: "nope",
			ports:   []corev1.ServicePort{port("api", 9000)},
			wantErr: `no port named "nope"`,
		},
		{
			name:    "no ports at all",
			ports:   nil,
			wantErr: "exposes no ports",
		},
		{
			name:    "out of range port",
			portAnn: "70000",
			ports:   []corev1.ServicePort{port("api", 9000)},
			wantErr: "out of range",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			annotations := ann("enabled", "true")
			if tc.portAnn != "" {
				annotations[AnnotationPrefix+AnnPort] = tc.portAnn
			}

			got, err := Defaults().FromService(svc("storefront", "x", annotations, tc.ports...), "")
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("FromService() = %v, want error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromService: %v", err)
			}
			if got[0].Port != tc.wantPort {
				t.Errorf("Port = %d, want %d", got[0].Port, tc.wantPort)
			}
		})
	}
}

func TestFromServiceSchemeAndPath(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantScheme  string
		wantPath    string
		wantURL     string
	}{
		{
			name:        "tcp scheme",
			annotations: ann("enabled", "true", "scheme", "tcp"),
			wantScheme:  "tcp",
		},
		{
			name:        "scheme is lowercased",
			annotations: ann("enabled", "true", "scheme", "TCP"),
			wantScheme:  "tcp",
		},
		{
			name:        "path with leading slash",
			annotations: ann("enabled", "true", "path", "/api/health"),
			wantPath:    "/api/health",
		},
		{
			name:        "path without leading slash is normalised",
			annotations: ann("enabled", "true", "path", "api/health"),
			wantPath:    "/api/health",
		},
		{
			name:        "bare slash path adds nothing",
			annotations: ann("enabled", "true", "path", "/"),
			wantPath:    "",
		},
		{
			name:        "explicit url wins and sets the scheme",
			annotations: ann("enabled", "true", "url", "https://example.org"),
			wantURL:     "https://example.org",
			wantScheme:  "https",
		},
		{
			name:        "explicit scheme is not overridden by the url",
			annotations: ann("enabled", "true", "scheme", "tcp", "url", "https://example.org"),
			wantURL:     "https://example.org",
			wantScheme:  "tcp",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Defaults().FromService(svc("storefront", "x", tc.annotations, port("p", 5432)), "")
			if err != nil {
				t.Fatalf("FromService: %v", err)
			}
			if got[0].Scheme != tc.wantScheme {
				t.Errorf("Scheme = %q, want %q", got[0].Scheme, tc.wantScheme)
			}
			if got[0].Path != tc.wantPath {
				t.Errorf("Path = %q, want %q", got[0].Path, tc.wantPath)
			}
			if got[0].URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", got[0].URL, tc.wantURL)
			}
		})
	}
}

func TestFromServiceExplicitURLSkipsPortResolution(t *testing.T) {
	// A multi-port Service is an error only when a URL has to be derived.
	got, err := Defaults().FromService(
		svc("ns", "x", ann("enabled", "true", "url", "https://example.org"), port("a", 1), port("b", 2)), "")
	if err != nil {
		t.Fatalf("FromService: %v", err)
	}
	if got[0].URL != "https://example.org" || got[0].Port != 0 {
		t.Errorf("got URL %q port %d, want the explicit URL and no port", got[0].URL, got[0].Port)
	}
}

func TestFromServiceGroupPrecedence(t *testing.T) {
	tests := []struct {
		name               string
		annotations        map[string]string
		nsGroup            string
		groupFromNamespace bool
		want               string
	}{
		{
			name:               "object annotation wins",
			annotations:        ann("enabled", "true", "group", "Online Shop"),
			nsGroup:            "From namespace",
			groupFromNamespace: true,
			want:               "Online Shop",
		},
		{
			name:               "namespace annotation is inherited",
			annotations:        ann("enabled", "true"),
			nsGroup:            "Online Shop",
			groupFromNamespace: true,
			want:               "Online Shop",
		},
		{
			name:               "falls back to the namespace name",
			annotations:        ann("enabled", "true"),
			groupFromNamespace: true,
			want:               "Storefront",
		},
		{
			// Several endpoints in the source config sit at the top level with
			// no group, so an empty annotation has to mean "none", not "unset".
			name:               "empty annotation means no group",
			annotations:        ann("enabled", "true", "group", ""),
			nsGroup:            "Online Shop",
			groupFromNamespace: true,
			want:               "",
		},
		{
			name:               "namespace derivation can be turned off",
			annotations:        ann("enabled", "true"),
			groupFromNamespace: false,
			want:               "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := Defaults()
			o.GroupFromNamespace = tc.groupFromNamespace

			got, err := o.FromService(svc("storefront", "auth", tc.annotations, port("http", 9091)), tc.nsGroup)
			if err != nil {
				t.Fatalf("FromService: %v", err)
			}
			if got[0].Group != tc.want {
				t.Errorf("Group = %q, want %q", got[0].Group, tc.want)
			}
		})
	}
}

func TestFromServiceTemplates(t *testing.T) {
	tests := []struct {
		name      string
		anns      map[string]string
		want      []string
		wantExtra []string
	}{
		{
			name: "no annotation leaves selection to defaultFor",
			anns: ann("enabled", "true"),
		},
		{
			name: "comma separated list",
			anns: ann("enabled", "true", "template", "default-http, strict"),
			want: []string{"default-http", "strict"},
		},
		{
			name: "trailing comma is harmless",
			anns: ann("enabled", "true", "template", "default-http,"),
			want: []string{"default-http"},
		},
		{
			name:      "template-extra is separate",
			anns:      ann("enabled", "true", "template-extra", "paged-oncall"),
			wantExtra: []string{"paged-oncall"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Defaults().FromService(svc("storefront", "x", tc.anns, port("http", 80)), "")
			if err != nil {
				t.Fatalf("FromService: %v", err)
			}
			if !reflect.DeepEqual(got[0].Templates, tc.want) {
				t.Errorf("Templates = %v, want %v", got[0].Templates, tc.want)
			}
			if !reflect.DeepEqual(got[0].ExtraTemplates, tc.wantExtra) {
				t.Errorf("ExtraTemplates = %v, want %v", got[0].ExtraTemplates, tc.wantExtra)
			}
		})
	}
}

func TestFromServiceRawPatch(t *testing.T) {
	// A workload behind forward auth, which returns 4xx without the headers.
	annotations := ann("enabled", "true", "group", "Platform")
	annotations[AnnotationPrefix+AnnEndpoint] = "conditions:\n  - \"[STATUS] > 400\"\n  - \"[STATUS] < 500\"\n"

	got, err := Defaults().FromService(svc("platform", "portal", annotations, port("http", 80)), "")
	if err != nil {
		t.Fatalf("FromService: %v", err)
	}

	conditions, ok := got[0].Patch["conditions"].([]any)
	if !ok {
		t.Fatalf("Patch = %#v, want a conditions list", got[0].Patch)
	}
	if len(conditions) != 2 || conditions[0] != "[STATUS] > 400" {
		t.Errorf("conditions = %#v", conditions)
	}
}

func TestFromServiceEndpointsList(t *testing.T) {
	// One Service exposing both a health port and a console port.
	annotations := ann("enabled", "true", "group", "Platform")
	annotations[AnnotationPrefix+AnnEndpoints] = `
- name: Object store
  port: 9000
  path: /health
- name: Object store console
  port: 9001
  interval: 5m
`

	got, err := Defaults().FromService(svc("platform", "object-store", annotations, port("api", 9000), port("ui", 9001)), "")
	if err != nil {
		t.Fatalf("FromService: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d endpoints, want 2", len(got))
	}

	if got[0].Port != 9000 || got[0].Path != "/health" {
		t.Errorf("first = port %d path %q, want 9000 /health", got[0].Port, got[0].Path)
	}
	if got[0].Host != "object-store.platform.svc.cluster.local" {
		t.Errorf("first Host = %q", got[0].Host)
	}
	if got[0].Name != "Object store" {
		t.Errorf("first Name = %q, want Object store", got[0].Name)
	}
	if got[1].Port != 9001 || got[1].Path != "" {
		t.Errorf("second = port %d path %q, want 9001 and no path", got[1].Port, got[1].Path)
	}
	// Non-control keys in a list item flow through as the patch.
	if got[1].Patch["interval"] != "5m" {
		t.Errorf("second Patch = %#v, want interval 5m", got[1].Patch)
	}
	// Control keys must not leak into the rendered endpoint.
	for _, key := range []string{AnnPort, AnnPath} {
		if _, leaked := got[0].Patch[key]; leaked {
			t.Errorf("control key %q leaked into the patch: %#v", key, got[0].Patch)
		}
	}
}

func TestFromServiceEndpointsListInheritsShortcuts(t *testing.T) {
	// Shared settings are written once on the object; the namespace is
	// deliberately unrelated to the group so inheritance cannot be confused
	// with the namespace fallback.
	annotations := ann("enabled", "true", "group", "Platform", "template", "strict")
	annotations[AnnotationPrefix+AnnEndpoints] = "- name: A\n  port: 9000\n- name: B\n  port: 9001\n  group: Other\n"

	got, err := Defaults().FromService(svc("storage", "object-store", annotations, port("a", 9000), port("b", 9001)), "")
	if err != nil {
		t.Fatalf("FromService: %v", err)
	}
	if got[0].Group != "Platform" {
		t.Errorf("first Group = %q, want the inherited Platform", got[0].Group)
	}
	if got[1].Group != "Other" {
		t.Errorf("second Group = %q, want the item's own Other", got[1].Group)
	}
	for i, ep := range got {
		if !reflect.DeepEqual(ep.Templates, []string{"strict"}) {
			t.Errorf("endpoint %d Templates = %v, want the inherited [strict]", i, ep.Templates)
		}
	}
}

func TestFromServiceEndpointsListItemsOverrideShortcuts(t *testing.T) {
	annotations := ann("enabled", "true", "name", "Ignored", "port", "9999", "scheme", "tcp")
	annotations[AnnotationPrefix+AnnEndpoints] = "- name: Real\n  port: 9000\n"

	got, err := Defaults().FromService(svc("platform", "object-store", annotations, port("a", 9000)), "")
	if err != nil {
		t.Fatalf("FromService: %v", err)
	}
	if len(got) != 1 || got[0].Name != "Real" {
		t.Fatalf("got %#v, want a single endpoint named Real", got)
	}
	if got[0].Port != 9000 {
		t.Errorf("Port = %d, want 9000 from the list item", got[0].Port)
	}
	// Unset item fields still inherit.
	if got[0].Scheme != "tcp" {
		t.Errorf("Scheme = %q, want the inherited tcp", got[0].Scheme)
	}
}

func TestFromServiceEndpointsListInheritsPatchKeyByKey(t *testing.T) {
	annotations := ann("enabled", "true")
	annotations[AnnotationPrefix+AnnEndpoint] = "interval: 1m\nclient:\n  timeout: 10s\n"
	annotations[AnnotationPrefix+AnnEndpoints] = "- name: A\n  port: 9000\n- name: B\n  port: 9001\n  interval: 5m\n"

	got, err := Defaults().FromService(svc("ns", "x", annotations, port("a", 9000), port("b", 9001)), "")
	if err != nil {
		t.Fatalf("FromService: %v", err)
	}
	if got[0].Patch["interval"] != "1m" {
		t.Errorf("first interval = %v, want the inherited 1m", got[0].Patch["interval"])
	}
	if got[1].Patch["interval"] != "5m" {
		t.Errorf("second interval = %v, want the item's own 5m", got[1].Patch["interval"])
	}
	// Keys the item did not mention survive the overlay.
	if got[1].Patch["client"] == nil {
		t.Errorf("second Patch = %#v, want the inherited client block", got[1].Patch)
	}
}

func TestFromServiceMalformedAnnotations(t *testing.T) {
	tests := []struct {
		name    string
		suffix  string
		value   string
		wantErr string
	}{
		{"endpoint is not a mapping", AnnEndpoint, "- just\n- a list\n", "expected a mapping"},
		{"endpoint is not valid yaml", AnnEndpoint, "key: [unclosed\n", "endpoint"},
		{"endpoints is not a list", AnnEndpoints, "name: single\n", "not a YAML list"},
		{"endpoints item is not a mapping", AnnEndpoints, "- scalar\n", "not a mapping"},
		{"endpoints item has a bad type", AnnEndpoints, "- name: [a, b]\n", "must be a string"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			annotations := ann("enabled", "true")
			annotations[AnnotationPrefix+tc.suffix] = tc.value

			_, err := Defaults().FromService(svc("ns", "x", annotations, port("http", 80)), "")
			if err == nil {
				t.Fatal("FromService() = nil error, want a failure")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestFromServiceExternalNameIsRejected(t *testing.T) {
	s := svc("ns", "x", ann("enabled", "true"), port("http", 80))
	s.Spec.Type = corev1.ServiceTypeExternalName

	_, err := Defaults().FromService(s, "")
	if err == nil {
		t.Fatal("FromService() = nil error, want a failure for ExternalName")
	}
	if !strings.Contains(err.Error(), "ExternalName") {
		t.Errorf("error = %q, want it to mention ExternalName", err)
	}
}

func TestFromServiceExternalNameWithExplicitURLStillRejected(t *testing.T) {
	// Documents current behaviour: the check fires before annotations are read.
	s := svc("ns", "x", ann("enabled", "true", "url", "https://example.org"), port("http", 80))
	s.Spec.Type = corev1.ServiceTypeExternalName

	if _, err := Defaults().FromService(s, ""); err == nil {
		t.Fatal("FromService() = nil error, want a failure")
	}
}

func TestParseMode(t *testing.T) {
	tests := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"opt-in", ModeOptIn, false},
		{"auto", ModeAuto, false},
		{"disabled", ModeDisabled, false},
		{"  AUTO  ", ModeAuto, false},
		{"", "", true},
		{"nonsense", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseMode(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseMode(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ParseMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestKey(t *testing.T) {
	if got, want := Key(AnnGroup), "gatus.kalexlab.xyz/group"; got != want {
		t.Errorf("Key(%q) = %q, want %q", AnnGroup, got, want)
	}
}

func TestAnnotationTrimsWhitespace(t *testing.T) {
	// Annotations written as YAML block scalars carry a trailing newline.
	got, ok := Annotation(map[string]string{Key(AnnGroup): "  Storefront\n"}, AnnGroup)
	if !ok || got != "Storefront" {
		t.Errorf("Annotation() = %q, %v, want \"Storefront\", true", got, ok)
	}
}

func TestDisplayName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"worker-queue", "Worker queue"},
		{"helper_backend", "Helper backend"},
		{"web", "Web"},
		{"team-a-v2", "Team a v2"},
		{"", ""},
		// Existing capitalisation is a deliberate signal and is left alone.
		{"Control plane", "Control plane"},
		{"Home Assistant", "Home Assistant"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := DisplayName(tc.in); got != tc.want {
				t.Errorf("DisplayName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
