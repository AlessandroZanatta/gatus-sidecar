// Package config implements template resolution, endpoint merging and rendering
// of the final Gatus configuration file.
package config

// Object is a decoded YAML/JSON mapping. The sidecar never decodes into Gatus's
// own structs: everything stays generic so fields the sidecar has never heard of
// (including ones added by future Gatus versions) survive a round trip untouched.
type Object = map[string]any

// Merge deep-merges src into a copy of dst and returns the result. dst is not
// modified.
//
// Rules:
//   - maps are merged recursively, key by key
//   - every other value, including lists, replaces the value it merges over
//
// List-replace rather than list-append is deliberate. It is what lets a workload
// override a template's "conditions" outright instead of accumulating the
// template's conditions plus its own, which is almost never what is wanted.
func Merge(dst, src Object) Object {
	out := deepCopyObject(dst)
	mergeInto(out, src)
	return out
}

// MergeAll folds layers left to right; later layers win over earlier ones.
// Nil layers are skipped, so callers can pass optional layers without guarding.
func MergeAll(layers ...Object) Object {
	out := Object{}
	for _, layer := range layers {
		if layer == nil {
			continue
		}
		mergeInto(out, layer)
	}
	return out
}

// mergeInto merges src into dst in place. Values taken from src are deep-copied,
// so the result never aliases src and later mutation of one cannot affect the other.
func mergeInto(dst, src Object) {
	for k, srcVal := range src {
		dstVal, present := dst[k]
		if !present {
			dst[k] = deepCopyValue(srcVal)
			continue
		}

		srcMap, srcIsMap := asObject(srcVal)
		dstMap, dstIsMap := asObject(dstVal)
		if srcIsMap && dstIsMap {
			merged := deepCopyObject(dstMap)
			mergeInto(merged, srcMap)
			dst[k] = merged
			continue
		}

		// Scalars, lists, and type mismatches: src wins outright.
		dst[k] = deepCopyValue(srcVal)
	}
}

// asObject normalises the two mapping shapes a YAML decoder can produce.
// gopkg.in/yaml.v3 yields map[string]any for string-keyed maps but falls back to
// map[any]any when a key is not a string; JSON always yields map[string]any.
func asObject(v any) (Object, bool) {
	switch m := v.(type) {
	case Object:
		return m, true
	case map[any]any:
		out := make(Object, len(m))
		for k, val := range m {
			ks, ok := k.(string)
			if !ok {
				// A non-string key cannot appear in a Gatus config; treating the
				// whole value as opaque is safer than dropping keys silently.
				return nil, false
			}
			out[ks] = val
		}
		return out, true
	default:
		return nil, false
	}
}

func deepCopyObject(in Object) Object {
	out := make(Object, len(in))
	for k, v := range in {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case Object:
		return deepCopyObject(t)
	case map[any]any:
		if obj, ok := asObject(t); ok {
			return deepCopyObject(obj)
		}
		out := make(map[any]any, len(t))
		for k, val := range t {
			out[k] = deepCopyValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = deepCopyValue(val)
		}
		return out
	default:
		// Scalars are immutable in Go's YAML/JSON value model.
		return v
	}
}
