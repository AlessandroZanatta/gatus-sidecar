package config

import (
	"fmt"
	"sort"
	"strings"
)

// endpointsKey is the top-level key in a Gatus config that this sidecar owns.
// Everything else in the base config is passed through untouched.
const endpointsKey = "endpoints"

// RenderOptions tunes assembly of the final document.
type RenderOptions struct {
	// DefaultScheme applies when neither the workload nor its templates say.
	DefaultScheme string
}

// Renderer turns discovered endpoints plus a template snapshot into the
// endpoints list of a Gatus configuration.
type Renderer struct {
	opts RenderOptions
}

// NewRenderer returns a Renderer, filling in an empty DefaultScheme with http.
func NewRenderer(opts RenderOptions) *Renderer {
	if opts.DefaultScheme == "" {
		opts.DefaultScheme = "http"
	}
	return &Renderer{opts: opts}
}

// RenderResult reports what a render produced, including the problems it worked
// around. Nothing here is fatal: a configuration that drops one broken endpoint
// is far better than one that fails to write at all and leaves Gatus stale.
type RenderResult struct {
	// Endpoints is the rendered list, sorted by group then name.
	Endpoints []Object

	// TemplateUsage counts how many endpoints resolved each template, for status.
	TemplateUsage map[string]int

	// Warnings describes endpoints that were dropped or renamed.
	Warnings []string
}

// Render builds the endpoints list. Endpoints are deduplicated by URL, then by
// group and name, then sorted so that identical inputs always produce identical
// bytes and Gatus is not reloaded for a no-op.
func (r *Renderer) Render(endpoints []Endpoint, templates *TemplateSet) RenderResult {
	res := RenderResult{TemplateUsage: map[string]int{}}

	ordered := sortForDedup(endpoints)
	ordered, res.Warnings = dedupByURL(ordered, r.opts.DefaultScheme, templates, res.Warnings)

	seenNames := make(map[[2]string]string, len(ordered))
	rendered := make([]Object, 0, len(ordered))

	for _, ep := range ordered {
		obj, warnings := r.renderOne(ep, templates, res.TemplateUsage)
		res.Warnings = append(res.Warnings, warnings...)

		key := ep.GroupKey()
		if prev, clash := seenNames[key]; clash {
			// Gatus keys stored history on group and name, so a duplicate would
			// silently interleave two services' results.
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%s %s: dropping endpoint %q in group %q, that name is already used by %s",
				ep.Source, ep.SourceRef, ep.Name, ep.Group, prev))
			continue
		}
		seenNames[key] = string(ep.Source) + " " + ep.SourceRef

		rendered = append(rendered, obj)
	}

	sort.SliceStable(rendered, func(i, j int) bool {
		gi, gj := stringValue(rendered[i], "group"), stringValue(rendered[j], "group")
		if gi != gj {
			return gi < gj
		}
		return stringValue(rendered[i], "name") < stringValue(rendered[j], "name")
	})

	res.Endpoints = rendered
	return res
}

// renderOne applies the full precedence chain for a single endpoint:
// templates, then identity, then the workload's raw patch.
func (r *Renderer) renderOne(ep Endpoint, templates *TemplateSet, usage map[string]int) (Object, []string) {
	var warnings []string

	scheme := r.resolveScheme(ep, templates)
	names := r.templateNames(ep, scheme, templates)

	tplLayer, errs := templates.ResolveMany(names)
	for _, err := range errs {
		warnings = append(warnings, fmt.Sprintf("%s %s: %s", ep.Source, ep.SourceRef, err))
	}
	for _, name := range names {
		if templates.Err(name) == nil {
			usage[name]++
		}
	}

	// Identity wins over templates: a template says how to check something, the
	// workload says what is being checked.
	identity := Object{
		"name": ep.Name,
		"url":  buildURL(ep, scheme),
	}
	if ep.Group != "" {
		identity["group"] = ep.Group
	}

	return MergeAll(tplLayer, identity, ep.Patch), warnings
}

// resolveScheme settles the URL scheme. An explicit annotation wins; otherwise
// the last explicitly-listed template that declares one decides, matching the
// merge rule that later templates win; otherwise the configured default applies.
func (r *Renderer) resolveScheme(ep Endpoint, templates *TemplateSet) string {
	if ep.Scheme != "" {
		return ep.Scheme
	}
	if ep.TemplatesSet {
		for i := len(ep.Templates) - 1; i >= 0; i-- {
			if s := templates.SchemeOf(ep.Templates[i]); s != "" {
				return s
			}
		}
	}
	for i := len(ep.ExtraTemplates) - 1; i >= 0; i-- {
		if s := templates.SchemeOf(ep.ExtraTemplates[i]); s != "" {
			return s
		}
	}
	return r.opts.DefaultScheme
}

// templateNames picks the templates that apply, in merge order.
func (r *Renderer) templateNames(ep Endpoint, scheme string, templates *TemplateSet) []string {
	var names []string
	if ep.TemplatesSet {
		names = append(names, ep.Templates...)
	} else {
		names = append(names, templates.DefaultsFor(scheme)...)
	}
	return append(names, ep.ExtraTemplates...)
}

// buildURL assembles the endpoint URL, unless one was given verbatim.
func buildURL(ep Endpoint, scheme string) string {
	if ep.URL != "" {
		return ep.URL
	}
	return fmt.Sprintf("%s://%s:%d%s", scheme, ep.Host, ep.Port, ep.Path)
}

// sortForDedup orders endpoints so dedup decisions are deterministic: by source
// priority, then by the object they came from, then by name.
func sortForDedup(endpoints []Endpoint) []Endpoint {
	out := make([]Endpoint, len(endpoints))
	copy(out, endpoints)
	sort.SliceStable(out, func(i, j int) bool {
		if pi, pj := out[i].Source.Priority(), out[j].Source.Priority(); pi != pj {
			return pi < pj
		}
		if out[i].SourceRef != out[j].SourceRef {
			return out[i].SourceRef < out[j].SourceRef
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// dedupByURL drops endpoints that would check exactly the same address. This is
// the common case where an IngressRoute is discovered alongside the Service it
// routes to; the Service wins because annotating it directly is the more
// deliberate statement of intent.
func dedupByURL(endpoints []Endpoint, defaultScheme string, templates *TemplateSet, warnings []string) ([]Endpoint, []string) {
	r := &Renderer{opts: RenderOptions{DefaultScheme: defaultScheme}}

	seen := make(map[string]Endpoint, len(endpoints))
	out := make([]Endpoint, 0, len(endpoints))

	for _, ep := range endpoints {
		url := buildURL(ep, r.resolveScheme(ep, templates))
		if prev, dup := seen[url]; dup {
			warnings = append(warnings, fmt.Sprintf(
				"%s %s: dropping endpoint %q, %s %s already checks %s",
				ep.Source, ep.SourceRef, ep.Name, prev.Source, prev.SourceRef, url))
			continue
		}
		seen[url] = ep
		out = append(out, ep)
	}
	return out, warnings
}

// Assemble merges a rendered endpoints list into a base configuration. The base
// is copied, so the caller's parsed base config can be reused across renders.
//
// Only the endpoints key is replaced. Everything else the operator wrote,
// including "${VAR}" placeholders that Gatus expands itself, is passed through
// byte for byte.
func Assemble(base Object, endpoints []Object) Object {
	out := deepCopyObject(base)

	// A YAML list must be []any to marshal as one.
	list := make([]any, len(endpoints))
	for i, ep := range endpoints {
		list[i] = ep
	}
	out[endpointsKey] = list
	return out
}

func stringValue(obj Object, key string) string {
	if v, ok := obj[key].(string); ok {
		return v
	}
	return ""
}

// SummariseWarnings folds a warning list into a short message for logging.
func SummariseWarnings(warnings []string) string {
	if len(warnings) == 0 {
		return ""
	}
	return strings.Join(warnings, "; ")
}
