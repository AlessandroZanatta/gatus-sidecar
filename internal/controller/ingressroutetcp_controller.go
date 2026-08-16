package controller

import (
	"context"
	"fmt"
	"strings"

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

// TraefikServiceLabel is how the Traefik chart and operator label the Service
// fronting an installation. It is what makes entrypoint ports discoverable
// without being told where Traefik lives.
const TraefikServiceLabel = "app.kubernetes.io/name=traefik"

// IngressRouteTCPReconciler turns Traefik IngressRouteTCPs into registry
// entries.
//
// It differs from the HTTP one in where the public port comes from. A TCP router
// names an entrypoint, not a port, and the port that entrypoint was published on
// lives in the Traefik Service. Those Services are therefore watched as well.
type IngressRouteTCPReconciler struct {
	client.Client
	Registry          *registry.Registry
	Options           discovery.Options
	NamespaceSelector labels.Selector

	// TraefikServices restricts entrypoint lookups to these namespace/name
	// Services. Empty means every Service carrying TraefikServiceLabel, which
	// covers a default installation without configuration.
	TraefikServices []string

	// EntrypointPorts are the --entrypoint-port overrides.
	EntrypointPorts map[string]int32
}

// +kubebuilder:rbac:groups=traefik.io,resources=ingressroutetcps,verbs=get;list;watch

// Reconcile brings the registry in line with one IngressRouteTCP.
func (r *IngressRouteTCPReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	key := registry.Key{Kind: "IngressRouteTCP", Namespace: req.Namespace, Name: req.Name}

	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(discovery.IngressRouteTCPGVK)
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

	ports, err := r.entrypointPorts(ctx)
	if err != nil {
		// Unlike a malformed route, this is a lookup failure and may well be
		// transient, so it is worth retrying rather than dropping the endpoints.
		return ctrl.Result{}, err
	}

	endpoints, err := r.Options.FromIngressRouteTCP(route, nsGroup, r.resolveService(ctx), ports)
	if err != nil {
		log.Error(err, "ignoring IngressRouteTCP: it could not be turned into endpoints")
		r.Registry.Delete(key)
		return ctrl.Result{}, nil
	}

	if r.Registry.Set(key, endpoints) {
		log.V(1).Info("updated endpoints", "count", len(endpoints))
	}
	return ctrl.Result{}, nil
}

// entrypointPorts reads the Traefik Services in scope.
//
// Several installations are normal — one for internal traffic and one for
// external, each publishing its own ports — so they are all collected and the
// ambiguity, if any, is resolved per route.
func (r *IngressRouteTCPReconciler) entrypointPorts(ctx context.Context) (discovery.EntrypointPorts, error) {
	out := discovery.EntrypointPorts{Overrides: r.EntrypointPorts}

	if len(r.TraefikServices) > 0 {
		for _, ref := range r.TraefikServices {
			namespace, name, ok := strings.Cut(ref, "/")
			if !ok {
				return out, fmt.Errorf("traefik service %q is not namespace/name", ref)
			}
			var svc corev1.Service
			if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &svc); err != nil {
				if errors.IsNotFound(err) {
					// Configured but absent: the override flag still works, and
					// saying so beats silently monitoring a guessed port.
					ctrl.LoggerFrom(ctx).V(1).Info("configured traefik service not found", "service", ref)
					continue
				}
				return out, err
			}
			out.Services = append(out.Services, traefikService(&svc))
		}
		return out, nil
	}

	selector, err := labels.Parse(TraefikServiceLabel)
	if err != nil {
		return out, fmt.Errorf("parse traefik service selector: %w", err)
	}
	var list corev1.ServiceList
	if err := r.List(ctx, &list, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return out, fmt.Errorf("list traefik services: %w", err)
	}
	for i := range list.Items {
		out.Services = append(out.Services, traefikService(&list.Items[i]))
	}
	return out, nil
}

// traefikService reduces a Service to the entrypoint ports a client outside the
// cluster would connect to.
//
// The chart names each Service port after the entrypoint it serves, which is the
// join that makes this work. For a NodePort Service the reachable port is the
// node port rather than the service port, and using the wrong one would monitor
// an address no client uses.
func traefikService(svc *corev1.Service) discovery.TraefikService {
	out := discovery.TraefikService{
		Namespace: svc.Namespace,
		Name:      svc.Name,
		Ports:     make(map[string]int32, len(svc.Spec.Ports)),
	}
	for _, p := range svc.Spec.Ports {
		if p.Name == "" {
			continue
		}
		port := p.Port
		if svc.Spec.Type == corev1.ServiceTypeNodePort && p.NodePort != 0 {
			port = p.NodePort
		}
		out.Ports[p.Name] = port
	}
	return out
}

func (r *IngressRouteTCPReconciler) resolveService(ctx context.Context) discovery.ServiceResolver {
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

func (r *IngressRouteTCPReconciler) namespace(ctx context.Context, name string) (*corev1.Namespace, error) {
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
// Every Service event re-derives the routes in that namespace, as for the HTTP
// controller, and a Traefik Service additionally re-derives every route in the
// cluster: its ports are what the public addresses are built from.
func (r *IngressRouteTCPReconciler) SetupWithManager(mgr ctrl.Manager) error {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(discovery.IngressRouteTCPGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(route).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.routesInNamespace)).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.routesForService)).
		Named("ingressroutetcp").
		Complete(r)
}

func (r *IngressRouteTCPReconciler) routesInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.listRoutes(ctx, obj.GetName())
}

// routesForService re-derives the routes in the Service's namespace, or every
// route in the cluster when the Service is one of Traefik's own.
func (r *IngressRouteTCPReconciler) routesForService(ctx context.Context, obj client.Object) []reconcile.Request {
	if r.isTraefikService(obj) {
		return r.listRoutes(ctx, "")
	}
	return r.listRoutes(ctx, obj.GetNamespace())
}

// isTraefikService reports whether a Service supplies entrypoint ports, either
// because it was named explicitly or because it carries Traefik's label.
func (r *IngressRouteTCPReconciler) isTraefikService(obj client.Object) bool {
	if len(r.TraefikServices) > 0 {
		ref := obj.GetNamespace() + "/" + obj.GetName()
		for _, configured := range r.TraefikServices {
			if configured == ref {
				return true
			}
		}
		return false
	}

	selector, err := labels.Parse(TraefikServiceLabel)
	if err != nil {
		return false
	}
	return selector.Matches(labels.Set(obj.GetLabels()))
}

// listRoutes lists the routes in a namespace, or in every namespace when it is
// empty.
func (r *IngressRouteTCPReconciler) listRoutes(ctx context.Context, namespace string) []reconcile.Request {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(discovery.IngressRouteTCPGVK)
	list.SetKind(discovery.IngressRouteTCPGVK.Kind + "List")

	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := r.List(ctx, list, opts...); err != nil {
		ctrl.LoggerFrom(ctx).V(1).Info("listing ingressroutetcps failed", "namespace", namespace, "error", err)
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
