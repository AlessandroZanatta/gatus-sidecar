package discovery

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// DefaultEntrypointPorts are the two entrypoints every Traefik installation
// defines, used when nothing better is known. Anything else has to be discovered
// or configured, since only the deployment knows what it published.
var DefaultEntrypointPorts = map[string]int32{
	"web":       80,
	"websecure": 443,
}

// TraefikService is one Traefik installation's Service, reduced to what an
// external client needs: which port answers for which entrypoint.
//
// There can be several. Splitting internal and external traffic across two
// Traefik installations, each with its own Service and its own published ports,
// is a normal arrangement, and the same entrypoint name then means a different
// port depending on which one a route belongs to.
type TraefikService struct {
	Namespace string
	Name      string

	// Ports maps entrypoint name to the port clients connect to. The Traefik
	// chart names each Service port after the entrypoint it serves, which is
	// what makes this discoverable at all.
	Ports map[string]int32
}

// Ref is the namespace/name form used by the traefik-service annotation.
func (s TraefikService) Ref() string { return s.Namespace + "/" + s.Name }

// EntrypointPorts resolves an entrypoint name to the port to check.
type EntrypointPorts struct {
	// Overrides come from --entrypoint-port and win over everything: they exist
	// precisely for the installations discovery reads wrongly.
	Overrides map[string]int32

	// Services are the Traefik Services in scope, in the order they were found.
	Services []TraefikService
}

// ParseEntrypointPorts parses a "name=port,name=port" flag value.
func ParseEntrypointPorts(v string) (map[string]int32, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}

	out := map[string]int32{}
	for _, pair := range splitList(v) {
		name, port, ok := strings.Cut(pair, "=")
		name, port = strings.TrimSpace(name), strings.TrimSpace(port)
		if !ok || name == "" || port == "" {
			return nil, fmt.Errorf("entrypoint port %q is not name=port", pair)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("entrypoint port %q: %q is not a port number", pair, port)
		}
		out[name] = int32(n)
	}
	return out, nil
}

// Port resolves one entrypoint.
//
// prefer is the traefik-service annotation, naming which installation a route
// belongs to. It is only needed when several installations publish the same
// entrypoint name on different ports, since that is the one case where the
// answer is genuinely ambiguous and guessing would monitor the wrong address.
func (e EntrypointPorts) Port(entrypoint, prefer string) (int32, error) {
	if port, ok := e.Overrides[entrypoint]; ok {
		return port, nil
	}

	if prefer != "" {
		for _, svc := range e.Services {
			if svc.Ref() != prefer {
				continue
			}
			if port, ok := svc.Ports[entrypoint]; ok {
				return port, nil
			}
			return 0, fmt.Errorf("traefik service %s publishes no port named %q; "+
				"name the Service port after the entrypoint or set --entrypoint-port", prefer, entrypoint)
		}
		return 0, fmt.Errorf("no traefik service %s found", prefer)
	}

	// Several Services agreeing on a port is not ambiguity, so distinct values
	// are what matters, not distinct Services.
	ports := map[int32][]string{}
	for _, svc := range e.Services {
		if port, ok := svc.Ports[entrypoint]; ok {
			ports[port] = append(ports[port], svc.Ref())
		}
	}

	switch len(ports) {
	case 0:
		if port, ok := DefaultEntrypointPorts[entrypoint]; ok {
			return port, nil
		}
		return 0, fmt.Errorf("cannot tell which port entrypoint %q is published on: "+
			"no traefik service names a port %q; set --entrypoint-port %s=<port>",
			entrypoint, entrypoint, entrypoint)
	case 1:
		for port := range ports {
			return port, nil
		}
	}

	return 0, fmt.Errorf("entrypoint %q is published on %s by different traefik services; "+
		"pick one with the %s annotation or set --entrypoint-port %s=<port>",
		entrypoint, describePorts(ports), Key(AnnTraefikService), entrypoint)
}

// describePorts renders the ambiguity in a form that names the way out.
func describePorts(ports map[int32][]string) string {
	out := make([]string, 0, len(ports))
	for port, refs := range ports {
		sort.Strings(refs)
		out = append(out, fmt.Sprintf("%d (%s)", port, strings.Join(refs, ", ")))
	}
	sort.Strings(out)
	return strings.Join(out, " and ")
}
