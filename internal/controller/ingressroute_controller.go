package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/AlessandroZanatta/gatus-sidecar/internal/discovery"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/registry"
)

// IngressRouteReconciler turns Traefik IngressRoutes into registry entries.
//
// IngressRoutes are read as unstructured objects so the sidecar carries no
// dependency on Traefik's Kubernetes types, and so a cluster without the CRD
// installed simply has this controller disabled rather than failing to start.
type IngressRouteReconciler struct {
	client.Client
	Registry          *registry.Registry
	Options           discovery.Options
	NamespaceSelector labels.Selector
}

// +kubebuilder:rbac:groups=traefik.io,resources=ingressroutes,verbs=get;list;watch

// Reconcile brings the registry in line with one IngressRoute.
func (r *IngressRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	key := registry.Key{Kind: "IngressRoute", Namespace: req.Namespace, Name: req.Name}

	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(discovery.IngressRouteGVK)
	if err := r.Get(ctx, req.NamespacedName, route); err != nil {
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
		r.Registry.Delete(key)
		return ctrl.Result{}, nil
	}

	nsGroup := ""
	if ns != nil {
		nsGroup, _ = discovery.Annotation(ns.Annotations, discovery.AnnGroup)
	}

	endpoints, err := r.Options.FromIngressRoute(route, nsGroup, r.resolveService(ctx))
	if err != nil {
		// A malformed rule or a missing backend is the author's mistake, not a
		// transient failure, so requeueing would spin. Drop it and say why.
		log.Error(err, "ignoring IngressRoute: it could not be turned into endpoints")
		r.Registry.Delete(key)
		return ctrl.Result{}, nil
	}

	if r.Registry.Set(key, endpoints) {
		log.V(1).Info("updated endpoints", "count", len(endpoints))
	}
	return ctrl.Result{}, nil
}

// resolveService looks a backend Service up in the cache so its port can be
// resolved by name.
func (r *IngressRouteReconciler) resolveService(ctx context.Context) discovery.ServiceResolver {
	return func(namespace, name string) ([]discovery.NamedPort, error) {
		var svc corev1.Service
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &svc); err != nil {
			return nil, err
		}
		out := make([]discovery.NamedPort, len(svc.Spec.Ports))
		for i, p := range svc.Spec.Ports {
			out[i] = discovery.NamedPort{Name: p.Name, Port: p.Port}
		}
		return out, nil
	}
}

func (r *IngressRouteReconciler) namespace(ctx context.Context, name string) (*corev1.Namespace, error) {
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
// Services are watched too: an IngressRoute's endpoint depends on its backend's
// ports, so a Service gaining or renaming a port has to re-derive the routes
// pointing at it.
func (r *IngressRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(discovery.IngressRouteGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(route).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.routesInNamespace)).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.routesForService)).
		Named("ingressroute").
		Complete(r)
}

// routesInNamespace maps a Namespace event to every IngressRoute it contains.
func (r *IngressRouteReconciler) routesInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.listRoutes(ctx, obj.GetName())
}

// routesForService maps a Service event to the IngressRoutes in its namespace.
//
// Cross-namespace backends are not followed: they are rare, and listing every
// route in the cluster on every Service event would be a poor trade.
func (r *IngressRouteReconciler) routesForService(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.listRoutes(ctx, obj.GetNamespace())
}

func (r *IngressRouteReconciler) listRoutes(ctx context.Context, namespace string) []reconcile.Request {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(discovery.IngressRouteGVK)
	list.SetKind(discovery.IngressRouteGVK.Kind + "List")

	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		ctrl.LoggerFrom(ctx).V(1).Info("listing ingressroutes failed", "namespace", namespace, "error", err)
		return nil
	}

	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].GetNamespace(),
			Name:      list.Items[i].GetName(),
		}})
	}
	return out
}

// namespaceSelected reports whether a namespace is in scope. Shared with the
// Service controller so both kinds honour the same selector.
func namespaceSelected(selector labels.Selector, ns *corev1.Namespace) bool {
	if selector == nil || selector.Empty() {
		return true
	}
	if ns == nil {
		return false
	}
	return selector.Matches(labels.Set(ns.Labels))
}
