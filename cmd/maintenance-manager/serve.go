package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/authz"
)

// serveConfig is everything `serve` reads from flags and the environment.
type serveConfig struct {
	Namespace      string        // release namespace (POD_NAMESPACE)
	ArmingLeadTime time.Duration // how long before a phase the routes are armed
	MetricsAddr    string
}

func parseServe(args []string) (serveConfig, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfg := serveConfig{Namespace: os.Getenv("POD_NAMESPACE")}
	fs.DurationVar(&cfg.ArmingLeadTime, "arming-lead-time", 15*time.Minute, "arm routes this long before a dated phase starts")
	fs.StringVar(&cfg.MetricsAddr, "metrics-addr", ":8081", "metrics and health address (/metrics, /healthz, /readyz)")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if cfg.Namespace == "" {
		return cfg, errors.New("POD_NAMESPACE is not set; the chart sets it from the downward API")
	}
	if cfg.ArmingLeadTime < 0 {
		return cfg, fmt.Errorf("--arming-lead-time must not be negative, got %s", cfg.ArmingLeadTime)
	}
	return cfg, nil
}

func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := mmv1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

func serve(args []string, log *slog.Logger) int {
	cfg, err := parseServe(args)
	if err != nil {
		log.Error("serve: bad configuration", "err", err)
		return 2
	}
	ctrl.SetLogger(logrFromSlog(log))

	scheme, err := newScheme()
	if err != nil {
		log.Error("serve: scheme", "err", err)
		return 1
	}

	ready := &readiness{}
	inNS := cache.ByObject{Namespaces: map[string]cache.Config{cfg.Namespace: {}}}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), manager.Options{
		Scheme: scheme,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&mmv1.MaintenanceService{}: inNS,
			&mmv1.MaintenancePage{}:    inNS,
			&mmv1.MaintenanceWindow{}:  inNS,
		}},
		// The default :8080 would collide with the editor (§11).
		Metrics: metricsserver.Options{
			BindAddress: cfg.MetricsAddr,
			ExtraHandlers: map[string]http.Handler{
				"/healthz": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) }),
				"/readyz":  ready,
			},
		},
		HealthProbeBindAddress:        "0",
		LeaderElection:                true,
		LeaderElectionID:              "maintenance-manager",
		LeaderElectionNamespace:       cfg.Namespace,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		log.Error("serve: manager", "err", err)
		return 1
	}

	if err := mgr.Add(&authz.Server{Addr: authz.Addr, Log: log}); err != nil {
		log.Error("serve: add authz", "err", err)
		return 1
	}
	if err := mgr.Add(&cacheWatcher{cache: mgr.GetCache(), ready: ready, log: log}); err != nil {
		log.Error("serve: add readiness", "err", err)
		return 1
	}

	log.Info("maintenance-manager starting", "version", version, "namespace", cfg.Namespace, "armingLeadTime", cfg.ArmingLeadTime)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error("serve: stopped", "err", err)
		return 1
	}
	return 0
}

// readiness answers /readyz: 200 once the caches have synced.
type readiness struct{ ok atomic.Bool }

func (r *readiness) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if !r.ok.Load() {
		http.Error(w, "not ready: caches not synced", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// cacheWatcher starts informers for our three resources and flips readiness
// once they have synced. Every replica runs it.
type cacheWatcher struct {
	cache cache.Cache
	ready *readiness
	log   *slog.Logger
}

func (c *cacheWatcher) NeedLeaderElection() bool { return false }

func (c *cacheWatcher) Start(ctx context.Context) error {
	for _, obj := range []client.Object{&mmv1.MaintenanceService{}, &mmv1.MaintenancePage{}, &mmv1.MaintenanceWindow{}} {
		if _, err := c.cache.GetInformer(ctx, obj); err != nil {
			return fmt.Errorf("informer for %T: %w", obj, err)
		}
	}
	if !c.cache.WaitForCacheSync(ctx) {
		return errors.New("caches did not sync")
	}
	c.ready.ok.Store(true)
	c.log.Info("caches synced; ready")
	<-ctx.Done()
	c.ready.ok.Store(false)
	return nil
}
