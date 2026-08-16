package discovery

import (
	"strings"
	"testing"

	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
)

// mosquittoRoute is the shape this feature exists for: a TLS-terminated MQTT
// router on a non-standard entrypoint, whose public port only the Traefik
// Service knows.
const mosquittoRoute = `
apiVersion: traefik.io/v1alpha1
kind: IngressRouteTCP
metadata:
  name: mosquitto
  namespace: home-assistant
  annotations:
    gatus.kalexlab.xyz/enabled: "true"
spec:
  entryPoints:
    - mqtt
  routes:
    - match: HostSNI(` + "`mqtt.example.org`" + `)
      services:
        - name: mosquitto
          port: 1883
  tls:
    secretName: home-assistant-ingress-certificate-secret
`

func mqttPorts() EntrypointPorts {
	return EntrypointPorts{Services: []TraefikService{
		traefik("traefik", "traefik", map[string]int32{"web": 80, "websecure": 443, "mqtt": 8883}),
	}}
}

func mosquittoResolver() ServiceResolver {
	return staticResolver(map[string][]NamedPort{
		"home-assistant/mosquitto": {{Name: "mosquitto", Port: 1883}},
	})
}

func TestFromIngressRouteTCPEmitsInternalAndExternal(t *testing.T) {
	o := Defaults()
	o.IngressRouteTCPMode = ModeOptIn

	got, err := o.FromIngressRouteTCP(route(t, mosquittoRoute), "", mosquittoResolver(), mqttPorts())
	if err != nil {
		t.Fatalf("FromIngressRouteTCP: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d endpoints, want 2: %v", len(got), names(got))
	}

	eps := byName(got)

	external, ok := eps["Mosquitto (external)"]
	if !ok {
		t.Fatalf("no external endpoint in %v", names(got))
	}
	// The entrypoint port, discovered from the Traefik Service, and tls because
	// the router terminates TLS: checking the certificate is the point.
	if want := "tls://mqtt.example.org:8883"; external.URL != want {
		t.Errorf("external URL = %q, want %q", external.URL, want)
	}
	if external.Scheme != "tls" {
		t.Errorf("external Scheme = %q, want tls", external.Scheme)
	}

	internal, ok := eps["Mosquitto"]
	if !ok {
		t.Fatalf("no internal endpoint in %v", names(got))
	}
	if internal.Host != "mosquitto.home-assistant.svc.cluster.local" || internal.Port != 1883 {
		t.Errorf("internal = %s:%d, want mosquitto.home-assistant.svc.cluster.local:1883",
			internal.Host, internal.Port)
	}
	// The backend is a plain socket whatever Traefik terminates in front of it.
	if internal.Scheme != "tcp" {
		t.Errorf("internal Scheme = %q, want tcp", internal.Scheme)
	}

	for _, ep := range got {
		if ep.Source != config.SourceIngressRouteTCP {
			t.Errorf("%s Source = %s, want IngressRouteTCP", ep.Name, ep.Source)
		}
		if ep.Group != "Home assistant" {
			t.Errorf("%s Group = %q, want Home assistant", ep.Name, ep.Group)
		}
	}
}

// Without tls the router forwards the raw connection, so there is no
// certificate to check and tcp is the honest scheme.
func TestFromIngressRouteTCPPlaintextRouterUsesTCP(t *testing.T) {
	o := Defaults()
	manifest := strings.ReplaceAll(mosquittoRoute,
		"  tls:\n    secretName: home-assistant-ingress-certificate-secret\n", "")

	got, err := o.FromIngressRouteTCP(route(t, manifest), "", mosquittoResolver(), mqttPorts())
	if err != nil {
		t.Fatalf("FromIngressRouteTCP: %v", err)
	}

	external, ok := byName(got)["Mosquitto (external)"]
	if !ok {
		t.Fatalf("no external endpoint in %v", names(got))
	}
	if want := "tcp://mqtt.example.org:8883"; external.URL != want {
		t.Errorf("external URL = %q, want %q", external.URL, want)
	}
}

// A router on several entrypoints is reachable at several ports, and one being
// up says nothing about the other.
func TestFromIngressRouteTCPChecksEveryEntrypoint(t *testing.T) {
	o := Defaults()
	manifest := strings.ReplaceAll(mosquittoRoute,
		"  entryPoints:\n    - mqtt\n",
		"  entryPoints:\n    - mqtt\n    - mqtt-plain\n")

	ports := EntrypointPorts{Services: []TraefikService{
		traefik("traefik", "traefik", map[string]int32{"mqtt": 8883, "mqtt-plain": 1883}),
	}}

	got, err := o.FromIngressRouteTCP(route(t, manifest), "", mosquittoResolver(), ports)
	if err != nil {
		t.Fatalf("FromIngressRouteTCP: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d endpoints, want 3 (two external, one internal): %v", len(got), names(got))
	}

	eps := byName(got)
	for name, want := range map[string]string{
		"Mosquitto mqtt (external)":       "tls://mqtt.example.org:8883",
		"Mosquitto mqtt-plain (external)": "tls://mqtt.example.org:1883",
	} {
		ep, ok := eps[name]
		if !ok {
			t.Fatalf("no endpoint %q in %v", name, names(got))
		}
		if ep.URL != want {
			t.Errorf("%s URL = %q, want %q", name, ep.URL, want)
		}
	}
}

// Ambiguity across installations is reported rather than guessed, and the
// annotation resolves it.
func TestFromIngressRouteTCPAmbiguousEntrypoint(t *testing.T) {
	o := Defaults()
	ports := EntrypointPorts{Services: []TraefikService{
		traefik("traefik", "internal", map[string]int32{"mqtt": 1883}),
		traefik("traefik", "external", map[string]int32{"mqtt": 8883}),
	}}

	if _, err := o.FromIngressRouteTCP(route(t, mosquittoRoute), "", mosquittoResolver(), ports); err == nil {
		t.Fatal("FromIngressRouteTCP() = nil error, want the ambiguity reported")
	}

	annotated := strings.Replace(mosquittoRoute,
		`    gatus.kalexlab.xyz/enabled: "true"`,
		"    gatus.kalexlab.xyz/enabled: \"true\"\n    gatus.kalexlab.xyz/traefik-service: traefik/external", 1)

	got, err := o.FromIngressRouteTCP(route(t, annotated), "", mosquittoResolver(), ports)
	if err != nil {
		t.Fatalf("FromIngressRouteTCP with traefik-service: %v", err)
	}
	external, ok := byName(got)["Mosquitto (external)"]
	if !ok {
		t.Fatalf("no external endpoint in %v", names(got))
	}
	if want := "tls://mqtt.example.org:8883"; external.URL != want {
		t.Errorf("external URL = %q, want %q", external.URL, want)
	}
}

// HostSNI(`*`) matches whatever reaches the entrypoint, so there is no address
// to check from outside. The backend is still worth watching.
func TestFromIngressRouteTCPWildcardSNIYieldsOnlyInternal(t *testing.T) {
	o := Defaults()
	manifest := strings.Replace(mosquittoRoute, "HostSNI(`mqtt.example.org`)", "HostSNI(`*`)", 1)

	got, err := o.FromIngressRouteTCP(route(t, manifest), "", mosquittoResolver(), mqttPorts())
	if err != nil {
		t.Fatalf("FromIngressRouteTCP: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d endpoints, want 1 (internal only): %v", len(got), names(got))
	}
	if got[0].Host != "mosquitto.home-assistant.svc.cluster.local" {
		t.Errorf("Host = %q, want the backing service", got[0].Host)
	}
}

// The shared annotation contract applies here too.
func TestFromIngressRouteTCPHonoursEnabledAndExclude(t *testing.T) {
	o := Defaults()

	off := strings.Replace(mosquittoRoute, `"true"`, `"false"`, 1)
	if got, err := o.FromIngressRouteTCP(route(t, off), "", mosquittoResolver(), mqttPorts()); err != nil || len(got) != 0 {
		t.Errorf("enabled=false yielded %v (err %v), want nothing", names(got), err)
	}

	excluded := strings.Replace(mosquittoRoute,
		`    gatus.kalexlab.xyz/enabled: "true"`,
		"    gatus.kalexlab.xyz/enabled: \"true\"\n    gatus.kalexlab.xyz/exclude: mosquitto", 1)
	if got, err := o.FromIngressRouteTCP(route(t, excluded), "", mosquittoResolver(), mqttPorts()); err != nil || len(got) != 0 {
		t.Errorf("exclude yielded %v (err %v), want nothing", names(got), err)
	}
}

// An explicit url annotation is the escape hatch for anything discovery gets
// wrong, and must bypass entrypoint resolution entirely.
func TestFromIngressRouteTCPExplicitURLSkipsEntrypointLookup(t *testing.T) {
	o := Defaults()
	manifest := strings.Replace(mosquittoRoute,
		`    gatus.kalexlab.xyz/enabled: "true"`,
		"    gatus.kalexlab.xyz/enabled: \"true\"\n    gatus.kalexlab.xyz/url: tls://mqtt.example.org:9999", 1)

	// No Traefik Service at all: resolution would fail if it were consulted.
	got, err := o.FromIngressRouteTCP(route(t, manifest), "", mosquittoResolver(), EntrypointPorts{})
	if err != nil {
		t.Fatalf("FromIngressRouteTCP: %v", err)
	}
	if len(got) != 1 || got[0].URL != "tls://mqtt.example.org:9999" {
		t.Fatalf("got %v, want the annotated url alone", got)
	}
}
