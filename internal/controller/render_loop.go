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

	// Metrics is optional.
	Metrics *Metrics

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

	for _, w := range result.Warnings {
		log.Info("endpoint skipped", "reason", w)
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

// logr is the subset of the logging interface this loop needs, kept local so the
// file does not depend on the concrete logger type.
type logr interface {
	Info(msg string, keysAndValues ...any)
	Error(err error, msg string, keysAndValues ...any)
}
