// Package controller wires Kubernetes watches to the endpoint registry and
// drives the render loop.
package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/AlessandroZanatta/gatus-sidecar/internal/discovery"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/registry"
)

// ServiceReconciler turns annotated Services into registry entries.
type ServiceReconciler struct {
	client.Client
	Registry *registry.Registry
	Options  discovery.Options

	// NamespaceSelector, when set, limits monitoring to namespaces whose labels
	// match. It is applied here rather than as a cache filter because the cache
	// scopes by namespace name, and the Namespace object is fetched anyway to
	// read its group annotation.
	NamespaceSelector labels.Selector
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile brings the registry in line with one Service.
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	key := registry.Key{Kind: "Service", Namespace: req.Namespace, Name: req.Name}

	var svc corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if errors.IsNotFound(err) {
			r.Registry.Delete(key)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	ns, err := r.namespace(ctx, req.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !namespaceSelected(r.NamespaceSelector, ns) {
		// The namespace stopped matching the selector, so anything it
		// contributed has to go.
		r.Registry.Delete(key)
		return ctrl.Result{}, nil
	}

	nsGroup := ""
	if ns != nil {
		nsGroup, _ = discovery.Annotation(ns.Annotations, discovery.AnnGroup)
	}

	endpoints, err := r.Options.FromService(&svc, nsGroup)
	if err != nil {
		// A malformed annotation is the author's mistake, not a transient
		// failure: requeueing would spin forever. Drop the endpoint and say why,
		// so the rest of the configuration still renders.
		log.Error(err, "ignoring Service: its gatus annotations could not be read")
		r.Registry.Delete(key)
		return ctrl.Result{}, nil
	}

	if r.Registry.Set(key, endpoints) {
		log.V(1).Info("updated endpoints", "count", len(endpoints))
	}
	return ctrl.Result{}, nil
}

// namespace fetches a Service's namespace, which carries both the optional group
// annotation shared by everything inside it and the labels the selector matches.
// A missing namespace is not an error: the Service is on its way out too.
func (r *ServiceReconciler) namespace(ctx context.Context, name string) (*corev1.Namespace, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &ns); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &ns, nil
}

// SetupWithManager registers the controller.
//
// Namespaces are watched as well, because changing a namespace's group
// annotation has to re-derive every endpoint inside it.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.servicesInNamespace)).
		Named("service").
		Complete(r)
}

// servicesInNamespace maps a Namespace event to every Service it contains.
func (r *ServiceReconciler) servicesInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var services corev1.ServiceList
	if err := r.List(ctx, &services, client.InNamespace(obj.GetName())); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing services for namespace change", "namespace", obj.GetName())
		return nil
	}

	out := make([]reconcile.Request, 0, len(services.Items))
	for i := range services.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: services.Items[i].Namespace,
			Name:      services.Items[i].Name,
		}})
	}
	return out
}
