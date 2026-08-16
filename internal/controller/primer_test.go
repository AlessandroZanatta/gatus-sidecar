package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/AlessandroZanatta/gatus-sidecar/internal/discovery"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/registry"
)

func annotatedService(ns, name string, annotations map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: annotations},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080}}},
	}
}

func enabledAnnotations() map[string]string {
	return map[string]string{discovery.Key(discovery.AnnEnabled): "true"}
}

func TestPrimerPopulatesTheRegistryFromAFullListing(t *testing.T) {
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
		annotatedService("shop", "web", enabledAnnotations()),
		annotatedService("shop", "api", enabledAnnotations()),
		// Not opted in: priming must not invent endpoints the watch would not.
		annotatedService("shop", "internal", nil),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()

	reg := registry.New()
	p := &Primer{
		Client:  c,
		Service: &ServiceReconciler{Client: c, Registry: reg, Options: discovery.Defaults()},
	}

	if err := p.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	got := reg.Snapshot()
	if len(got) != 2 {
		t.Fatalf("primed %d endpoints, want 2: %#v", len(got), got)
	}
	if reg.Len() != 2 {
		t.Errorf("sources = %d, want 2", reg.Len())
	}
}

// Priming is what makes the first render complete, so it has to signal the
// render loop the same way a reconcile does.
func TestPrimerSignalsTheRenderLoop(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
			annotatedService("shop", "web", enabledAnnotations()),
		).Build()

	reg := registry.New()
	p := &Primer{
		Client:  c,
		Service: &ServiceReconciler{Client: c, Registry: reg, Options: discovery.Defaults()},
	}
	if err := p.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	select {
	case <-reg.Changed():
	default:
		t.Error("priming raised no change signal")
	}
}

// A cluster with no Traefik CRD leaves IngressRoute nil; priming Services alone
// must still work rather than failing on the missing kind.
func TestPrimerSkipsDisabledKinds(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	p := &Primer{Client: c}
	if err := p.Prime(context.Background()); err != nil {
		t.Fatalf("Prime with nothing enabled: %v", err)
	}
}
