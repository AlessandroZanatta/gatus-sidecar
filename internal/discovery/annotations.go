// Package discovery turns Kubernetes objects into unresolved Gatus endpoints by
// reading annotations and deriving URLs.
package discovery

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
)

// AnnotationPrefix is the prefix every annotation this sidecar reads shares.
const AnnotationPrefix = "gatus.kalexlab.xyz/"

// Annotation suffixes, appended to the prefix.
const (
	AnnEnabled        = "enabled"
	AnnExclude        = "exclude"
	AnnTraefikService = "traefik-service"
	AnnName           = "name"
	AnnGroup          = "group"
	AnnTemplate       = "template"
	AnnTemplateExtra  = "template-extra"
	AnnScheme         = "scheme"
	AnnPort           = "port"
	AnnPath           = "path"
	AnnURL            = "url"
	AnnEndpoint       = "endpoint"
	AnnEndpoints      = "endpoints"
)

// Key returns the full annotation key for a suffix.
func Key(suffix string) string { return AnnotationPrefix + suffix }

// Annotation returns the annotation value and whether it was present. Values are
// trimmed, since annotations written through YAML block scalars routinely carry
// a trailing newline.
func Annotation(annotations map[string]string, suffix string) (string, bool) {
	v, ok := annotations[Key(suffix)]
	if !ok {
		return "", false
	}
	return strings.TrimSpace(v), true
}

// rawAnnotation returns an annotation value without trimming, for the
// YAML-valued annotations where leading indentation is significant.
func rawAnnotation(annotations map[string]string, suffix string) (string, bool) {
	v, ok := annotations[Key(suffix)]
	return v, ok
}

// spec is the parsed, source-agnostic description of one endpoint, before URL
// derivation. It is produced both from an object's shortcut annotations and from
// each item of the list-valued "endpoints" annotation, so the two paths cannot
// drift apart.
type spec struct {
	name           string
	group          string
	groupSet       bool // distinguishes "group: " (explicitly none) from absent
	url            string
	scheme         string
	port           string
	path           string
	templates      []string
	templatesSet   bool
	extraTemplates []string
	patch          config.Object
}

// controlKeys are spec fields that describe how to build an endpoint rather than
// fields Gatus itself understands. They are consumed during parsing and must not
// leak into the rendered output.
var controlKeys = map[string]bool{
	AnnScheme:         true,
	AnnPort:           true,
	AnnPath:           true,
	AnnTemplate:       true,
	AnnTemplateExtra:  true,
	AnnEnabled:        true,
	AnnExclude:        true,
	AnnTraefikService: true,
}

// identityKeys are consumed into spec fields but are also real Gatus fields, so
// they are removed from the patch to keep a single source of truth per key.
var identityKeys = map[string]bool{
	AnnName:  true,
	AnnGroup: true,
	AnnURL:   true,
}

// specsFromAnnotations parses an object's annotations into one or more endpoint
// specs.
//
// When the list-valued "endpoints" annotation is present, each of its items is
// overlaid on the object's shortcut annotations. That way a Service can set
// group and template once and still describe several endpoints, instead of
// repeating shared settings in every item.
func specsFromAnnotations(annotations map[string]string) ([]spec, error) {
	base, err := parseShortcuts(annotations)
	if err != nil {
		return nil, err
	}

	raw, ok := rawAnnotation(annotations, AnnEndpoints)
	if !ok || strings.TrimSpace(raw) == "" {
		return []spec{base}, nil
	}

	items, err := parseEndpointList(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", Key(AnnEndpoints), err)
	}

	specs := make([]spec, 0, len(items))
	for _, item := range items {
		specs = append(specs, item.inherit(base))
	}
	return specs, nil
}

// inherit fills unset fields of an "endpoints" list item from the object's
// shortcut annotations.
//
// Name is deliberately not inherited: several endpoints sharing one object must
// not all end up with the same name, since Gatus keys its stored history on the
// group and name pair.
func (s spec) inherit(base spec) spec {
	if s.url == "" {
		s.url = base.url
	}
	if s.scheme == "" {
		s.scheme = base.scheme
	}
	if s.port == "" {
		s.port = base.port
	}
	if s.path == "" {
		s.path = base.path
	}
	if !s.groupSet && base.groupSet {
		s.group, s.groupSet = base.group, true
	}
	if !s.templatesSet {
		s.templates, s.templatesSet = base.templates, base.templatesSet
	}
	if s.extraTemplates == nil {
		s.extraTemplates = base.extraTemplates
	}
	// The item's own keys win over the object-level patch, key by key.
	if base.patch != nil {
		s.patch = config.Merge(base.patch, s.patch)
	}
	return s
}

// parseShortcuts reads the one-endpoint-per-object form.
func parseShortcuts(annotations map[string]string) (spec, error) {
	var s spec

	s.name, _ = Annotation(annotations, AnnName)
	s.group, s.groupSet = Annotation(annotations, AnnGroup)
	s.url, _ = Annotation(annotations, AnnURL)
	s.scheme, _ = Annotation(annotations, AnnScheme)
	s.port, _ = Annotation(annotations, AnnPort)
	s.path, _ = Annotation(annotations, AnnPath)

	if v, ok := Annotation(annotations, AnnTemplate); ok {
		s.templates = splitList(v)
		s.templatesSet = true
	}
	if v, ok := Annotation(annotations, AnnTemplateExtra); ok {
		s.extraTemplates = splitList(v)
	}

	if raw, ok := rawAnnotation(annotations, AnnEndpoint); ok && strings.TrimSpace(raw) != "" {
		patch, err := parseObject(raw)
		if err != nil {
			return spec{}, fmt.Errorf("%s: %w", Key(AnnEndpoint), err)
		}
		s.patch = patch
	}

	return s, nil
}

// parseEndpointList parses the list-valued form, used when one object exposes
// several things worth checking (a Service with both a health port and a UI port).
func parseEndpointList(raw string) ([]spec, error) {
	var items []any
	if err := yaml.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("not a YAML list: %w", err)
	}
	if len(items) == 0 {
		return nil, nil
	}

	specs := make([]spec, 0, len(items))
	for i, item := range items {
		obj, ok := toObject(item)
		if !ok {
			return nil, fmt.Errorf("item %d is not a mapping", i)
		}
		s, err := specFromObject(obj)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		specs = append(specs, s)
	}
	return specs, nil
}

// specFromObject splits a mapping into control fields, identity fields, and the
// remaining Gatus fields which become the patch.
func specFromObject(obj config.Object) (spec, error) {
	var s spec
	patch := make(config.Object, len(obj))

	for key, val := range obj {
		// Control and identity keys are read into spec fields below; everything
		// else is a Gatus field and passes straight through as the patch.
		if controlKeys[key] || identityKeys[key] {
			continue
		}
		patch[key] = val
	}

	var err error
	if s.name, err = stringField(obj, AnnName); err != nil {
		return spec{}, err
	}
	if s.url, err = stringField(obj, AnnURL); err != nil {
		return spec{}, err
	}
	if s.scheme, err = stringField(obj, AnnScheme); err != nil {
		return spec{}, err
	}
	if s.path, err = stringField(obj, AnnPath); err != nil {
		return spec{}, err
	}
	// Ports are commonly written unquoted, so accept a YAML integer too.
	if s.port, err = scalarField(obj, AnnPort); err != nil {
		return spec{}, err
	}
	if raw, ok := obj[AnnGroup]; ok {
		s.groupSet = true
		if raw != nil {
			if s.group, err = stringField(obj, AnnGroup); err != nil {
				return spec{}, err
			}
		}
	}
	if s.templates, s.templatesSet, err = listField(obj, AnnTemplate); err != nil {
		return spec{}, err
	}
	if s.extraTemplates, _, err = listField(obj, AnnTemplateExtra); err != nil {
		return spec{}, err
	}

	if len(patch) > 0 {
		s.patch = patch
	}
	return s, nil
}

// stringField reads an optional string-valued key.
func stringField(obj config.Object, key string) (string, error) {
	v, ok := obj[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%q must be a string, got %T", key, v)
	}
	return strings.TrimSpace(s), nil
}

// scalarField reads a key that may be written as a string or a number.
func scalarField(obj config.Object, key string) (string, error) {
	v, ok := obj[key]
	if !ok || v == nil {
		return "", nil
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t), nil
	case int:
		return fmt.Sprintf("%d", t), nil
	case int64:
		return fmt.Sprintf("%d", t), nil
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t)), nil
		}
		return "", fmt.Errorf("%q must be a whole number, got %v", key, t)
	default:
		return "", fmt.Errorf("%q must be a string or number, got %T", key, v)
	}
}

// listField reads a key written either as a YAML list or as a comma-separated
// string, so the same syntax works in an annotation value and in a list item.
func listField(obj config.Object, key string) ([]string, bool, error) {
	v, ok := obj[key]
	if !ok || v == nil {
		return nil, false, nil
	}
	switch t := v.(type) {
	case string:
		return splitList(t), true, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				return nil, false, fmt.Errorf("%q entries must be strings, got %T", key, item)
			}
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, true, nil
	default:
		return nil, false, fmt.Errorf("%q must be a string or list, got %T", key, v)
	}
}

// parseObject decodes a YAML mapping, rejecting other shapes so a malformed
// annotation fails loudly instead of silently contributing nothing.
func parseObject(raw string) (config.Object, error) {
	var v any
	if err := yaml.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	obj, ok := toObject(v)
	if !ok {
		return nil, fmt.Errorf("expected a mapping, got %T", v)
	}
	return obj, nil
}

// toObject normalises the map shapes a YAML decoder can produce.
func toObject(v any) (config.Object, bool) {
	switch m := v.(type) {
	case config.Object:
		return m, true
	case map[any]any:
		out := make(config.Object, len(m))
		for k, val := range m {
			ks, ok := k.(string)
			if !ok {
				return nil, false
			}
			out[ks] = val
		}
		return out, true
	default:
		return nil, false
	}
}

// splitList parses a comma- or newline-separated annotation value, dropping
// empty entries so a trailing comma is harmless.
func splitList(v string) []string {
	// Newlines separate as well as commas, so a long list can be written as a
	// YAML block scalar instead of one unreadable line.
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
