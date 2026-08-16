package controller

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gatusv1alpha1 "github.com/AlessandroZanatta/gatus-sidecar/api/v1alpha1"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/registry"
)

// RenderLoop is the single writer of the Gatus configuration file.
//
// It runs as a manager Runnable rather than a controller because it is not
// reconciling an object: it collapses many watch events into one file write.
type RenderLoop struct {
	Client   client.Client
	Registry *registry.Registry
	Renderer *config.Renderer
	Writer   *config.Writer

	// BaseConfigPath is the operator-maintained part of the config. It is
	// re-read on every render so a ConfigMap edit takes effect without a restart.
	BaseConfigPath string

	// Debounce is how long to wait for the event stream to go quiet before
	// rendering. A rollout touches many Services at once, and Gatus restarts
	// every check's interval on reload, so batching matters.
	Debounce time.Duration

	// WaitForCacheSync blocks until every watch has its initial listing, and
	// Prime then reconciles all of it into the registry. Both are optional, but
	// without them the first render sees a registry that is still filling up, and
	// Gatus deletes the history of every endpoint missing from a configuration it
	// reloads. Publishing a partial file is not a transient cosmetic problem: the
	// data is gone.
	WaitForCacheSync func(context.Context) bool
	Prime            func(context.Context) error

	// Metrics is optional.
	Metrics *Metrics

	// lastWarnings is the warning set reported by the previous render, so a
	// cluster whose objects are rewritten by an operator does not reprint the
	// same warnings on every pass.
	lastWarnings []string
	warnedOnce   bool

	// ready flips once a render has succeeded, so the sidecar does not report
	// ready while Gatus is still looking at a stale or absent file.
	ready atomic.Bool
}

// Ready reports whether at least one render has succeeded.
func (l *RenderLoop) Ready() bool { return l.ready.Load() }

// NeedLeaderElection returns false: every replica renders its own local file,
// so there is nothing to coordinate and no reason to sit idle as a standby.
func (l *RenderLoop) NeedLeaderElection() bool { return false }

// Start runs until the context is cancelled.
func (l *RenderLoop) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("render")

	debounce := l.Debounce
	if debounce <= 0 {
		debounce = 500 * time.Millisecond
	}

	// A stopped timer with a drained channel: nothing is pending until the first
	// change arrives.
	timer := time.NewTimer(debounce)
	if !timer.Stop() {
		<-timer.C
	}
	pending := false

	// The first render must see the whole cluster, not a registry that is still
	// filling in: Gatus purges the history of everything absent from the file it
	// reloads. Waiting for the caches and then priming from a full listing makes
	// the first published configuration complete.
	if l.WaitForCacheSync != nil && !l.WaitForCacheSync(ctx) {
		return fmt.Errorf("caches did not sync")
	}
	if l.Prime != nil {
		if err := l.Prime(ctx); err != nil {
			// Rendering anyway would publish exactly the partial configuration
			// priming exists to prevent, so wait for the watches to fill the
			// registry and render on the next change instead.
			log.Error(err, "priming the registry failed; waiting for watch events before the first render")
		}
	}
	// Drain the signal priming just raised: its work is already in this render.
	select {
	case <-l.Registry.Changed():
	default:
	}

	// Render once at startup so the file exists even in a cluster with nothing
	// annotated yet. Gatus refuses to start without a config.
	if err := l.render(ctx, log); err != nil {
		log.Error(err, "initial render failed; will retry on the next change")
	}

	for {
		select {
		case <-ctx.Done():
			if pending && !timer.Stop() {
				<-timer.C
			}
			return nil

		case <-l.Registry.Changed():
			// Reset the window on every change, so a burst renders once after
			// the burst ends rather than once per event.
			if pending && !timer.Stop() {
				<-timer.C
			}
			timer.Reset(debounce)
			pending = true

		case <-timer.C:
			pending = false
			if err := l.render(ctx, log); err != nil {
				log.Error(err, "render failed; the previous configuration is left in place")
			}
		}
	}
}

// render builds the configuration and publishes it.
//
// Errors leave the existing file untouched. A stale configuration still monitors
// things; an empty or partial one would blank the status page.
func (l *RenderLoop) render(ctx context.Context, log logr) error {
	base, err := config.LoadBase(l.BaseConfigPath)
	if err != nil {
		l.Metrics.recordError()
		return err
	}

	var list gatusv1alpha1.EndpointTemplateList
	if err := l.Client.List(ctx, &list); err != nil {
		l.Metrics.recordError()
		return fmt.Errorf("list endpoint templates: %w", err)
	}
	templates := config.NewTemplateSet(list.Items)

	endpoints := l.Registry.Snapshot()
	result := l.Renderer.Render(endpoints, templates)

	// Warnings describe a standing condition, not an event, and a render happens
	// for every watch event. An operator that rewrites its own Services a few
	// times a second would otherwise reprint the same unchanged warnings
	// thousands of times an hour and bury everything else.
	if !l.warnedOnce || !equalWarnings(l.lastWarnings, result.Warnings) {
		for _, w := range result.Warnings {
			log.Info("endpoint skipped", "reason", w)
		}
		if l.warnedOnce && len(result.Warnings) == 0 {
			log.Info("all previously skipped endpoints now render")
		}
		l.lastWarnings = result.Warnings
		l.warnedOnce = true
	}

	content, err := config.Marshal(config.Assemble(base, result.Endpoints))
	if err != nil {
		l.Metrics.recordError()
		return err
	}

	changed, err := l.Writer.Write(content)
	if err != nil {
		l.Metrics.recordError()
		return err
	}

	l.ready.Store(true)
	l.Metrics.recordRender(len(result.Endpoints), len(result.Warnings))
	l.Metrics.recordSources(l.Registry.Len())

	// Status is informational, so a failure here is logged rather than returned:
	// the configuration has already been published successfully.
	generation := func(name string) int64 {
		if tpl := templates.Get(name); tpl != nil {
			return tpl.Generation
		}
		return 0
	}
	if err := updateTemplateStatus(ctx, l.Client, templates, result.TemplateUsage, generation); err != nil {
		log.Info("could not update template status", "error", err.Error())
	}

	if changed {
		log.Info("wrote gatus configuration",
			"path", l.Writer.Path(),
			"endpoints", len(result.Endpoints),
			"sources", l.Registry.Len(),
			"warnings", len(result.Warnings))
	}
	return nil
}

// equalWarnings compares two warning sets. Order is stable across renders of the
// same cluster state, so a positional comparison is enough.
func equalWarnings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// logr is the subset of the logging interface this loop needs, kept local so the
// file does not depend on the concrete logger type.
type logr interface {
	Info(msg string, keysAndValues ...any)
	Error(err error, msg string, keysAndValues ...any)
}
