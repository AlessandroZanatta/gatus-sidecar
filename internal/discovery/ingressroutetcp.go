package discovery

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
)

// IngressRouteTCPGVR is the Traefik CRD for TCP routers.
var IngressRouteTCPGVR = schema.GroupVersionResource{
	Group:    "traefik.io",
	Version:  "v1alpha1",
	Resource: "ingressroutetcps",
}

// IngressRouteTCPGVK is the kind matching IngressRouteTCPGVR.
var IngressRouteTCPGVK = schema.GroupVersionKind{
	Group:   "traefik.io",
	Version: "v1alpha1",
	Kind:    "IngressRouteTCP",
}

// FromIngressRouteTCP turns an IngressRouteTCP into unresolved endpoints.
//
// Like its HTTP counterpart it yields the public address and the in-cluster one,
// but the public address takes more work: a TCP router names an entrypoint
// rather than a port, and only the Traefik Service knows which port that
// entrypoint was published on. Resolving it is what ports is for.
//
// The scheme follows the router's TLS setting. A router with tls terminates TLS
// at Traefik, so tls:// checks the certificate the client is actually served,
// which is worth more than a bare connect on a port that is up regardless.
func (o Options) FromIngressRouteTCP(
	obj *unstructured.Unstructured,
	nsGroup string,
	resolve ServiceResolver,
	ports EntrypointPorts,
) ([]config.Endpoint, error) {
	if !enabled(o.IngressRouteTCPMode, obj.GetAnnotations()) {
		return nil, nil
	}
	if excluded(obj.GetName(), obj.GetAnnotations()) {
		return nil, nil
	}

	specs, err := specsFromAnnotations(obj.GetAnnotations())
	if err != nil {
		return nil, err
	}

	ctx := specContext{
		source:    config.SourceIngressRouteTCP,
		ref:       obj.GetNamespace() + "/" + obj.GetName(),
		fallback:  obj.GetName(),
		namespace: obj.GetNamespace(),
		nsGroup:   nsGroup,
	}

	// Annotations that describe the endpoints outright skip all derivation, the
	// same way they do for an IngressRoute.
	if len(specs) > 0 && specs[0].url != "" {
		return o.endpointsFromSpecs(specs, ctx)
	}
	if _, listed := rawAnnotation(obj.GetAnnotations(), AnnEndpoints); listed {
		return o.endpointsFromSpecs(specs, ctx)
	}

	base := specs[0]
	if base.scheme == "" {
		base.scheme = "tcp"
		if _, tls, _ := unstructured.NestedMap(obj.Object, "spec", "tls"); tls {
			base.scheme = "tls"
		}
	}

	hosts, backends, err := routeTargets(obj)
	if err != nil {
		return nil, err
	}

	out, err := o.externalTCPEndpoints(base, ctx, obj, hosts, ports)
	if err != nil {
		return nil, err
	}

	// A TCP backend is a plain socket wherever it is reached from, so the
	// in-cluster check is a connect regardless of what Traefik terminates.
	internalBase := base
	internalBase.scheme = "tcp"
	internal, err := o.internalEndpoints(internalBase, ctx, backends, resolve)
	if err != nil {
		return nil, err
	}
	return append(out, internal...), nil
}

// externalTCPEndpoints builds one endpoint per host and entrypoint.
//
// A router listening on several entrypoints is reachable at several ports, and
// they are not interchangeable: one of them being up says nothing about the
// other. Each is checked, and the entrypoint is folded into the name when there
// is more than one, since Gatus keys history on the name.
func (o Options) externalTCPEndpoints(
	base spec,
	ctx specContext,
	obj *unstructured.Unstructured,
	hosts []hostTarget,
	ports EntrypointPorts,
) ([]config.Endpoint, error) {
	entrypoints, _, err := unstructured.NestedStringSlice(obj.Object, "spec", "entryPoints")
	if err != nil {
		return nil, fmt.Errorf("read spec.entryPoints: %w", err)
	}
	if len(hosts) == 0 || len(entrypoints) == 0 {
		// A router with no HostSNI serves whatever reaches it, so there is no
		// address to check from outside. The in-cluster endpoint still stands.
		return nil, nil
	}

	prefer, _ := Annotation(obj.GetAnnotations(), AnnTraefikService)

	out := make([]config.Endpoint, 0, len(hosts)*len(entrypoints))
	for _, entrypoint := range entrypoints {
		port, err := ports.Port(entrypoint, prefer)
		if err != nil {
			return nil, fmt.Errorf("entrypoint %q: %w", entrypoint, err)
		}

		for _, h := range hosts {
			s := base
			s.url = fmt.Sprintf("%s://%s:%d", s.scheme, h.host, port)

			ep := o.baseEndpoint(s, ctx)
			ep.Name = o.externalName(base, ctx, h.host, len(hosts))
			if len(entrypoints) > 1 {
				ep.Name = o.externalTCPName(base, ctx, h.host, len(hosts), entrypoint)
			}
			ep.URL = s.url
			ep.Scheme = strings.ToLower(s.scheme)
			out = append(out, ep)
		}
	}

	return out, nil
}

// externalTCPName names an endpoint whose router listens on several
// entrypoints, so that the two ports do not collide on one name.
func (o Options) externalTCPName(base spec, ctx specContext, host string, hosts int, entrypoint string) string {
	name := base.name
	if name == "" {
		name = DisplayName(ctx.fallback)
	}
	if hosts > 1 {
		name += " " + host
	}
	return name + " " + entrypoint + o.ExternalSuffix
}
