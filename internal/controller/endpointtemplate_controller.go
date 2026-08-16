package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gatusv1alpha1 "github.com/AlessandroZanatta/gatus-sidecar/api/v1alpha1"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/registry"
)

// EndpointTemplateReconciler triggers a re-render when a template changes.
//
// It deliberately does nothing else. Templates are resolved at render time from
// a fresh list, so there is no reverse index from a template back to the objects
// that use it and no per-object work to redo here.
type EndpointTemplateReconciler struct {
	client.Client
	Registry *registry.Registry
}

// +kubebuilder:rbac:groups=gatus.kalexlab.xyz,resources=endpointtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=gatus.kalexlab.xyz,resources=endpointtemplates/status,verbs=get;update;patch

// Reconcile signals the render loop.
func (r *EndpointTemplateReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	r.Registry.Touch()
	return ctrl.Result{}, nil
}

// SetupWithManager registers the controller.
func (r *EndpointTemplateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatusv1alpha1.EndpointTemplate{}).
		Named("endpointtemplate").
		Complete(r)
}
