package discovery

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
)

// Mode controls whether an object kind is monitored by default.
type Mode string

const (
	// ModeOptIn only monitors objects that explicitly set enabled=true.
	ModeOptIn Mode = "opt-in"
	// ModeAuto monitors every object unless it sets enabled=false.
	ModeAuto Mode = "auto"
	// ModeDisabled ignores the object kind entirely.
	ModeDisabled Mode = "disabled"
)

// ParseMode validates a discovery mode flag value.
func ParseMode(v string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(v))) {
	case ModeOptIn:
		return ModeOptIn, nil
	case ModeAuto:
		return ModeAuto, nil
	case ModeDisabled:
		return ModeDisabled, nil
	default:
		return "", fmt.Errorf("invalid discovery mode %q: want opt-in, auto or disabled", v)
	}
}

// Options configures how objects are turned into endpoints.
type Options struct {
	Keys Keys

	// ServiceMode and IngressRouteMode are independent so, for example, Services
	// can be swept automatically while IngressRoutes stay opt-in.
	ServiceMode      Mode
	IngressRouteMode Mode

	// GroupFromNamespace derives a missing group from the object's namespace.
	// Disable it to leave endpoints ungrouped unless annotated.
	GroupFromNamespace bool

	// ClusterDomain is the cluster's DNS suffix, for building service URLs.
	ClusterDomain string

	// ExternalSuffix distinguishes the externally-reachable endpoint generated
	// from an IngressRoute from the internal one.
	ExternalSuffix string

	// DefaultScheme applies when neither the object nor its templates say.
	DefaultScheme string
}

// Defaults returns Options matching the documented flag defaults.
func Defaults() Options {
	return Options{
		Keys:               NewKeys(DefaultAnnotationPrefix),
		ServiceMode:        ModeOptIn,
		IngressRouteMode:   ModeOptIn,
		GroupFromNamespace: true,
		ClusterDomain:      "cluster.local",
		ExternalSuffix:     " (external)",
		DefaultScheme:      "http",
	}
}

// enabled reports whether an object opts in under the given mode.
func enabled(k Keys, mode Mode, annotations map[string]string) bool {
	switch mode {
	case ModeDisabled:
		return false
	case ModeAuto:
		v, ok := k.Get(annotations, AnnEnabled)
		if !ok {
			return true
		}
		return !isFalse(v)
	default: // ModeOptIn
		v, ok := k.Get(annotations, AnnEnabled)
		return ok && isTrue(v)
	}
}

func isTrue(v string) bool {
	b, err := strconv.ParseBool(strings.ToLower(v))
	return err == nil && b
}

func isFalse(v string) bool {
	b, err := strconv.ParseBool(strings.ToLower(v))
	return err == nil && !b
}

// FromService turns a Service into zero or more unresolved endpoints.
//
// A nil error with no endpoints means the Service simply did not opt in. An
// error means it asked to be monitored but could not be understood, which is
// worth surfacing rather than silently dropping.
func (o Options) FromService(svc *corev1.Service, nsGroup string) ([]config.Endpoint, error) {
	if !enabled(o.Keys, o.ServiceMode, svc.Annotations) {
		return nil, nil
	}
	if svc.Spec.Type == corev1.ServiceTypeExternalName {
		return nil, fmt.Errorf("ExternalName services have no cluster IP to check; set %s explicitly", o.Keys.Key(AnnURL))
	}

	specs, err := specsFromAnnotations(o.Keys, svc.Annotations)
	if err != nil {
		return nil, err
	}

	ref := svc.Namespace + "/" + svc.Name
	out := make([]config.Endpoint, 0, len(specs))
	for _, s := range specs {
		ep, err := o.endpointFromSpec(s, specContext{
			source:    config.SourceService,
			ref:       ref,
			host:      fmt.Sprintf("%s.%s.svc.%s", svc.Name, svc.Namespace, o.ClusterDomain),
			fallback:  svc.Name,
			namespace: svc.Namespace,
			nsGroup:   nsGroup,
			ports:     toNamedPorts(svc.Spec.Ports),
		})
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, nil
}

// specContext carries the per-object facts needed to turn a spec into an endpoint.
type specContext struct {
	source    config.Source
	ref       string
	host      string // DNS name to build a URL from, when the spec gives no url
	fallback  string // object name, used to derive a display name
	namespace string
	nsGroup   string // group annotated on the namespace, if any
	ports     []NamedPort
}

// endpointFromSpec applies naming, grouping, scheme and URL derivation.
func (o Options) endpointFromSpec(s spec, ctx specContext) (config.Endpoint, error) {
	ep := config.Endpoint{
		Source:         ctx.source,
		SourceRef:      ctx.ref,
		Name:           s.name,
		Templates:      s.templates,
		TemplatesSet:   s.templatesSet,
		ExtraTemplates: s.extraTemplates,
		Patch:          s.patch,
		Scheme:         strings.ToLower(s.scheme),
	}
	if ep.Name == "" {
		ep.Name = DisplayName(ctx.fallback)
	}
	ep.Group = o.resolveGroup(s, ctx)

	// An explicit url is authoritative, including the scheme it carries.
	if s.url != "" {
		if scheme, _, found := strings.Cut(s.url, "://"); found && ep.Scheme == "" {
			ep.Scheme = strings.ToLower(scheme)
		}
		ep.URL = s.url
		return ep, nil
	}

	// Port selection has to happen here, since it needs the Service's ports.
	// Assembling the URL does not: the scheme may come from a template, which
	// is only known at render time.
	port, err := resolvePort(s.port, ctx.ports)
	if err != nil {
		return config.Endpoint{}, fmt.Errorf("%s: %w", ep.Name, err)
	}
	ep.Host = ctx.host
	ep.Port = port
	ep.Path = normalisePath(s.path)
	return ep, nil
}

// resolveGroup applies the group precedence: the spec's own value, then the
// namespace annotation, then the namespace name. An explicitly empty group is
// honoured, because some endpoints belong at the top level of the status page.
func (o Options) resolveGroup(s spec, ctx specContext) string {
	if s.groupSet {
		return s.group
	}
	if ctx.nsGroup != "" {
		return ctx.nsGroup
	}
	if o.GroupFromNamespace {
		return DisplayName(ctx.namespace)
	}
	return ""
}

// toNamedPorts adapts a Service's ports to the shape shared with IngressRoute
// backend resolution.
func toNamedPorts(ports []corev1.ServicePort) []NamedPort {
	out := make([]NamedPort, len(ports))
	for i, p := range ports {
		out[i] = NamedPort{Name: p.Name, Port: p.Port}
	}
	return out
}

// resolvePort picks the port to check. A single-port Service needs no
// annotation; a multi-port one does, because guessing would silently monitor the
// wrong thing.
func resolvePort(want string, ports []NamedPort) (int32, error) {
	if want != "" {
		if n, err := strconv.Atoi(want); err == nil {
			if n < 1 || n > 65535 {
				return 0, fmt.Errorf("port %d out of range", n)
			}
			return int32(n), nil
		}
		for _, p := range ports {
			if p.Name == want {
				return p.Port, nil
			}
		}
		return 0, fmt.Errorf("no port named %q on this service", want)
	}

	switch len(ports) {
	case 0:
		return 0, fmt.Errorf("service exposes no ports")
	case 1:
		return ports[0].Port, nil
	default:
		names := make([]string, 0, len(ports))
		for _, p := range ports {
			names = append(names, fmt.Sprintf("%s(%d)", p.Name, p.Port))
		}
		return 0, fmt.Errorf("service exposes %d ports [%s]; pick one with the port annotation",
			len(ports), strings.Join(names, " "))
	}
}

// normalisePath makes annotating either "health" or "/health" work, and treats a
// bare slash as no path so URLs do not pick up a pointless trailing separator.
func normalisePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || path == "/" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}
