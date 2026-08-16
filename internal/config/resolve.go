package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	gatusv1alpha1 "github.com/AlessandroZanatta/gatus-sidecar/api/v1alpha1"
)

// TemplateSet is an immutable snapshot of every EndpointTemplate known to the
// sidecar, keyed by name. The renderer resolves against a snapshot so a template
// changing mid-render cannot produce a half-old, half-new configuration.
type TemplateSet struct {
	byName map[string]*gatusv1alpha1.EndpointTemplate

	// resolved caches flattened endpoint bodies per template name, so a template
	// deep in many extends chains is only flattened once per render.
	resolved map[string]Object
	// failed records why a template could not be resolved, for status reporting.
	failed map[string]error
}

// NewTemplateSet snapshots the given templates. The templates themselves are not
// copied: callers must not mutate them afterwards, which holds for objects that
// come out of a controller-runtime cache read.
func NewTemplateSet(templates []gatusv1alpha1.EndpointTemplate) *TemplateSet {
	ts := &TemplateSet{
		byName:   make(map[string]*gatusv1alpha1.EndpointTemplate, len(templates)),
		resolved: make(map[string]Object, len(templates)),
		failed:   make(map[string]error),
	}
	for i := range templates {
		ts.byName[templates[i].Name] = &templates[i]
	}
	return ts
}

// Names returns every known template name, sorted, for deterministic iteration.
func (ts *TemplateSet) Names() []string {
	names := make([]string, 0, len(ts.byName))
	for name := range ts.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Get returns the named template, or nil.
func (ts *TemplateSet) Get(name string) *gatusv1alpha1.EndpointTemplate {
	return ts.byName[name]
}

// Err returns the resolution error recorded for a template, or nil. Only
// populated for templates that Resolve has been called on.
func (ts *TemplateSet) Err(name string) error { return ts.failed[name] }

// Resolve flattens a template's extends chain and merges its own endpoint body
// on top, returning the effective endpoint fragment it contributes.
//
// Ordering within a chain is depth-first, left to right: for extends [a, b], a's
// full chain is merged first, then b's, then this template's own body. So the
// nearest definition always wins, matching how a reader expects overrides to work.
//
// The returned object is a copy: callers may mutate it without corrupting the
// internal cache that every other endpoint using this template reads from.
func (ts *TemplateSet) Resolve(name string) (Object, error) {
	obj, err := ts.resolve(name, nil)
	if err != nil {
		return nil, err
	}
	return deepCopyObject(obj), nil
}

func (ts *TemplateSet) resolve(name string, stack []string) (Object, error) {
	if cached, ok := ts.resolved[name]; ok {
		return cached, nil
	}
	if err, ok := ts.failed[name]; ok {
		return nil, err
	}

	for _, seen := range stack {
		if seen == name {
			err := &ResolveError{
				Template: name,
				Reason:   gatusv1alpha1.ReasonCycle,
				Message:  fmt.Sprintf("extends cycle: %s -> %s", strings.Join(stack, " -> "), name),
			}
			ts.failed[name] = err
			return nil, err
		}
	}

	tpl, ok := ts.byName[name]
	if !ok {
		err := &ResolveError{
			Template: name,
			Reason:   gatusv1alpha1.ReasonUnknownParent,
			Message:  fmt.Sprintf("template %q not found", name),
		}
		ts.failed[name] = err
		return nil, err
	}

	stack = append(stack, name)
	out := Object{}
	for _, parent := range tpl.Spec.Extends {
		parentObj, err := ts.resolve(parent, stack)
		if err != nil {
			// Surface the underlying reason but attribute it to this template,
			// so the operator sees which object they need to fix.
			wrapped := &ResolveError{
				Template: name,
				Reason:   reasonOf(err),
				Message:  fmt.Sprintf("extends %q: %s", parent, err),
			}
			ts.failed[name] = wrapped
			return nil, wrapped
		}
		mergeInto(out, parentObj)
	}

	own, err := decodeEndpoint(tpl)
	if err != nil {
		ts.failed[name] = err
		return nil, err
	}
	mergeInto(out, own)

	ts.resolved[name] = out
	return out, nil
}

// ResolveMany flattens several templates and merges them in the given order,
// later templates winning. Templates that fail to resolve are skipped and
// reported: one broken template must never blank the whole configuration.
func (ts *TemplateSet) ResolveMany(names []string) (Object, []error) {
	out := Object{}
	var errs []error
	for _, name := range names {
		// Use the uncopied cache entry: mergeInto already deep-copies what it takes.
		obj, err := ts.resolve(name, nil)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		mergeInto(out, obj)
	}
	return out, errs
}

// DefaultsFor returns the names of templates that apply automatically to an
// endpoint with the given scheme, sorted by name for deterministic merge order.
//
// Sorting by name rather than by creation time means the rendered output does not
// depend on the order templates happened to be applied to the cluster. Templates
// that overlap on a scheme should not conflict; if they do, the alphabetically
// later one wins, and the operator can force an order with an explicit
// "template" annotation.
func (ts *TemplateSet) DefaultsFor(scheme string) []string {
	var names []string
	for name, tpl := range ts.byName {
		for _, s := range tpl.Spec.DefaultFor {
			if strings.EqualFold(s, scheme) {
				names = append(names, name)
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

// SchemeOf returns the scheme a template declares, or "" if it declares none.
func (ts *TemplateSet) SchemeOf(name string) string {
	if tpl, ok := ts.byName[name]; ok {
		return strings.ToLower(tpl.Spec.Scheme)
	}
	return ""
}

// decodeEndpoint converts a template's raw endpoint body into an Object.
// An absent body is an empty fragment, which is valid: a template may exist only
// to declare a scheme or to group others via extends.
func decodeEndpoint(tpl *gatusv1alpha1.EndpointTemplate) (Object, error) {
	if tpl.Spec.Endpoint == nil || len(tpl.Spec.Endpoint.Raw) == 0 {
		return Object{}, nil
	}
	var obj Object
	if err := json.Unmarshal(tpl.Spec.Endpoint.Raw, &obj); err != nil {
		return nil, &ResolveError{
			Template: tpl.Name,
			Reason:   gatusv1alpha1.ReasonInvalidEndpoint,
			Message:  fmt.Sprintf("spec.endpoint is not an object: %s", err),
		}
	}
	if obj == nil {
		return Object{}, nil
	}
	return obj, nil
}

// ResolveError describes why a template could not be resolved. Reason maps to an
// EndpointTemplate status condition reason.
type ResolveError struct {
	Template string
	Reason   string
	Message  string
}

func (e *ResolveError) Error() string { return e.Message }

func reasonOf(err error) string {
	var re *ResolveError
	if errors.As(err, &re) {
		return re.Reason
	}
	return gatusv1alpha1.ReasonUnknownParent
}
