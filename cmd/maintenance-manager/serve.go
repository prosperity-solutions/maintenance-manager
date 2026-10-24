package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1b1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/authz"
	"github.com/prosperity-solutions/maintenance-manager/internal/controller"
	"github.com/prosperity-solutions/maintenance-manager/internal/editor"
	"github.com/prosperity-solutions/maintenance-manager/internal/metrics"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// serveConfig is everything `serve` reads from flags and the environment.
type serveConfig struct {
	Namespace      string        // release namespace (POD_NAMESPACE)
	ArmingLeadTime time.Duration // how long before a phase the routes are armed
	MetricsAddr    string
	EditorAddr     string
	PprofAddr      string // empty: no pprof
	Bypass         string // MM_BYPASS_TOKEN; empty disables the bypass
	EditorPassword string // MM_EDITOR_PASSWORD; empty turns the editor off
}

func parseServe(args []string) (serveConfig, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfg := serveConfig{Namespace: os.Getenv("POD_NAMESPACE"), Bypass: os.Getenv("MM_BYPASS_TOKEN"), EditorPassword: os.Getenv("MM_EDITOR_PASSWORD")}
	fs.DurationVar(&cfg.ArmingLeadTime, "arming-lead-time", 15*time.Minute, "arm routes this long before a dated phase starts")
	fs.StringVar(&cfg.MetricsAddr, "metrics-addr", ":8081", "metrics and health address (/metrics, /healthz, /readyz)")
	fs.StringVar(&cfg.EditorAddr, "editor-addr", editor.Addr, "editor address (HTTP basic auth, user admin)")
	fs.StringVar(&cfg.PprofAddr, "pprof-addr", "", "serve net/http/pprof here, e.g. 127.0.0.1:6060 (off when empty; reach it with kubectl port-forward)")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if cfg.Namespace == "" {
		return cfg, errors.New("POD_NAMESPACE is not set; the chart sets it from the downward API")
	}
	if cfg.Bypass != "" && len(cfg.Bypass) < 16 {
		return cfg, errors.New("MM_BYPASS_TOKEN must be empty (bypass off) or at least 16 characters: the status endpoint would make a short token guessable")
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
	if err := gwapiv1.Install(s); err != nil {
		return nil, err
	}
	if err := gwapiv1b1.Install(s); err != nil {
		return nil, err
	}
	if err := egv1a1.AddToScheme(s); err != nil {
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
			// HTTPRoutes: cluster-wide, stripped to labels and parentRefs (§3).
			&gwapiv1.HTTPRoute{}: {Namespaces: map[string]cache.Config{cache.AllNamespaces: {}}, Transform: snapshot.StripRoute},
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
		if meta.IsNoMatchError(err) {
			log.Error("serve: the cluster does not serve gateway.networking.k8s.io/v1 HTTPRoute. maintenance-manager needs Envoy Gateway >= 1.8, which installs the Gateway API; install it first", "err", err)
			return 1
		}
		log.Error("serve: manager", "err", err)
		return 1
	}

	metrics.Register(ctrlmetrics.Registry)
	if cfg.PprofAddr != "" {
		if err := mgr.Add(pprofServer(cfg.PprofAddr, log)); err != nil {
			log.Error("serve: add pprof", "err", err)
			return 1
		}
	}

	srv := &authz.Server{Addr: authz.Addr, Log: log, Drain: authz.DrainDelay, OnShutdown: func() { ready.shutting.Store(true) }}
	if err := mgr.Add(srv); err != nil {
		log.Error("serve: add authz", "err", err)
		return 1
	}
	hostname, _ := os.Hostname()
	ctl := &controller.Controller{
		Config: mgr.GetConfig(), Scheme: scheme, Mapper: mgr.GetRESTMapper(), Shared: mgr.GetCache(), Writer: mgr.GetClient(),
		Namespace: cfg.Namespace, Identity: hostname, Lead: cfg.ArmingLeadTime, Log: log.With("component", "controller"),
	}
	if err := mgr.Add(ctl); err != nil {
		log.Error("serve: add controller", "err", err)
		return 1
	}
	// The served ListenerSet kinds change rarely; asking the mapper on every
	// dashboard load would trigger discovery for an unserved group each time.
	var kindsMu sync.Mutex
	var kinds []*unstructured.UnstructuredList
	var kindsAt time.Time
	listenerSetKinds := func() ([]*unstructured.UnstructuredList, error) {
		kindsMu.Lock()
		defer kindsMu.Unlock()
		if kindsAt.IsZero() || time.Since(kindsAt) > 10*time.Minute {
			k, err := controller.ListenerSetKinds(mgr.GetRESTMapper())
			if err != nil {
				return nil, err
			}
			kinds, kindsAt = k, time.Now()
		}
		return kinds, nil
	}
	ed := &editor.Server{
		Addr: cfg.EditorAddr, Namespace: cfg.Namespace, Password: cfg.EditorPassword,
		Client: mgr.GetClient(), API: mgr.GetAPIReader(), Lead: cfg.ArmingLeadTime, Log: log.With("component", "editor"),
		// The editor reads what the controller sees through the API, not the
		// leader's caches, so every replica shows the same picture.
		View: func(ctx context.Context) (controller.View, error) {
			ls, err := listenerSetKinds()
			if err != nil {
				return controller.View{}, err
			}
			rec := &controller.Reconciler{Namespace: cfg.Namespace, Lead: cfg.ArmingLeadTime, Shared: mgr.GetCache(), Leader: mgr.GetAPIReader(), ListenerSets: ls, Log: log}
			return rec.View(ctx)
		},
	}
	if err := mgr.Add(ed); err != nil {
		log.Error("serve: add editor", "err", err)
		return 1
	}
	builder := &snapshot.Builder{
		Cache: mgr.GetCache(), Namespace: cfg.Namespace, Bypass: cfg.Bypass, Log: log,
		OnSnapshot: func(snap *snapshot.Snapshot) {
			srv.Update(snap)
			ready.loaded.Store(true) // caches synced and the first snapshot exists (§3)
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
// Two flags, each written by one side, so a rebuild that lands during the
// shutdown drain cannot report ready again.
type readiness struct{ loaded, shutting atomic.Bool }

func (r *readiness) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if !r.loaded.Load() || r.shutting.Load() {
		http.Error(w, "not ready: no snapshot yet, or shutting down", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func ptrTo[T any](v T) *T { return &v }

// pprofServer serves net/http/pprof on addr until the manager stops. Off by
// default: profiles show internals, so bind it to 127.0.0.1 and reach it with
// kubectl port-forward. A failure to bind is logged, never fatal (authz runs
// in the same process).
func pprofServer(addr string, log *slog.Logger) manager.RunnableFunc {
	return func(ctx context.Context) error {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		log.Info("pprof listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("pprof stopped; authz keeps serving", "err", err)
			<-ctx.Done()
		}
		return nil
	}
}
