package registry

import (
	"testing"

	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
)

func key(ns, name string) Key { return Key{Kind: "Service", Namespace: ns, Name: name} }

func endpoint(name string) config.Endpoint {
	return config.Endpoint{
		Source:    config.SourceService,
		SourceRef: "ns/" + name,
		Name:      name,
		Host:      name + ".ns.svc.cluster.local",
		Port:      80,
	}
}

// drain reports whether a change signal is pending, consuming it if so.
func drain(r *Registry) bool {
	select {
	case <-r.Changed():
		return true
	default:
		return false
	}
}

func TestSetAddsAndSignals(t *testing.T) {
	r := New()

	if !r.Set(key("storefront", "web"), []config.Endpoint{endpoint("Web")}) {
		t.Error("Set() = false, want true for a new entry")
	}
	if !drain(r) {
		t.Error("no change signal after Set")
	}
	if r.Len() != 1 {
		t.Errorf("Len() = %d, want 1", r.Len())
	}
	if got := r.Snapshot(); len(got) != 1 || got[0].Name != "Web" {
		t.Errorf("Snapshot() = %#v", got)
	}
}

func TestSetIdenticalIsANoOp(t *testing.T) {
	// Reconcilers fire on resync even when nothing changed; waking the render
	// loop for those would reload Gatus for no reason.
	r := New()
	k := key("storefront", "web")

	r.Set(k, []config.Endpoint{endpoint("Web")})
	drain(r)

	if r.Set(k, []config.Endpoint{endpoint("Web")}) {
		t.Error("Set() = true for identical content, want false")
	}
	if drain(r) {
		t.Error("change signalled for identical content")
	}
}

func TestSetDetectsFieldChanges(t *testing.T) {
	base := endpoint("Web")

	tests := []struct {
		name   string
		mutate func(*config.Endpoint)
	}{
		{"name", func(e *config.Endpoint) { e.Name = "Other" }},
		{"group", func(e *config.Endpoint) { e.Group = "Other" }},
		{"host", func(e *config.Endpoint) { e.Host = "other" }},
		{"port", func(e *config.Endpoint) { e.Port = 8080 }},
		{"path", func(e *config.Endpoint) { e.Path = "/health" }},
		{"url", func(e *config.Endpoint) { e.URL = "https://example.org" }},
		{"scheme", func(e *config.Endpoint) { e.Scheme = "tcp" }},
		{"templates", func(e *config.Endpoint) { e.Templates = []string{"x"} }},
		{"templatesSet", func(e *config.Endpoint) { e.TemplatesSet = true }},
		{"extraTemplates", func(e *config.Endpoint) { e.ExtraTemplates = []string{"x"} }},
		{"patch", func(e *config.Endpoint) { e.Patch = config.Object{"interval": "5m"} }},
		{"source", func(e *config.Endpoint) { e.Source = config.SourceIngressRoute }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			k := key("storefront", "web")
			r.Set(k, []config.Endpoint{base})
			drain(r)

			changed := base
			tc.mutate(&changed)

			if !r.Set(k, []config.Endpoint{changed}) {
				t.Errorf("Set() = false after changing %s, want true", tc.name)
			}
			if !drain(r) {
				t.Errorf("no change signal after changing %s", tc.name)
			}
		})
	}
}

func TestSetDetectsNestedPatchChanges(t *testing.T) {
	r := New()
	k := key("ns", "x")

	withPatch := func(timeout string) config.Endpoint {
		e := endpoint("X")
		e.Patch = config.Object{
			"client":     config.Object{"timeout": timeout},
			"conditions": []any{"[STATUS] == 200"},
		}
		return e
	}

	r.Set(k, []config.Endpoint{withPatch("10s")})
	drain(r)

	if r.Set(k, []config.Endpoint{withPatch("10s")}) {
		t.Error("Set() = true for an identical nested patch, want false")
	}
	if !r.Set(k, []config.Endpoint{withPatch("30s")}) {
		t.Error("Set() = false for a changed nested patch, want true")
	}
}

func TestSetEmptyRemovesEntry(t *testing.T) {
	r := New()
	k := key("storefront", "web")

	r.Set(k, []config.Endpoint{endpoint("Web")})
	drain(r)

	if !r.Set(k, nil) {
		t.Error("Set(nil) = false, want true when removing an existing entry")
	}
	if !drain(r) {
		t.Error("no change signal after removal")
	}
	if r.Len() != 0 {
		t.Errorf("Len() = %d, want 0", r.Len())
	}
}

func TestSetEmptyOnUnknownKeyIsANoOp(t *testing.T) {
	// A Service that never opted in is deleted: nothing to do, nothing to signal.
	r := New()
	if r.Set(key("ns", "never-seen"), nil) {
		t.Error("Set(nil) = true for an unknown key, want false")
	}
	if drain(r) {
		t.Error("change signalled for an unknown key")
	}
}

func TestDeleteIsSetNil(t *testing.T) {
	r := New()
	k := key("ns", "x")
	r.Set(k, []config.Endpoint{endpoint("X")})
	drain(r)

	if !r.Delete(k) {
		t.Error("Delete() = false, want true")
	}
	if r.Len() != 0 {
		t.Errorf("Len() = %d, want 0", r.Len())
	}
}

func TestTouchSignalsWithoutChangingEntries(t *testing.T) {
	// An EndpointTemplate edit changes the output without changing any entry.
	r := New()
	r.Set(key("ns", "x"), []config.Endpoint{endpoint("X")})
	drain(r)

	r.Touch()
	if !drain(r) {
		t.Error("no change signal after Touch")
	}
	if r.Len() != 1 {
		t.Errorf("Len() = %d, want the entries untouched", r.Len())
	}
}

func TestChangeSignalCoalesces(t *testing.T) {
	// A burst of updates must wake the render loop once, not queue work per event.
	r := New()
	for i, name := range []string{"a", "b", "c", "d"} {
		r.Set(key("ns", name), []config.Endpoint{endpoint(name)})
		_ = i
	}

	if !drain(r) {
		t.Fatal("no change signal after a burst")
	}
	if drain(r) {
		t.Error("second signal pending; the channel should coalesce")
	}
}

func TestSnapshotIsStablyOrdered(t *testing.T) {
	r := New()
	r.Set(Key{Kind: "Service", Namespace: "storefront", Name: "reports"}, []config.Endpoint{endpoint("Reports")})
	r.Set(Key{Kind: "Service", Namespace: "shop", Name: "shop"}, []config.Endpoint{endpoint("Shop")})
	r.Set(Key{Kind: "IngressRoute", Namespace: "aaa", Name: "aaa"}, []config.Endpoint{endpoint("Route")})

	want := []string{"Route", "Shop", "Reports"} // IngressRoute < Service, then by namespace
	for i := 0; i < 10; i++ {
		got := r.Snapshot()
		for j := range want {
			if got[j].Name != want[j] {
				t.Fatalf("Snapshot() = %v, want order %v", names(got), want)
			}
		}
	}
}

func TestSnapshotDoesNotAliasStoredEntries(t *testing.T) {
	r := New()
	k := key("ns", "x")
	r.Set(k, []config.Endpoint{endpoint("X")})

	snap := r.Snapshot()
	snap[0].Name = "Mutated"

	if again := r.Snapshot(); again[0].Name != "X" {
		t.Errorf("Snapshot() = %q, want the stored value unchanged", again[0].Name)
	}
}

func TestSetDoesNotAliasCallerSlice(t *testing.T) {
	r := New()
	k := key("ns", "x")

	input := []config.Endpoint{endpoint("X")}
	r.Set(k, input)
	input[0].Name = "Mutated"

	if got := r.Snapshot(); got[0].Name != "X" {
		t.Errorf("Snapshot() = %q, want the registry to hold its own copy", got[0].Name)
	}
}

func TestConcurrentAccessIsSafe(t *testing.T) {
	// Run under -race: several reconcilers write while the render loop reads.
	r := New()
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			r.Snapshot()
			r.Len()
		}
	}()
	for i := 0; i < 200; i++ {
		r.Set(key("ns", "x"), []config.Endpoint{endpoint("X")})
		r.Delete(key("ns", "x"))
	}
	<-done
}

func names(endpoints []config.Endpoint) []string {
	out := make([]string, len(endpoints))
	for i, e := range endpoints {
		out[i] = e.Name
	}
	return out
}
