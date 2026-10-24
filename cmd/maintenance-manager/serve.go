package main

import (
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
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// serveConfig is everything `serve` reads from flags and the environment.
type serveConfig struct {
	Namespace      string        // release namespace (POD_NAMESPACE)
	ArmingLeadTime time.Duration // how long before a phase the routes are armed
	MetricsAddr    string
	Bypass         string // MM_BYPASS_TOKEN; empty disables the bypass
}

func parseServe(args []string) (serveConfig, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfg := serveConfig{Namespace: os.Getenv("POD_NAMESPACE"), Bypass: os.Getenv("MM_BYPASS_TOKEN")}
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
		// Everything is read from the release namespace unless ByObject says
		// otherwise; cluster-wide types (HTTPRoutes, SecurityPolicies) opt out
		// there when they arrive, matching the ClusterRole.
		Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{cfg.Namespace: {}}, ByObject: map[client.Object]cache.ByObject{
			&mmv1.MaintenanceService{}: inNS,
			&mmv1.MaintenancePage{}:    inNS,
			&mmv1.MaintenanceWindow{}:  inNS,
			// HTTPRoutes: cluster-wide, metadata only, stripped (§3).
			snapshot.NewRouteMetadata(): {Namespaces: map[string]cache.Config{cache.AllNamespaces: {}}, Transform: snapshot.StripRoute},
		}},
		// The default :8080 would collide with the editor (§11).
		Metrics: metricsserver.Options{
			BindAddress: cfg.MetricsAddr,
			ExtraHandlers: map[string]http.Handler{
				"/healthz": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) }),
				"/readyz":  ready,
			},
		},
		HealthProbeBindAddress: "0",
		// No manager-level leader election: in controller-runtime v0.24 losing
		// the Lease ends mgr.Start, which would take authz down with it (§12:
		// leader lost, authz unaffected). The controller (M4) runs its own
		// elector inside a runnable that every replica starts.
		LeaderElection: false,
		// Leaves room for the authz drain (10 s) and GracefulStop (5 s) within
		// the pod's 30 s termination grace period.
		GracefulShutdownTimeout: ptrTo(25 * time.Second),
	})
	if err != nil {
		log.Error("serve: manager", "err", err)
		return 1
	}

	srv := &authz.Server{Addr: authz.Addr, Log: log, Drain: authz.DrainDelay, OnShutdown: func() { ready.ok.Store(false) }}
	if err := mgr.Add(srv); err != nil {
		log.Error("serve: add authz", "err", err)
		return 1
	}
	builder := &snapshot.Builder{
		Cache: mgr.GetCache(), Namespace: cfg.Namespace, Bypass: cfg.Bypass, Log: log,
		OnSnapshot: func(snap *snapshot.Snapshot) {
			srv.Update(snap)
			ready.ok.Store(true) // caches synced and the first snapshot exists (§3)
		},
	}
	if err := mgr.Add(builder); err != nil {
		log.Error("serve: add snapshot builder", "err", err)
		return 1
	}

	log.Info("maintenance-manager starting", "version", version, "namespace", cfg.Namespace, "armingLeadTime", cfg.ArmingLeadTime)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error("serve: stopped", "err", err)
		return 1
	}
	return 0
}

// readiness answers /readyz: 200 once the caches have synced and the first
// snapshot is loaded; 503 again when shutdown starts.
type readiness struct{ ok atomic.Bool }

func (r *readiness) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if !r.ok.Load() {
		http.Error(w, "not ready: no snapshot yet, or shutting down", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func ptrTo[T any](v T) *T { return &v }
