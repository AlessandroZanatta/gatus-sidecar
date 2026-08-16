package discovery

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
)

// IngressRouteGVR is the Traefik CRD this sidecar watches.
//
// It is handled as unstructured data through a dynamic informer rather than with
// Traefik's typed API, so the sidecar does not depend on Traefik's Kubernetes
// types and the same code path can later accept Ingress or Gateway API objects.
var IngressRouteGVR = schema.GroupVersionResource{
	Group:    "traefik.io",
	Version:  "v1alpha1",
	Resource: "ingressroutes",
}

// IngressRouteGVK is the kind matching IngressRouteGVR.
var IngressRouteGVK = schema.GroupVersionKind{
	Group:   "traefik.io",
	Version: "v1alpha1",
	Kind:    "IngressRoute",
}

// ServiceResolver looks up a backing Service so its port can be resolved. It is
// injected rather than called directly so URL derivation stays testable without
// a cluster.
type ServiceResolver func(namespace, name string) (ports []NamedPort, err error)

// NamedPort is the part of a Service port this package needs.
type NamedPort struct {
	Name string
	Port int32
}

// backendRef is one entry of an IngressRoute's spec.routes[].services[].
type backendRef struct {
	name      string
	namespace string
	port      string // a number or a port name, as written

	// path is the prefix from the route this backend belongs to. It is applied
	// to the in-cluster check as well, since the backend generally serves the
	// same path the route matches. Where a middleware strips the prefix, the
	// path annotation overrides this.
	path string
}

// FromIngressRoute turns an IngressRoute into unresolved endpoints.
//
// Two endpoints are produced per route: the externally reachable address taken
// from the rule's Host matcher, and the in-cluster address of the Service the
// route forwards to. The external one exercises DNS, TLS, the ingress proxy and
// any middleware; the internal one isolates the workload itself. When they
// disagree, the difference is the useful signal.
func (o Options) FromIngressRoute(obj *unstructured.Unstructured, nsGroup string, resolve ServiceResolver) ([]config.Endpoint, error) {
	if !enabled(o.IngressRouteMode, obj.GetAnnotations()) {
		return nil, nil
	}
	if excluded(obj.GetName(), obj.GetAnnotations()) {
		return nil, nil
	}

	specs, err := specsFromAnnotations(obj.GetAnnotations())
	if err != nil {
		return nil, err
	}

	ref := obj.GetNamespace() + "/" + obj.GetName()
	ctx := specContext{
		source:    config.SourceIngressRoute,
		ref:       ref,
		fallback:  obj.GetName(),
		namespace: obj.GetNamespace(),
		nsGroup:   nsGroup,
	}

	// An explicit url annotation means the author has already said exactly what
	// to check, so no derivation from the route is wanted.
	if len(specs) > 0 && specs[0].url != "" {
		return o.endpointsFromSpecs(specs, ctx)
	}
	// The list-valued endpoints annotation is likewise an explicit description.
	if _, listed := rawAnnotation(obj.GetAnnotations(), AnnEndpoints); listed {
		return o.endpointsFromSpecs(specs, ctx)
	}

	base := specs[0]
	hosts, backends, err := routeTargets(obj)
	if err != nil {
		return nil, err
	}

	var out []config.Endpoint
	out = append(out, o.externalEndpoints(base, ctx, hosts)...)

	internal, err := o.internalEndpoints(base, ctx, backends, resolve)
	if err != nil {
		return nil, err
	}
	return append(out, internal...), nil
}

// hostTarget is one externally reachable address from a route's match rule.
type hostTarget struct {
	host string
	path string
}

// routeTargets reads the parts of an IngressRoute's spec that describe addresses.
func routeTargets(obj *unstructured.Unstructured) ([]hostTarget, []backendRef, error) {
	routes, found, err := unstructured.NestedSlice(obj.Object, "spec", "routes")
	if err != nil {
		return nil, nil, fmt.Errorf("read spec.routes: %w", err)
	}
	if !found {
		return nil, nil, nil
	}

	var (
		hosts    []hostTarget
		backends []backendRef
		seenHost = map[string]bool{}
		seenSvc  = map[string]bool{}
	)

	for i, raw := range routes {
		route, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		match, _, _ := unstructured.NestedString(route, "match")
		parsed, err := parseRule(match)
		if err != nil {
			return nil, nil, fmt.Errorf("route %d: %w", i, err)
		}
		for _, host := range parsed.Hosts {
			key := host + parsed.Path
			if seenHost[key] {
				continue
			}
			seenHost[key] = true
			hosts = append(hosts, hostTarget{host: host, path: parsed.Path})
		}

		services, _, _ := unstructured.NestedSlice(route, "services")
		for _, rawSvc := range services {
			svc, ok := rawSvc.(map[string]any)
			if !ok {
				continue
			}
			name, _, _ := unstructured.NestedString(svc, "name")
			if name == "" {
				continue
			}
			ns, _, _ := unstructured.NestedString(svc, "namespace")
			if ns == "" {
				ns = obj.GetNamespace()
			}

			// A backend port is written as either a number or a port name, so
			// it arrives as an int64 or a string depending on which.
			port := ""
			if v, found, _ := unstructured.NestedString(svc, "port"); found {
				port = v
			} else if v, found, _ := unstructured.NestedInt64(svc, "port"); found {
				port = fmt.Sprintf("%d", v)
			}

			key := ns + "/" + name + ":" + port
			if seenSvc[key] {
				continue
			}
			seenSvc[key] = true
			backends = append(backends, backendRef{
				name: name, namespace: ns, port: port, path: parsed.Path,
			})
		}
	}

	return hosts, backends, nil
}

// externalEndpoints builds the endpoints reachable from outside the cluster.
func (o Options) externalEndpoints(base spec, ctx specContext, hosts []hostTarget) []config.Endpoint {
	out := make([]config.Endpoint, 0, len(hosts))

	for _, h := range hosts {
		s := base
		// An ingress is TLS-terminated in practice, and checking the plaintext
		// port would exercise a redirect rather than the service.
		if s.scheme == "" {
			s.scheme = "https"
		}
		if s.path == "" {
			s.path = h.path
		}
		s.url = s.scheme + "://" + h.host + normalisePath(s.path)

		ep := o.baseEndpoint(s, ctx)
		ep.Name = o.externalName(base, ctx, h.host, len(hosts))
		ep.URL = s.url
		ep.Scheme = s.scheme
		out = append(out, ep)
	}

	return out
}

// internalEndpoints builds the in-cluster endpoints for a route's backends.
func (o Options) internalEndpoints(base spec, ctx specContext, backends []backendRef, resolve ServiceResolver) ([]config.Endpoint, error) {
	out := make([]config.Endpoint, 0, len(backends))

	for _, b := range backends {
		var ports []NamedPort
		if resolve != nil {
			var err error
			if ports, err = resolve(b.namespace, b.name); err != nil {
				return nil, fmt.Errorf("resolve backend service %s/%s: %w", b.namespace, b.name, err)
			}
		}

		want := b.port
		if want == "" {
			want = base.port
		}
		port, err := resolvePort(want, ports)
		if err != nil {
			return nil, fmt.Errorf("backend service %s/%s: %w", b.namespace, b.name, err)
		}

		// An explicit path annotation wins; otherwise the route's own prefix is
		// what the backend is expected to serve.
		path := base.path
		if path == "" {
			path = b.path
		}

		ep := o.baseEndpoint(base, ctx)
		ep.Name = o.internalName(base, ctx, b, len(backends))
		ep.Host = fmt.Sprintf("%s.%s.svc.%s", b.name, b.namespace, o.ClusterDomain)
		ep.Port = port
		ep.Path = normalisePath(path)
		out = append(out, ep)
	}

	return out, nil
}

// baseEndpoint fills in the parts of an endpoint that do not depend on which
// address it points at.
func (o Options) baseEndpoint(s spec, ctx specContext) config.Endpoint {
	return config.Endpoint{
		Source:         ctx.source,
		SourceRef:      ctx.ref,
		Group:          o.resolveGroup(s, ctx),
		Scheme:         strings.ToLower(s.scheme),
		Templates:      s.templates,
		TemplatesSet:   s.templatesSet,
		ExtraTemplates: s.extraTemplates,
		Patch:          s.patch,
	}
}

// externalName names an outside-the-cluster endpoint. The host is folded into
// the name only when a route serves several, since otherwise every endpoint from
// that route would collide.
func (o Options) externalName(base spec, ctx specContext, host string, total int) string {
	name := base.name
	if name == "" {
		name = DisplayName(ctx.fallback)
	}
	if total > 1 {
		name += " " + host
	}
	return name + o.ExternalSuffix
}

// internalName names an in-cluster endpoint, falling back to the backend's own
// name when a route fans out to several services.
func (o Options) internalName(base spec, ctx specContext, b backendRef, total int) string {
	if base.name != "" && total == 1 {
		return base.name
	}
	if total > 1 {
		return DisplayName(b.name)
	}
	return DisplayName(ctx.fallback)
}

// endpointsFromSpecs is the path taken when annotations describe the endpoints
// outright, bypassing derivation from the route.
func (o Options) endpointsFromSpecs(specs []spec, ctx specContext) ([]config.Endpoint, error) {
	out := make([]config.Endpoint, 0, len(specs))
	for _, s := range specs {
		ep, err := o.endpointFromSpec(s, ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, nil
}
