// Package registry holds the discovered, still-unresolved endpoints that the
// renderer turns into a Gatus configuration.
package registry

import (
	"sort"
	"sync"

	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
)

// Key identifies the Kubernetes object an entry came from. Objects of different
// kinds can share a namespace and name, so the kind is part of the key.
type Key struct {
	Kind      string
	Namespace string
	Name      string
}

// Registry stores endpoints per source object and signals when they change.
//
// It holds unresolved endpoints on purpose: templates are applied at render
// time, so editing an EndpointTemplate needs no reverse index from templates
// back to the objects that reference them.
type Registry struct {
	mu      sync.RWMutex
	entries map[Key][]config.Endpoint

	// changed is a coalescing signal: it has room for one pending notification,
	// so a burst of updates wakes the render loop once rather than queueing work
	// per event.
	changed chan struct{}
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{
		entries: make(map[Key][]config.Endpoint),
		changed: make(chan struct{}, 1),
	}
}

// Changed returns the channel that receives a value whenever the contents change.
func (r *Registry) Changed() <-chan struct{} { return r.changed }

// Set replaces the endpoints for an object, reporting whether anything actually
// differs. Reconcilers run on resync even when nothing changed, so suppressing
// no-op updates here keeps the render loop from waking up for nothing.
func (r *Registry) Set(key Key, endpoints []config.Endpoint) bool {
	r.mu.Lock()
	existing, present := r.entries[key]

	if len(endpoints) == 0 {
		if !present {
			r.mu.Unlock()
			return false
		}
		delete(r.entries, key)
		r.mu.Unlock()
		r.notify()
		return true
	}

	if present && equalEndpoints(existing, endpoints) {
		r.mu.Unlock()
		return false
	}

	stored := make([]config.Endpoint, len(endpoints))
	copy(stored, endpoints)
	r.entries[key] = stored
	r.mu.Unlock()

	r.notify()
	return true
}

// Delete drops an object's endpoints, reporting whether anything was removed.
func (r *Registry) Delete(key Key) bool {
	return r.Set(key, nil)
}

// Touch signals a change without altering any entry. Used when something outside
// the registry, such as an EndpointTemplate, invalidates the rendered output.
func (r *Registry) Touch() { r.notify() }

// Snapshot returns every endpoint, in a stable order so that two renders of the
// same cluster state produce identical bytes.
func (r *Registry) Snapshot() []config.Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys := make([]Key, 0, len(r.entries))
	total := 0
	for k, v := range r.entries {
		keys = append(keys, k)
		total += len(v)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Kind != keys[j].Kind {
			return keys[i].Kind < keys[j].Kind
		}
		if keys[i].Namespace != keys[j].Namespace {
			return keys[i].Namespace < keys[j].Namespace
		}
		return keys[i].Name < keys[j].Name
	})

	out := make([]config.Endpoint, 0, total)
	for _, k := range keys {
		out = append(out, r.entries[k]...)
	}
	return out
}

// Len returns the number of source objects currently contributing endpoints.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// notify posts a change signal without blocking. Dropping the signal when one is
// already pending is safe: the waiting renderer reads the whole snapshot anyway.
func (r *Registry) notify() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// equalEndpoints compares two endpoint lists for render-relevant equality.
func equalEndpoints(a, b []config.Endpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !equalEndpoint(a[i], b[i]) {
			return false
		}
	}
	return true
}

func equalEndpoint(a, b config.Endpoint) bool {
	if a.Source != b.Source || a.SourceRef != b.SourceRef ||
		a.Name != b.Name || a.Group != b.Group ||
		a.URL != b.URL || a.Host != b.Host || a.Port != b.Port || a.Path != b.Path ||
		a.Scheme != b.Scheme || a.TemplatesSet != b.TemplatesSet {
		return false
	}
	if !equalStrings(a.Templates, b.Templates) || !equalStrings(a.ExtraTemplates, b.ExtraTemplates) {
		return false
	}
	return equalValue(a.Patch, b.Patch)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// equalValue compares two decoded YAML values structurally. reflect.DeepEqual
// would work, but patches are small and this avoids reflection on a hot path
// that every reconcile hits.
func equalValue(a, b any) bool {
	switch av := a.(type) {
	case config.Object:
		bv, ok := b.(config.Object)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			other, present := bv[k]
			if !present || !equalValue(v, other) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !equalValue(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
