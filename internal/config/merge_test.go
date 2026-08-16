package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// mustYAML decodes a YAML fragment the way the annotation parser and the base
// config loader do, so tests exercise the same value shapes as production.
func mustYAML(t *testing.T, in string) Object {
	t.Helper()
	var obj Object
	if err := yaml.Unmarshal([]byte(in), &obj); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if obj == nil {
		return Object{}
	}
	return obj
}

func TestMerge(t *testing.T) {
	tests := []struct {
		name string
		dst  string
		src  string
		want string
	}{
		{
			name: "disjoint keys union",
			dst:  "interval: 1m",
			src:  "group: Storefront",
			want: "interval: 1m\ngroup: Storefront",
		},
		{
			name: "scalar replaced",
			dst:  "interval: 1m",
			src:  "interval: 5m",
			want: "interval: 5m",
		},
		{
			// A workload behind forward auth overrides the template's conditions
			// outright instead of accumulating both sets.
			name: "list replaced not appended",
			dst:  "conditions: [\"[STATUS] == 200\"]",
			src:  "conditions: [\"[STATUS] > 400\", \"[STATUS] < 500\"]",
			want: "conditions: [\"[STATUS] > 400\", \"[STATUS] < 500\"]",
		},
		{
			name: "nested maps merge recursively",
			dst:  "client:\n  timeout: 10s\n  insecure: false",
			src:  "client:\n  insecure: true",
			want: "client:\n  timeout: 10s\n  insecure: true",
		},
		{
			name: "type mismatch: src wins",
			dst:  "ui:\n  hide-hostname: true",
			src:  "ui: disabled",
			want: "ui: disabled",
		},
		{
			name: "empty src is identity",
			dst:  "interval: 1m",
			src:  "{}",
			want: "interval: 1m",
		},
		{
			name: "env var placeholders pass through verbatim",
			dst:  "{}",
			src:  "alerts:\n  - type: telegram\n    provider-override:\n      id: \"${TELEGRAM_SECONDARY_ID}\"",
			want: "alerts:\n  - type: telegram\n    provider-override:\n      id: \"${TELEGRAM_SECONDARY_ID}\"",
		},
		{
			name: "explicit null replaces value",
			dst:  "group: Storefront",
			src:  "group: null",
			want: "group: null",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dst := mustYAML(t, tc.dst)
			src := mustYAML(t, tc.src)
			got := Merge(dst, src)

			if want := mustYAML(t, tc.want); !reflect.DeepEqual(got, want) {
				t.Errorf("Merge() = %#v, want %#v", got, want)
			}
			// Merge must not mutate either input.
			if !reflect.DeepEqual(dst, mustYAML(t, tc.dst)) {
				t.Errorf("Merge() mutated dst: %#v", dst)
			}
			if !reflect.DeepEqual(src, mustYAML(t, tc.src)) {
				t.Errorf("Merge() mutated src: %#v", src)
			}
		})
	}
}

func TestMergeDoesNotAliasSource(t *testing.T) {
	src := mustYAML(t, "client:\n  timeout: 10s\nconditions: [a, b]")
	got := Merge(Object{}, src)

	// Mutating the result must not reach back into src, or one endpoint's
	// override would leak into every other endpoint sharing the template.
	got["client"].(Object)["timeout"] = "99s"
	got["conditions"].([]any)[0] = "mutated"

	if v := src["client"].(Object)["timeout"]; v != "10s" {
		t.Errorf("src map aliased: timeout = %v", v)
	}
	if v := src["conditions"].([]any)[0]; v != "a" {
		t.Errorf("src slice aliased: conditions[0] = %v", v)
	}
}

func TestMergeAll(t *testing.T) {
	// Precedence chain: derived defaults, then template, then annotations.
	derived := mustYAML(t, "name: Portal\ngroup: Platform\nurl: http://portal.platform.svc.cluster.local:80")
	template := mustYAML(t, "interval: 1m\nconditions: [\"[STATUS] == 200\"]\nalerts:\n  - type: telegram")
	patch := mustYAML(t, "conditions: [\"[STATUS] > 400\", \"[STATUS] < 500\"]")

	got := MergeAll(derived, nil, template, patch)

	want := mustYAML(t, `
name: Portal
group: Platform
url: http://portal.platform.svc.cluster.local:80
interval: 1m
conditions: ["[STATUS] > 400", "[STATUS] < 500"]
alerts:
  - type: telegram
`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeAll() = %#v, want %#v", got, want)
	}
}

func TestMergeAllNoLayers(t *testing.T) {
	if got := MergeAll(); !reflect.DeepEqual(got, Object{}) {
		t.Errorf("MergeAll() = %#v, want empty object", got)
	}
}

func TestAsObjectNormalisesNonStringKeyedMaps(t *testing.T) {
	// yaml.v3 produces map[string]any for string keys, but a YAML document with
	// a non-string key yields map[any]any. Merging must not panic on either.
	dst := Object{"client": map[any]any{"timeout": "10s"}}
	src := Object{"client": Object{"insecure": true}}

	got := Merge(dst, src)
	client, ok := got["client"].(Object)
	if !ok {
		t.Fatalf("client = %#v, want Object", got["client"])
	}
	if client["timeout"] != "10s" || client["insecure"] != true {
		t.Errorf("client = %#v, want both keys merged", client)
	}
}
