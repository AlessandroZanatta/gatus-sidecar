package config

import (
	"errors"
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	gatusv1alpha1 "github.com/AlessandroZanatta/gatus-sidecar/api/v1alpha1"
)

// tpl builds an EndpointTemplate whose endpoint body is written as YAML, the way
// an operator writes it in a manifest. sigs.k8s.io/yaml converts to JSON, which is
// exactly what the API server stores in the RawExtension.
func tpl(t *testing.T, name string, scheme string, defaultFor []string, extends []string, endpointYAML string) gatusv1alpha1.EndpointTemplate {
	t.Helper()
	out := gatusv1alpha1.EndpointTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gatusv1alpha1.EndpointTemplateSpec{
			Scheme:     scheme,
			DefaultFor: defaultFor,
			Extends:    extends,
		},
	}
	if endpointYAML != "" {
		raw, err := yaml.YAMLToJSON([]byte(endpointYAML))
		if err != nil {
			t.Fatalf("convert endpoint yaml for %q: %v", name, err)
		}
		out.Spec.Endpoint = &apiextensionsv1.JSON{Raw: raw}
	}
	return out
}

// realWorldTemplates mirrors the anchors in the Gatus config this sidecar replaces.
func realWorldTemplates(t *testing.T) []gatusv1alpha1.EndpointTemplate {
	t.Helper()
	return []gatusv1alpha1.EndpointTemplate{
		tpl(t, "common-alerts", "", nil, nil, `
interval: 1m
alerts:
  - type: telegram
  - type: telegram
    provider-override:
      id: "${TELEGRAM_SECONDARY_ID}"
`),
		tpl(t, "default-http", "http", []string{"http", "https"}, []string{"common-alerts"}, `
conditions:
  - "[STATUS] == 200"
`),
		tpl(t, "default-tcp", "tcp", []string{"tcp"}, []string{"common-alerts"}, `
conditions:
  - "[CONNECTED] == true"
`),
	}
}

func TestResolveFlattensExtends(t *testing.T) {
	ts := NewTemplateSet(realWorldTemplates(t))

	got, err := ts.Resolve("default-http")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := mustYAML(t, `
interval: 1m
conditions: ["[STATUS] == 200"]
alerts:
  - type: telegram
  - type: telegram
    provider-override:
      id: "${TELEGRAM_SECONDARY_ID}"
`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve() = %#v\nwant %#v", got, want)
	}
}

func TestResolveOwnBodyWinsOverParent(t *testing.T) {
	ts := NewTemplateSet([]gatusv1alpha1.EndpointTemplate{
		tpl(t, "base", "", nil, nil, "interval: 1m\nconditions: [\"[STATUS] == 200\"]"),
		tpl(t, "slow", "", nil, []string{"base"}, "interval: 10m"),
	})

	got, err := ts.Resolve("slow")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["interval"] != "10m" {
		t.Errorf("interval = %v, want 10m (own body must win)", got["interval"])
	}
	if got["conditions"] == nil {
		t.Error("conditions dropped; parent keys must survive")
	}
}

func TestResolveExtendsOrderLastWins(t *testing.T) {
	ts := NewTemplateSet([]gatusv1alpha1.EndpointTemplate{
		tpl(t, "a", "", nil, nil, "interval: 1m\ngroup: FromA"),
		tpl(t, "b", "", nil, nil, "interval: 5m"),
		tpl(t, "c", "", nil, []string{"a", "b"}, ""),
	})

	got, err := ts.Resolve("c")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["interval"] != "5m" {
		t.Errorf("interval = %v, want 5m (later extends entry wins)", got["interval"])
	}
	if got["group"] != "FromA" {
		t.Errorf("group = %v, want FromA (non-conflicting parent key survives)", got["group"])
	}
}

func TestResolveDeepChain(t *testing.T) {
	ts := NewTemplateSet([]gatusv1alpha1.EndpointTemplate{
		tpl(t, "l1", "", nil, nil, "a: 1"),
		tpl(t, "l2", "", nil, []string{"l1"}, "b: 2"),
		tpl(t, "l3", "", nil, []string{"l2"}, "c: 3"),
	})

	got, err := ts.Resolve("l3")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := Object{"a": float64(1), "b": float64(2), "c": float64(3)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveDiamondMergesOnce(t *testing.T) {
	// root is reached through both mid1 and mid2. Resolution must succeed and
	// the nearer definition must win rather than the chain being rejected.
	ts := NewTemplateSet([]gatusv1alpha1.EndpointTemplate{
		tpl(t, "root", "", nil, nil, `interval: 1m`+"\n"+`root: "from-root"`),
		tpl(t, "mid1", "", nil, []string{"root"}, "interval: 2m"),
		tpl(t, "mid2", "", nil, []string{"root"}, "interval: 3m"),
		tpl(t, "leaf", "", nil, []string{"mid1", "mid2"}, ""),
	})

	got, err := ts.Resolve("leaf")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["interval"] != "3m" {
		t.Errorf("interval = %v, want 3m", got["interval"])
	}
	if got["root"] != "from-root" {
		t.Errorf("root = %v, want from-root", got["root"])
	}
}

func TestResolveErrors(t *testing.T) {
	tests := []struct {
		name       string
		templates  []gatusv1alpha1.EndpointTemplate
		resolve    string
		wantReason string
	}{
		{
			name:       "unknown template",
			templates:  nil,
			resolve:    "nope",
			wantReason: gatusv1alpha1.ReasonUnknownParent,
		},
		{
			name: "unknown parent",
			templates: []gatusv1alpha1.EndpointTemplate{
				tpl(t, "child", "", nil, []string{"missing"}, ""),
			},
			resolve:    "child",
			wantReason: gatusv1alpha1.ReasonUnknownParent,
		},
		{
			name: "self cycle",
			templates: []gatusv1alpha1.EndpointTemplate{
				tpl(t, "loop", "", nil, []string{"loop"}, ""),
			},
			resolve:    "loop",
			wantReason: gatusv1alpha1.ReasonCycle,
		},
		{
			name: "mutual cycle",
			templates: []gatusv1alpha1.EndpointTemplate{
				tpl(t, "a", "", nil, []string{"b"}, ""),
				tpl(t, "b", "", nil, []string{"a"}, ""),
			},
			resolve:    "a",
			wantReason: gatusv1alpha1.ReasonCycle,
		},
		{
			name: "endpoint body is not an object",
			templates: []gatusv1alpha1.EndpointTemplate{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "bad"},
					Spec: gatusv1alpha1.EndpointTemplateSpec{
						Endpoint: &apiextensionsv1.JSON{Raw: []byte(`["not","an","object"]`)},
					},
				},
			},
			resolve:    "bad",
			wantReason: gatusv1alpha1.ReasonInvalidEndpoint,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := NewTemplateSet(tc.templates)
			_, err := ts.Resolve(tc.resolve)
			if err == nil {
				t.Fatal("Resolve() = nil error, want failure")
			}

			var re *ResolveError
			if !errors.As(err, &re) {
				t.Fatalf("error %v is not a *ResolveError", err)
			}
			if re.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q (%v)", re.Reason, tc.wantReason, err)
			}
			if got := ts.Err(tc.resolve); got == nil {
				t.Error("Err() = nil, want the recorded failure for status reporting")
			}
		})
	}
}

func TestResolveCycleDoesNotHang(t *testing.T) {
	// A cycle reached from outside must terminate and be attributed to the
	// entry point, not spin until the stack blows.
	ts := NewTemplateSet([]gatusv1alpha1.EndpointTemplate{
		tpl(t, "entry", "", nil, []string{"a"}, ""),
		tpl(t, "a", "", nil, []string{"b"}, ""),
		tpl(t, "b", "", nil, []string{"a"}, ""),
	})

	done := make(chan error, 1)
	go func() {
		_, err := ts.Resolve("entry")
		done <- err
	}()
	if err := <-done; err == nil {
		t.Fatal("Resolve() = nil error, want cycle failure")
	}
}

func TestResolveManySkipsBrokenTemplates(t *testing.T) {
	ts := NewTemplateSet([]gatusv1alpha1.EndpointTemplate{
		tpl(t, "good", "", nil, nil, "interval: 1m"),
	})

	got, errs := ts.ResolveMany([]string{"good", "missing"})
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly one", errs)
	}
	// The good template must still contribute: one broken reference cannot
	// blank an endpoint's configuration.
	if got["interval"] != "1m" {
		t.Errorf("interval = %v, want 1m", got["interval"])
	}
}

func TestResolveManyOrderLastWins(t *testing.T) {
	ts := NewTemplateSet([]gatusv1alpha1.EndpointTemplate{
		tpl(t, "first", "", nil, nil, `interval: 1m`+"\n"+`first: "from-first"`),
		tpl(t, "second", "", nil, nil, "interval: 9m"),
	})

	got, errs := ts.ResolveMany([]string{"first", "second"})
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if got["interval"] != "9m" {
		t.Errorf("interval = %v, want 9m", got["interval"])
	}
	if got["first"] != "from-first" {
		t.Errorf("first = %v, want from-first", got["first"])
	}
}

func TestResolveMissingEndpointBodyIsEmpty(t *testing.T) {
	ts := NewTemplateSet([]gatusv1alpha1.EndpointTemplate{
		// A template that exists only to declare a scheme is legitimate.
		tpl(t, "scheme-only", "tcp", []string{"tcp"}, nil, ""),
	})

	got, err := ts.Resolve("scheme-only")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Resolve() = %#v, want empty object", got)
	}
}

func TestResolveResultIsNotSharedBetweenCallers(t *testing.T) {
	ts := NewTemplateSet(realWorldTemplates(t))

	first, err := ts.Resolve("default-http")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// ResolveMany merges into a fresh object; mutating its output must not
	// corrupt the cache that the next endpoint will read.
	merged, _ := ts.ResolveMany([]string{"default-http"})
	merged["interval"] = "999m"

	if first["interval"] != "1m" {
		t.Errorf("cached template mutated: interval = %v", first["interval"])
	}
	second, _ := ts.ResolveMany([]string{"default-http"})
	if second["interval"] != "1m" {
		t.Errorf("second resolve saw mutation: interval = %v", second["interval"])
	}
}

func TestDefaultsFor(t *testing.T) {
	ts := NewTemplateSet(realWorldTemplates(t))

	tests := []struct {
		scheme string
		want   []string
	}{
		{"http", []string{"default-http"}},
		{"https", []string{"default-http"}},
		{"HTTP", []string{"default-http"}}, // matching is case-insensitive
		{"tcp", []string{"default-tcp"}},
		{"dns", nil},
	}

	for _, tc := range tests {
		t.Run(tc.scheme, func(t *testing.T) {
			if got := ts.DefaultsFor(tc.scheme); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DefaultsFor(%q) = %v, want %v", tc.scheme, got, tc.want)
			}
		})
	}
}

func TestDefaultsForIsSortedNotInsertionOrdered(t *testing.T) {
	// Rendered output must not depend on the order templates were applied.
	ts := NewTemplateSet([]gatusv1alpha1.EndpointTemplate{
		tpl(t, "zulu", "", []string{"http"}, nil, ""),
		tpl(t, "alpha", "", []string{"http"}, nil, ""),
	})

	want := []string{"alpha", "zulu"}
	for i := 0; i < 5; i++ {
		if got := ts.DefaultsFor("http"); !reflect.DeepEqual(got, want) {
			t.Fatalf("DefaultsFor() = %v, want %v", got, want)
		}
	}
}

func TestSchemeOf(t *testing.T) {
	ts := NewTemplateSet(realWorldTemplates(t))

	if got := ts.SchemeOf("default-tcp"); got != "tcp" {
		t.Errorf("SchemeOf(default-tcp) = %q, want tcp", got)
	}
	if got := ts.SchemeOf("common-alerts"); got != "" {
		t.Errorf("SchemeOf(common-alerts) = %q, want empty", got)
	}
	if got := ts.SchemeOf("nonexistent"); got != "" {
		t.Errorf("SchemeOf(nonexistent) = %q, want empty", got)
	}
}

func TestNames(t *testing.T) {
	ts := NewTemplateSet(realWorldTemplates(t))
	want := []string{"common-alerts", "default-http", "default-tcp"}
	if got := ts.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}
