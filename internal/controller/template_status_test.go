package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gatusv1alpha1 "github.com/AlessandroZanatta/gatus-sidecar/api/v1alpha1"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
)

// statusOf reads a template's status back from the API.
func statusOf(t *testing.T, c client.Client, name string) gatusv1alpha1.EndpointTemplateStatus {
	t.Helper()
	var tpl gatusv1alpha1.EndpointTemplate
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &tpl); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return tpl.Status
}

// ready returns the Ready condition, or the zero value if absent.
func ready(status gatusv1alpha1.EndpointTemplateStatus) metav1.Condition {
	for _, c := range status.Conditions {
		if c.Type == gatusv1alpha1.ConditionReady {
			return c
		}
	}
	return metav1.Condition{}
}

// statusFixture builds a client and template set from the given templates.
func statusFixture(t *testing.T, templates ...*gatusv1alpha1.EndpointTemplate) (client.Client, *config.TemplateSet) {
	t.Helper()

	objs := make([]client.Object, len(templates))
	items := make([]gatusv1alpha1.EndpointTemplate, len(templates))
	for i, tpl := range templates {
		objs[i] = tpl
		items[i] = *tpl
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&gatusv1alpha1.EndpointTemplate{}).
		Build()

	return c, config.NewTemplateSet(items)
}

func noGeneration(string) int64 { return 1 }

func TestUpdateTemplateStatusMarksResolvableTemplatesReady(t *testing.T) {
	c, templates := statusFixture(t,
		template(t, "common", "", nil, nil, "interval: 1m"),
		template(t, "default-http", "http", []string{"http"}, []string{"common"}, `conditions: ["[STATUS] == 200"]`),
	)

	err := updateTemplateStatus(context.Background(), c, templates,
		map[string]int{"default-http": 3, "common": 3}, noGeneration)
	if err != nil {
		t.Fatalf("updateTemplateStatus: %v", err)
	}

	status := statusOf(t, c, "default-http")
	if got := ready(status); got.Status != metav1.ConditionTrue || got.Reason != gatusv1alpha1.ReasonResolved {
		t.Errorf("Ready = %s/%s, want True/Resolved", got.Status, got.Reason)
	}
	if status.UsedBy != 3 {
		t.Errorf("UsedBy = %d, want 3", status.UsedBy)
	}
	if status.ObservedGeneration != 1 {
		t.Errorf("ObservedGeneration = %d, want 1", status.ObservedGeneration)
	}
}

func TestUpdateTemplateStatusReportsResolutionFailures(t *testing.T) {
	tests := []struct {
		name       string
		templates  []*gatusv1alpha1.EndpointTemplate
		check      string
		wantReason string
	}{
		{
			name:       "missing parent",
			templates:  []*gatusv1alpha1.EndpointTemplate{template(t, "child", "", nil, []string{"absent"}, "")},
			check:      "child",
			wantReason: gatusv1alpha1.ReasonUnknownParent,
		},
		{
			name:       "self cycle",
			templates:  []*gatusv1alpha1.EndpointTemplate{template(t, "loop", "", nil, []string{"loop"}, "")},
			check:      "loop",
			wantReason: gatusv1alpha1.ReasonCycle,
		},
		{
			name: "mutual cycle",
			templates: []*gatusv1alpha1.EndpointTemplate{
				template(t, "a", "", nil, []string{"b"}, ""),
				template(t, "b", "", nil, []string{"a"}, ""),
			},
			check:      "a",
			wantReason: gatusv1alpha1.ReasonCycle,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, templates := statusFixture(t, tc.templates...)

			if err := updateTemplateStatus(context.Background(), c, templates, nil, noGeneration); err != nil {
				t.Fatalf("updateTemplateStatus: %v", err)
			}

			got := ready(statusOf(t, c, tc.check))
			if got.Status != metav1.ConditionFalse {
				t.Errorf("Ready = %s, want False", got.Status)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q (message: %s)", got.Reason, tc.wantReason, got.Message)
			}
			if got.Message == "" {
				t.Error("Message is empty; it should say what is wrong")
			}
		})
	}
}

func TestUpdateTemplateStatusChecksUnusedTemplates(t *testing.T) {
	// A broken template should be visible before anything adopts it.
	c, templates := statusFixture(t, template(t, "orphan", "", nil, []string{"absent"}, ""))

	if err := updateTemplateStatus(context.Background(), c, templates, nil, noGeneration); err != nil {
		t.Fatalf("updateTemplateStatus: %v", err)
	}

	status := statusOf(t, c, "orphan")
	if ready(status).Status != metav1.ConditionFalse {
		t.Errorf("Ready = %s, want False even though nothing uses it", ready(status).Status)
	}
	if status.UsedBy != 0 {
		t.Errorf("UsedBy = %d, want 0", status.UsedBy)
	}
}

func TestUpdateTemplateStatusIsIdempotent(t *testing.T) {
	// Renders happen on every cluster change; rewriting unchanged status would
	// add API traffic and bump resourceVersion for nothing.
	c, templates := statusFixture(t, template(t, "x", "http", []string{"http"}, nil, "interval: 1m"))
	usage := map[string]int{"x": 2}

	if err := updateTemplateStatus(context.Background(), c, templates, usage, noGeneration); err != nil {
		t.Fatalf("first update: %v", err)
	}

	var first gatusv1alpha1.EndpointTemplate
	if err := c.Get(context.Background(), client.ObjectKey{Name: "x"}, &first); err != nil {
		t.Fatalf("get: %v", err)
	}

	if err := updateTemplateStatus(context.Background(), c, templates, usage, noGeneration); err != nil {
		t.Fatalf("second update: %v", err)
	}

	var second gatusv1alpha1.EndpointTemplate
	if err := c.Get(context.Background(), client.ObjectKey{Name: "x"}, &second); err != nil {
		t.Fatalf("get: %v", err)
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("resourceVersion changed from %s to %s on an unchanged status",
			first.ResourceVersion, second.ResourceVersion)
	}
}

func TestUpdateTemplateStatusWritesWhenUsageChanges(t *testing.T) {
	c, templates := statusFixture(t, template(t, "x", "http", []string{"http"}, nil, "interval: 1m"))

	if err := updateTemplateStatus(context.Background(), c, templates, map[string]int{"x": 1}, noGeneration); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if err := updateTemplateStatus(context.Background(), c, templates, map[string]int{"x": 7}, noGeneration); err != nil {
		t.Fatalf("second update: %v", err)
	}

	if got := statusOf(t, c, "x").UsedBy; got != 7 {
		t.Errorf("UsedBy = %d, want 7", got)
	}
}

func TestUpdateTemplateStatusPreservesTransitionTime(t *testing.T) {
	// "How long has this been broken" has to stay answerable across renders.
	c, templates := statusFixture(t, template(t, "x", "http", []string{"http"}, nil, "interval: 1m"))

	if err := updateTemplateStatus(context.Background(), c, templates, map[string]int{"x": 1}, noGeneration); err != nil {
		t.Fatalf("first update: %v", err)
	}
	firstTransition := ready(statusOf(t, c, "x")).LastTransitionTime

	time.Sleep(10 * time.Millisecond)

	// Usage changes, but the Ready status does not.
	if err := updateTemplateStatus(context.Background(), c, templates, map[string]int{"x": 9}, noGeneration); err != nil {
		t.Fatalf("second update: %v", err)
	}
	second := statusOf(t, c, "x")

	secondTransition := ready(second).LastTransitionTime
	if !secondTransition.Equal(&firstTransition) {
		t.Errorf("LastTransitionTime moved from %v to %v without the status changing",
			firstTransition, secondTransition)
	}
	if second.UsedBy != 9 {
		t.Errorf("UsedBy = %d, want 9", second.UsedBy)
	}
}

func TestUpdateTemplateStatusWithNoTemplates(t *testing.T) {
	c, templates := statusFixture(t)
	if err := updateTemplateStatus(context.Background(), c, templates, nil, noGeneration); err != nil {
		t.Errorf("updateTemplateStatus: %v", err)
	}
}
