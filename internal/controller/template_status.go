package controller

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gatusv1alpha1 "github.com/AlessandroZanatta/gatus-sidecar/api/v1alpha1"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
)

// updateTemplateStatus reports, on each EndpointTemplate, whether it resolves
// and how many endpoints used it in the last render.
//
// This is the only place the sidecar writes to the cluster, and it is purely
// informational: a template that fails to resolve is skipped by the renderer, so
// a status write failing must never stop a configuration from being published.
func updateTemplateStatus(
	ctx context.Context,
	c client.Client,
	templates *config.TemplateSet,
	usage map[string]int,
	generation func(string) int64,
) error {
	var errs []error

	for _, name := range templates.Names() {
		tpl := templates.Get(name)
		if tpl == nil {
			continue
		}

		// Resolve every template, not just the used ones, so an operator sees a
		// broken template immediately rather than only once something adopts it.
		_, resolveErr := templates.Resolve(name)

		want := gatusv1alpha1.EndpointTemplateStatus{
			ObservedGeneration: generation(name),
			UsedBy:             int32(usage[name]),
			Conditions:         []metav1.Condition{readyCondition(tpl, resolveErr)},
		}

		if statusUnchanged(tpl.Status, want) {
			continue
		}

		written, err := patchStatus(ctx, c, tpl, want)
		if err != nil {
			errs = append(errs, fmt.Errorf("update status of template %q: %w", name, err))
			continue
		}
		// Record what was written on the snapshot as well. The next render
		// normally works from a fresh List, but the informer cache can lag, and
		// without this the same status would be patched again on every pass.
		tpl.Status = written
	}

	return errors.Join(errs...)
}

// readyCondition describes whether a template and its extends chain resolve.
func readyCondition(tpl *gatusv1alpha1.EndpointTemplate, err error) metav1.Condition {
	cond := metav1.Condition{
		Type:               gatusv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             gatusv1alpha1.ReasonResolved,
		Message:            "Template resolves",
		ObservedGeneration: tpl.Generation,
	}
	if err == nil {
		return cond
	}

	cond.Status = metav1.ConditionFalse
	cond.Message = err.Error()
	cond.Reason = gatusv1alpha1.ReasonUnknownParent

	var re *config.ResolveError
	if errors.As(err, &re) {
		cond.Reason = re.Reason
	}
	return cond
}

// statusUnchanged reports whether a status write would be a no-op. Renders
// happen on every cluster change, and patching unchanged status would add API
// traffic and bump resourceVersion for nothing.
func statusUnchanged(have, want gatusv1alpha1.EndpointTemplateStatus) bool {
	if have.ObservedGeneration != want.ObservedGeneration || have.UsedBy != want.UsedBy {
		return false
	}
	if len(have.Conditions) != len(want.Conditions) {
		return false
	}
	for i := range want.Conditions {
		h, w := have.Conditions[i], want.Conditions[i]
		if h.Type != w.Type || h.Status != w.Status || h.Reason != w.Reason ||
			h.Message != w.Message || h.ObservedGeneration != w.ObservedGeneration {
			return false
		}
	}
	return true
}

// patchStatus writes the status subresource, preserving the transition time of
// a condition whose status did not actually flip. It returns what was written.
func patchStatus(ctx context.Context, c client.Client, tpl *gatusv1alpha1.EndpointTemplate, want gatusv1alpha1.EndpointTemplateStatus) (gatusv1alpha1.EndpointTemplateStatus, error) {
	updated := tpl.DeepCopy()
	patch := client.MergeFrom(tpl.DeepCopy())

	for i := range want.Conditions {
		want.Conditions[i].LastTransitionTime = transitionTime(tpl.Status.Conditions, want.Conditions[i])
	}
	updated.Status = want

	if err := c.Status().Patch(ctx, updated, patch); err != nil {
		return gatusv1alpha1.EndpointTemplateStatus{}, err
	}
	return want, nil
}

// transitionTime keeps the original timestamp while a condition's status is
// unchanged, so "how long has this been broken" stays answerable.
func transitionTime(existing []metav1.Condition, want metav1.Condition) metav1.Time {
	for _, c := range existing {
		if c.Type == want.Type && c.Status == want.Status && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime
		}
	}
	return metav1.Now()
}
