package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/AlessandroZanatta/gatus-sidecar/internal/discovery"
)

// Primer fills the registry from a full listing before the first render.
//
// Without it the registry fills object by object as the initial reconcile
// backlog drains, and every intermediate state is a complete configuration as
// far as Gatus is concerned. Gatus deletes the stored history of every endpoint
// missing from the configuration it reloads, so publishing a half-populated file
// destroys the history of whatever had not been reconciled yet — permanently,
// and silently.
//
// It reconciles through the same reconcilers the watches use rather than
// reimplementing discovery, so priming can never disagree with steady state.
type Primer struct {
	Client client.Client

	// Service and IngressRoute are nil when that kind's discovery is disabled.
	Service      *ServiceReconciler
	IngressRoute *IngressRouteReconciler
}

// Prime reconciles every object once. Individual failures are reported but do
// not stop the sweep: one unreadable object should delay nothing else, and the
// watch will retry it anyway.
func (p *Primer) Prime(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("primer")

	services, routes := 0, 0
	if p.Service != nil {
		var list corev1.ServiceList
		if err := p.Client.List(ctx, &list); err != nil {
			return fmt.Errorf("list services: %w", err)
		}
		for i := range list.Items {
			req := ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].Namespace,
				Name:      list.Items[i].Name,
			}}
			if _, err := p.Service.Reconcile(ctx, req); err != nil {
				log.Error(err, "priming service failed; the watch will retry it",
					"service", req.String())
				continue
			}
			services++
		}
	}

	if p.IngressRoute != nil {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(discovery.IngressRouteGVK)
		if err := p.Client.List(ctx, list); err != nil {
			return fmt.Errorf("list ingressroutes: %w", err)
		}
		for i := range list.Items {
			req := ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].GetNamespace(),
				Name:      list.Items[i].GetName(),
			}}
			if _, err := p.IngressRoute.Reconcile(ctx, req); err != nil {
				log.Error(err, "priming ingressroute failed; the watch will retry it",
					"ingressroute", req.String())
				continue
			}
			routes++
		}
	}

	log.Info("primed registry", "services", services, "ingressroutes", routes)
	return nil
}
