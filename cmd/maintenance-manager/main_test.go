package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRunUsage(t *testing.T) {
	if got := run(nil); got != 2 {
		t.Fatalf("no command: exit %d, want 2", got)
	}
	if got := run([]string{"nope"}); got != 2 {
		t.Fatalf("unknown command: exit %d, want 2", got)
	}
	if got := run([]string{"version"}); got != 0 {
		t.Fatalf("version: exit %d, want 0", got)
	}
}

func TestParseServe(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	if _, err := parseServe(nil); err == nil {
		t.Fatal("missing POD_NAMESPACE must be an error")
	}
	t.Setenv("POD_NAMESPACE", "mm")
	cfg, err := parseServe([]string{"--arming-lead-time=5m"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Namespace != "mm" || cfg.ArmingLeadTime != 5*time.Minute || cfg.MetricsAddr != ":8081" {
		t.Fatalf("unexpected config %+v", cfg)
	}
	t.Setenv("MM_BYPASS_TOKEN", "0123456789abcdef")
	if cfg, err := parseServe(nil); err != nil || cfg.Bypass != "0123456789abcdef" {
		t.Fatalf("bypass token from MM_BYPASS_TOKEN: %+v, %v", cfg, err)
	}
	if _, err := parseServe([]string{"--arming-lead-time=-1s"}); err == nil {
		t.Fatal("negative lead time must be an error")
	}
}

func TestReadiness(t *testing.T) {
	r := &readiness{}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before sync: %d, want 503", rec.Code)
	}
	r.loaded.Store(true)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("after sync: %d, want 200", rec.Code)
	}
}

func TestScheme(t *testing.T) {
	s, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"MaintenanceService", "MaintenancePage", "MaintenanceWindow"} {
		found := false
		for gvk := range s.AllKnownTypes() {
			if gvk.Group == "maintenance-manager.oss.life.li" && gvk.Kind == kind {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s not registered", kind)
		}
	}
}

func TestReadinessStaysDownAfterShutdown(t *testing.T) {
	// A rebuild that lands during the drain must not report ready again.
	r := &readiness{}
	r.loaded.Store(true)
	r.shutting.Store(true)
	r.loaded.Store(true) // the late snapshot
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("during shutdown: %d, want 503", rec.Code)
	}
}

func TestPprofAddrLoopbackOnly(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "mm")
	for _, ok := range []string{"127.0.0.1:6060", "localhost:6060", "[::1]:6060"} {
		if _, err := parseServe([]string{"--pprof-addr=" + ok}); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{":6060", "0.0.0.0:6060", "10.0.0.1:6060", "6060"} {
		if _, err := parseServe([]string{"--pprof-addr=" + bad}); err == nil {
			t.Errorf("%s must be refused: only loopback", bad)
		}
	}
}
