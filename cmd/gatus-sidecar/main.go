// Command gatus-sidecar watches a Kubernetes cluster and keeps a Gatus
// configuration file in sync with the workloads that opt in to monitoring.
//
// It runs beside Gatus in the same pod, writing to a shared volume. Gatus
// hot-reloads the file, so no restart is needed and each replica renders its own
// copy independently: there is no shared state and therefore no leader election.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	gatusv1alpha1 "github.com/AlessandroZanatta/gatus-sidecar/api/v1alpha1"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/config"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/controller"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/discovery"
	"github.com/AlessandroZanatta/gatus-sidecar/internal/registry"
)

// version is stamped at build time with -X main.version. The default matters:
// an image reporting "dev" was not built by the release pipeline.
var version = "dev"

var scheme = runtime.NewScheme()

func init() {
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := gatusv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
}

type options struct {
	baseConfig       string
	output           string
	serviceMode      string
	ingressRouteMode string
	namespaceSel     string
	annotationPrefix string
	externalSuffix   string
	clusterDomain    string
	defaultScheme    string
	groupFromNS      bool
	debounce         time.Duration
	metricsAddr      string
	healthAddr       string
}

func main() {
	var o options
	flag.StringVar(&o.baseConfig, "base-config", "",
		"Path to the operator-maintained part of the Gatus config (storage, alerting, ui). Its endpoints key is ignored.")
	flag.StringVar(&o.output, "output", "/config/config.yaml",
		"Path to write the rendered Gatus configuration to.")
	flag.StringVar(&o.serviceMode, "service-discovery", string(discovery.ModeOptIn),
		"How Services are picked up: opt-in, auto or disabled.")
	flag.StringVar(&o.ingressRouteMode, "ingressroute-discovery", string(discovery.ModeOptIn),
		"How IngressRoutes are picked up: opt-in, auto or disabled.")
	flag.StringVar(&o.namespaceSel, "namespace-selector", "",
		"Only watch namespaces matching this label selector. Empty means all namespaces.")
	flag.StringVar(&o.annotationPrefix, "annotation-prefix", discovery.DefaultAnnotationPrefix,
		"Prefix for the annotations this sidecar reads.")
	flag.StringVar(&o.externalSuffix, "external-suffix", " (external)",
		"Suffix for the externally-reachable endpoint generated from an IngressRoute.")
	flag.StringVar(&o.clusterDomain, "cluster-domain", "cluster.local",
		"Cluster DNS suffix used to build service URLs.")
	flag.StringVar(&o.defaultScheme, "default-scheme", "http",
		"URL scheme used when neither the workload nor its templates specify one.")
	flag.BoolVar(&o.groupFromNS, "group-from-namespace", true,
		"Derive a missing endpoint group from the object's namespace.")
	flag.DurationVar(&o.debounce, "debounce", 500*time.Millisecond,
		"How long the event stream must be quiet before rendering.")
	flag.StringVar(&o.metricsAddr, "metrics-bind-address", ":8081",
		"Address the metrics endpoint binds to. Set to 0 to disable.")
	flag.StringVar(&o.healthAddr, "health-probe-bind-address", ":8082",
		"Address the health and readiness probes bind to.")

	showVersion := flag.Bool("version", false, "Print the version and exit.")

	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	if err := run(o); err != nil {
		ctrl.Log.Error(err, "fatal")
		os.Exit(1)
	}
}

func run(o options) error {
	log := ctrl.Log.WithName("setup")

	serviceMode, err := discovery.ParseMode(o.serviceMode)
	if err != nil {
		return fmt.Errorf("--service-discovery: %w", err)
	}
	ingressRouteMode, err := discovery.ParseMode(o.ingressRouteMode)
	if err != nil {
		return fmt.Errorf("--ingressroute-discovery: %w", err)
	}
	if o.output == "" {
		return fmt.Errorf("--output is required")
	}

	// Fail fast on an unreadable base config rather than starting up and writing
	// a configuration that silently lacks storage and alerting.
	if _, err := config.LoadBase(o.baseConfig); err != nil {
		return err
	}

	var nsSelector labels.Selector
	if o.namespaceSel != "" {
		if nsSelector, err = labels.Parse(o.namespaceSel); err != nil {
			return fmt.Errorf("--namespace-selector: %w", err)
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: o.metricsAddr},
		HealthProbeBindAddress: o.healthAddr,
		// Each replica writes its own local file, so there is nothing to elect a
		// leader for and no reason for a standby replica to sit idle.
		LeaderElection: false,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	discoveryOpts := discovery.Options{
		Keys:               discovery.NewKeys(o.annotationPrefix),
		ServiceMode:        serviceMode,
		IngressRouteMode:   ingressRouteMode,
		GroupFromNamespace: o.groupFromNS,
		ClusterDomain:      o.clusterDomain,
		ExternalSuffix:     o.externalSuffix,
		DefaultScheme:      o.defaultScheme,
	}

	reg := registry.New()

	if serviceMode != discovery.ModeDisabled {
		if err := (&controller.ServiceReconciler{
			Client:            mgr.GetClient(),
			Registry:          reg,
			Options:           discoveryOpts,
			NamespaceSelector: nsSelector,
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("set up service controller: %w", err)
		}
	}

	if ingressRouteMode != discovery.ModeDisabled {
		// A cluster without Traefik installed has no IngressRoute CRD, and
		// starting a watch for a kind the API server does not serve would stop
		// the manager. Skipping it keeps Service discovery working there.
		installed, err := crdInstalled(mgr, discovery.IngressRouteGVK)
		if err != nil {
			return fmt.Errorf("check for the IngressRoute CRD: %w", err)
		}
		switch {
		case !installed:
			log.Info("IngressRoute discovery requested but the Traefik CRD is not installed; skipping it")
		default:
			if err := (&controller.IngressRouteReconciler{
				Client:            mgr.GetClient(),
				Registry:          reg,
				Options:           discoveryOpts,
				NamespaceSelector: nsSelector,
			}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("set up ingressroute controller: %w", err)
			}
		}
	}

	if err := (&controller.EndpointTemplateReconciler{
		Client:   mgr.GetClient(),
		Registry: reg,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up endpoint template controller: %w", err)
	}

	loop := &controller.RenderLoop{
		Client:         mgr.GetClient(),
		Registry:       reg,
		Renderer:       config.NewRenderer(config.RenderOptions{DefaultScheme: o.defaultScheme}),
		Writer:         config.NewWriter(o.output),
		BaseConfigPath: o.baseConfig,
		Debounce:       o.debounce,
		Metrics:        controller.NewMetrics(),
	}
	if err := mgr.Add(loop); err != nil {
		return fmt.Errorf("add render loop: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add health check: %w", err)
	}
	// Not ready until a configuration has actually been written, so a rollout
	// does not tear down the old pod while the new one has nothing for Gatus.
	if err := mgr.AddReadyzCheck("readyz", func(_ *http.Request) error {
		if !loop.Ready() {
			return fmt.Errorf("no configuration rendered yet")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("add ready check: %w", err)
	}

	log.Info("starting gatus-sidecar",
		"version", version,
		"output", o.output,
		"baseConfig", o.baseConfig,
		"serviceDiscovery", serviceMode,
		"ingressRouteDiscovery", ingressRouteMode,
		"annotationPrefix", discoveryOpts.Keys.Prefix())

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("manager exited: %w", err)
	}
	return nil
}

// crdInstalled reports whether the API server serves a kind. Starting a watch
// for a kind that does not exist stops the manager, so IngressRoute discovery is
// skipped rather than fatal in a cluster without Traefik.
func crdInstalled(mgr ctrl.Manager, gvk schema.GroupVersionKind) (bool, error) {
	mapping, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	return mapping != nil, nil
}
