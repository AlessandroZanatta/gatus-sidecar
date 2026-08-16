package config

// Source identifies which kind of Kubernetes object an endpoint was derived from.
type Source string

// Sources, ordered by dedup precedence: when two discovered endpoints resolve to
// the same URL, the one from the lower-priority-number source is kept. A Service
// annotated directly is a more deliberate statement of intent than an endpoint
// inferred from an IngressRoute that happens to point at it.
const (
	SourceService         Source = "Service"
	SourceIngressRoute    Source = "IngressRoute"
	SourceIngressRouteTCP Source = "IngressRouteTCP"
)

// Priority returns the dedup precedence of a source; lower wins.
func (s Source) Priority() int {
	switch s {
	case SourceService:
		return 0
	case SourceIngressRoute, SourceIngressRouteTCP:
		return 1
	default:
		return 100
	}
}

// Endpoint is one discovered monitoring target, still unresolved: it records
// which templates it wants rather than their contents. Templates are applied at
// render time, so editing an EndpointTemplate re-renders without the sidecar
// having to track which objects reference it.
type Endpoint struct {
	// Source and SourceRef say where this came from, for logs and dedup.
	Source    Source
	SourceRef string // "namespace/name"

	// Identity fields. These always win over template contents: a template
	// describes how to check something, the workload says what is being checked.
	Name  string
	Group string

	// URL, when set, was given explicitly and is used verbatim. Otherwise the
	// renderer assembles one from Host, Port and Path once the scheme is known.
	URL  string
	Host string
	Port int32
	Path string

	// Scheme is the scheme the workload asked for, or "" to infer it from the
	// templates that apply. It is resolved at render time rather than here,
	// because a template such as default-tcp is what implies the scheme, and
	// templates are deliberately not read during discovery.
	Scheme string

	// Templates replaces the automatic defaultFor selection when TemplatesSet is
	// true. The extra bool distinguishes "use the defaults" from an explicit
	// empty list meaning "use no templates at all".
	Templates    []string
	TemplatesSet bool
	// ExtraTemplates is appended after the selected templates, whether those
	// came from defaultFor or from Templates.
	ExtraTemplates []string

	// Patch is the workload's raw override fragment, merged last so it beats
	// both templates and identity fields.
	Patch Object
}

// GroupKey is the identity used to detect duplicate endpoint names. Gatus keys
// its storage on the group/name pair, so two endpoints sharing one would
// overwrite each other's history.
func (e Endpoint) GroupKey() [2]string { return [2]string{e.Group, e.Name} }
