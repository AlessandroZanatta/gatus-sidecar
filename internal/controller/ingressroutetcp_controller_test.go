package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func traefikSvc(ns, name string, svcType corev1.ServiceType, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    map[string]string{"app.kubernetes.io/name": "traefik"},
		},
		Spec: corev1.ServiceSpec{Type: svcType, Ports: ports},
	}
}

// Splitting internal and external traffic across two Traefik installations is a
// normal arrangement, and both have to be found.
func TestEntrypointPortsDiscoversEveryTraefikService(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		traefikSvc("traefik", "internal", corev1.ServiceTypeLoadBalancer,
			corev1.ServicePort{Name: "mqtt", Port: 1883}),
		traefikSvc("traefik", "external", corev1.ServiceTypeLoadBalancer,
			corev1.ServicePort{Name: "mqtt", Port: 8883}),
		// Not Traefik's: must not contribute ports.
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
			Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "mqtt", Port: 111}}}},
	).Build()

	r := &IngressRouteTCPReconciler{Client: c}
	got, err := r.entrypointPorts(context.Background())
	if err != nil {
		t.Fatalf("entrypointPorts: %v", err)
	}
	if len(got.Services) != 2 {
		t.Fatalf("found %d traefik services, want 2: %#v", len(got.Services), got.Services)
	}
	if _, err := got.Port("mqtt", ""); err == nil {
		t.Error("Port() = nil error, want the two installations reported as ambiguous")
	}
	if port, err := got.Port("mqtt", "traefik/external"); err != nil || port != 8883 {
		t.Errorf("Port(mqtt, traefik/external) = %d, %v, want 8883", port, err)
	}
}

// An explicit list is the way out when the label is missing or matches things it
// should not.
func TestEntrypointPortsHonoursTheConfiguredList(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		traefikSvc("traefik", "internal", corev1.ServiceTypeLoadBalancer,
			corev1.ServicePort{Name: "mqtt", Port: 1883}),
		traefikSvc("traefik", "external", corev1.ServiceTypeLoadBalancer,
			corev1.ServicePort{Name: "mqtt", Port: 8883}),
	).Build()

	r := &IngressRouteTCPReconciler{Client: c, TraefikServices: []string{"traefik/external"}}
	got, err := r.entrypointPorts(context.Background())
	if err != nil {
		t.Fatalf("entrypointPorts: %v", err)
	}
	// One installation in scope means no ambiguity to resolve.
	if port, err := got.Port("mqtt", ""); err != nil || port != 8883 {
		t.Errorf("Port(mqtt) = %d, %v, want 8883", port, err)
	}
}

// For a NodePort installation the reachable port is the node port; the service
// port is an address no client outside the cluster uses.
func TestEntrypointPortsUsesNodePortForNodePortServices(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		traefikSvc("traefik", "traefik", corev1.ServiceTypeNodePort,
			corev1.ServicePort{Name: "mqtt", Port: 8883, NodePort: 31883}),
	).Build()

	r := &IngressRouteTCPReconciler{Client: c}
	got, err := r.entrypointPorts(context.Background())
	if err != nil {
		t.Fatalf("entrypointPorts: %v", err)
	}
	if port, err := got.Port("mqtt", ""); err != nil || port != 31883 {
		t.Errorf("Port(mqtt) = %d, %v, want the node port 31883", port, err)
	}
}

// A missing configured Service must not fail the reconcile: the override flag
// still works, and the watch will pick the Service up when it appears.
func TestEntrypointPortsToleratesAMissingConfiguredService(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	r := &IngressRouteTCPReconciler{
		Client:          c,
		TraefikServices: []string{"traefik/gone"},
		EntrypointPorts: map[string]int32{"mqtt": 8883},
	}
	got, err := r.entrypointPorts(context.Background())
	if err != nil {
		t.Fatalf("entrypointPorts: %v", err)
	}
	if port, err := got.Port("mqtt", ""); err != nil || port != 8883 {
		t.Errorf("Port(mqtt) = %d, %v, want the override 8883", port, err)
	}
}

// A Traefik Service changing its ports changes every public address in the
// cluster, not just the routes in its own namespace.
func TestTraefikServiceEventReDerivesEveryRoute(t *testing.T) {
	r := &IngressRouteTCPReconciler{}

	if !r.isTraefikService(traefikSvc("traefik", "traefik", corev1.ServiceTypeLoadBalancer)) {
		t.Error("labelled Service not recognised as Traefik's")
	}
	plain := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"}}
	if r.isTraefikService(plain) {
		t.Error("unlabelled Service recognised as Traefik's")
	}

	configured := &IngressRouteTCPReconciler{TraefikServices: []string{"traefik/external"}}
	if !configured.isTraefikService(traefikSvc("traefik", "external", corev1.ServiceTypeLoadBalancer)) {
		t.Error("configured Service not recognised")
	}
	// With an explicit list, the label alone does not qualify.
	if configured.isTraefikService(traefikSvc("traefik", "other", corev1.ServiceTypeLoadBalancer)) {
		t.Error("unconfigured Service recognised despite an explicit list")
	}
}
