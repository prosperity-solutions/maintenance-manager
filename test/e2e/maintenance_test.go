//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
)

// attachFixture applies the fixture SecurityPolicy and ReferenceGrant (until
// the M4 controller does it), waits until Envoy Gateway accepts the policy,
// and removes both when the test ends.
func attachFixture(t *testing.T, ns string) {
	t.Helper()
	ctx := t.Context()
	if err := applyFile(ctx, "fixture-securitypolicy.yaml", ns); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// t.Context() is already cancelled when cleanups run.
		if err := deleteFile(context.Background(), "fixture-securitypolicy.yaml", ns); err != nil {
			t.Errorf("remove fixture: %v", err)
		}
	})
	waitFor(t, 2*time.Minute, "fixture SecurityPolicy Accepted", func() (bool, string) {
		sp := &unstructured.Unstructured{}
		sp.SetGroupVersionKind(gvk("gateway.envoyproxy.io", "v1alpha1", "SecurityPolicy"))
		if err := kc.Get(ctx, types.NamespacedName{Namespace: ns, Name: "mm-fixture"}, sp); err != nil {
			return false, err.Error()
		}
		return accepted(sp)
	})
}

// accepted reports whether every ancestor reports Accepted=True for the
// current generation.
func accepted(obj *unstructured.Unstructured) (bool, string) {
	ancestors, _, _ := unstructured.NestedSlice(obj.Object, "status", "ancestors")
	if len(ancestors) == 0 {
		return false, "no ancestors in status yet"
	}
	for _, a := range ancestors {
		conds, _, _ := unstructured.NestedSlice(a.(map[string]any), "conditions")
		ok := false
		for _, c := range conds {
			m := c.(map[string]any)
			if m["type"] == "Accepted" {
				gen, _ := m["observedGeneration"].(int64)
				if m["status"] != "True" || gen != obj.GetGeneration() {
					return false, fmt.Sprintf("Accepted=%v reason=%v message=%v", m["status"], m["reason"], m["message"])
				}
				ok = true
			}
		}
		if !ok {
			return false, "no Accepted condition yet"
		}
	}
	return true, ""
}

// create makes a maintenance-manager resource in the release namespace and
// deletes it when the test ends.
func create(t *testing.T, obj client.Object) {
	t.Helper()
	obj.SetNamespace(mmNamespace)
	if err := kc.Create(t.Context(), obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
	t.Cleanup(func() {
		if err := client.IgnoreNotFound(kc.Delete(context.Background(), obj)); err != nil {
			t.Errorf("delete %s: %v", obj.GetName(), err)
		}
	})
}

// service selects the app's HTTPRoute by the label app.yaml puts on it.
func service(t *testing.T, ns string) string {
	t.Helper()
	create(t, &mmv1.MaintenanceService{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
		Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{
			MatchLabels: map[string]string{"e2e.maintenance-manager.oss.life.li/app": ns},
		}},
	})
	return ns
}

// page creates a two-locale page and returns its name.
func page(t *testing.T, ns string) string {
	t.Helper()
	create(t, &mmv1.MaintenancePage{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
		Spec: mmv1.MaintenancePageSpec{
			FallbackLocale: "en",
			Locales: map[string]mmv1.PageText{
				"en": {Title: "We'll be right back", Message: "Scheduled maintenance."},
				"de": {Title: "Gleich wieder da", Message: "Geplante Wartung."},
			},
		},
	})
	return ns
}

func ts(t time.Time) *metav1.Time {
	m := metav1.NewTime(t.UTC().Truncate(time.Second))
	return &m
}

// maintenance sets up an app with a window of the given phases on it.
func maintenance(t *testing.T, ns string, endsAt *metav1.Time, phases ...mmv1.Phase) {
	t.Helper()
	newApp(t, ns)
	svc, pg := service(t, ns), page(t, ns)
	for i := range phases {
		phases[i].Routings = []mmv1.Routing{{Services: []string{svc}, Page: pg}}
	}
	create(t, &mmv1.MaintenanceWindow{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
		Spec:       mmv1.MaintenanceWindowSpec{Enabled: true, EndsAt: endsAt, Phases: phases},
	})
	attachFixture(t, ns)
}

const browser = "text/html,application/xhtml+xml,*/*;q=0.8"

// E1: block → 503 HTML for a browser, problem+json for an API client,
// Cache-Control: no-store, Retry-After.
func TestE1Block(t *testing.T) {
	ns := "e1"
	end := time.Now().Add(time.Hour)
	maintenance(t, ns, ts(end), mmv1.Phase{Name: "down", Action: mmv1.ActionBlock})

	waitStable(t, 2*time.Minute, 5, "browser and API requests are blocked", func() (bool, string) {
		r := get(ns, "/", browser, nil)
		if r.status != 503 {
			return false, "browser: " + r.String()
		}
		r = get(ns, "/api/orders", "application/json", nil)
		return r.status == 503, "API: " + r.String()
	})

	r := get(ns, "/", browser, map[string]string{"Accept-Language": "de-CH,de;q=0.9"})
	if r.status != 503 || !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") || !strings.Contains(r.body, "Gleich wieder da") {
		t.Fatalf("browser: want the German 503 page, got %s", r)
	}
	if got := r.header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("browser: Cache-Control %q, want no-store", got)
	}
	ra, err := http.ParseTime(r.header.Get("Retry-After"))
	if err != nil || !ra.Equal(ts(end).Time) {
		t.Fatalf("Retry-After %q, want the window end %s (%v)", r.header.Get("Retry-After"), ts(end).Time, err)
	}

	r = get(ns, "/api/orders", "application/json", nil)
	if r.status != 503 || r.header.Get("Content-Type") != "application/problem+json" || r.header.Get("Retry-After") == "" {
		t.Fatalf("API: want 503 application/problem+json with Retry-After, got %s", r)
	}
	var problem struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Window string `json:"window"`
	}
	if err := json.Unmarshal([]byte(r.body), &problem); err != nil || problem.Status != 503 || problem.Window != ns ||
		problem.Type != "https://maintenance-manager.oss.life.li/problems/maintenance" {
		t.Fatalf("API: bad problem+json %s (%v)", r.body, err)
	}
	if r := do("POST", ns, "/submit", browser, nil); r.status != 503 || r.header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("POST: want 503 problem+json, got %s", r)
	}
}

// E2: banner → the app answers, with the X-Maintenance-Manager header; the
// status endpoint returns JSON.
func TestE2Banner(t *testing.T) {
	ns := "e2"
	maintenance(t, ns, nil, mmv1.Phase{Name: "warn", Action: mmv1.ActionBanner})
	waitStable(t, 2*time.Minute, 3, "the banner header arrives", func() (bool, string) {
		r := get(ns, "/", browser, nil)
		return fromEcho(r, ns) && r.header.Get("X-Maintenance-Manager") == "window=e2; phase=warn; action=banner", r.String() + " header=" + r.header.Get("X-Maintenance-Manager")
	})
	r := get(ns, "/__maintenance-manager/status?locale=de", "", nil)
	var st map[string]any
	if err := json.Unmarshal([]byte(r.body), &st); err != nil || r.status != 200 || st["action"] != "banner" ||
		st["locale"] != "de" || st["title"] != "Gleich wieder da" || r.header.Get("X-Maintenance-Manager") != "1" ||
		r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status: %s (%v)", r, err)
	}
}

// E3: interstitial → a 200 page and a cookie; the retry with the cookie
// passes; a POST passes.
func TestE3Interstitial(t *testing.T) {
	ns := "e3"
	maintenance(t, ns, nil, mmv1.Phase{Name: "hello", Action: mmv1.ActionInterstitial})
	var cookie *http.Cookie
	waitStable(t, 2*time.Minute, 3, "the interstitial page is served", func() (bool, string) {
		r := get(ns, "/", browser, nil)
		if r.status != 200 || fromEcho(r, ns) || r.header.Get("Refresh") != "8" {
			return false, r.String()
		}
		for _, c := range (&http.Response{Header: r.header}).Cookies() {
			if strings.HasPrefix(c.Name, "mm_seen_") {
				cookie = c
			}
		}
		return cookie != nil && cookie.HttpOnly && cookie.SameSite == http.SameSiteLaxMode && !cookie.Secure, r.header.Get("Set-Cookie")
	})
	r := get(ns, "/", browser, map[string]string{"Cookie": cookie.Name + "=" + cookie.Value})
	if !fromEcho(r, ns) || r.header.Get("X-Maintenance-Manager") == "" {
		t.Fatalf("with the cookie: want the app with the header, got %s", r)
	}
	if r := do("POST", ns, "/", browser, nil); !fromEcho(r, ns) {
		t.Fatalf("POST: want the app, got %s", r)
	}
}

// E4: the bypass header passes during a block, and the app never sees it.
func TestE4Bypass(t *testing.T) {
	ns := "e4"
	maintenance(t, ns, nil, mmv1.Phase{Name: "down", Action: mmv1.ActionBlock})
	waitStable(t, 2*time.Minute, 3, "blocked without the bypass", func() (bool, string) {
		r := get(ns, "/", browser, nil)
		return r.status == 503, r.String()
	})
	// The canary proves the echo app reflects request headers, so the
	// absence of the bypass header below means something.
	r := get(ns, "/", browser, map[string]string{"X-Maintenance-Manager-Bypass": bypassToken, "X-E2e-Canary": "tweet"})
	if !fromEcho(r, ns) {
		t.Fatalf("bypass: want the app, got %s", r)
	}
	if !strings.Contains(strings.ToLower(r.body), "x-e2e-canary") {
		t.Fatalf("the echo app did not reflect the canary header, so the next check proves nothing: %s", r.body)
	}
	if strings.Contains(strings.ToLower(r.body), "x-maintenance-manager-bypass") || strings.Contains(r.body, bypassToken) {
		t.Fatalf("the app saw the bypass header: %s", r.body)
	}
	if r := get(ns, "/", browser, map[string]string{"X-Maintenance-Manager-Bypass": "wrong-token-0000000"}); r.status != 503 {
		t.Fatalf("wrong token: want 503, got %s", r)
	}
}

// E5: maintenance-manager scaled to 0 → the route serves the app (fail-open).
func TestE5FailOpen(t *testing.T) {
	ns := "e5"
	maintenance(t, ns, nil, mmv1.Phase{Name: "down", Action: mmv1.ActionBlock})
	waitStable(t, 2*time.Minute, 5, "blocked while maintenance-manager runs", func() (bool, string) {
		r := get(ns, "/", browser, nil)
		return r.status == 503, r.String()
	})

	ctx := context.Background()      // also used in Cleanup, after t.Context() is cancelled
	replicas := int32(mmReplicas(t)) //nolint:gosec // a replica count, far below 2^31
	scale := func(n int32) {
		var d appsv1.Deployment
		if err := kc.Get(ctx, types.NamespacedName{Namespace: mmNamespace, Name: "maintenance-manager"}, &d); err != nil {
			t.Fatal(err)
		}
		patch := client.MergeFrom(d.DeepCopy())
		d.Spec.Replicas = &n
		if err := kc.Patch(ctx, &d, patch); err != nil {
			t.Fatal(err)
		}
	}
	scale(0)
	t.Cleanup(func() {
		scale(replicas)
		waitPods(t, int(replicas))
	})
	waitPods(t, 0)

	waitStable(t, 2*time.Minute, 5, "the echo app answers with maintenance-manager down", func() (bool, string) {
		r := get(ns, "/", browser, nil)
		return fromEcho(r, ns), r.String()
	})
}

// E8: the warn → block transition happens by clock, within 2 s of startsAt.
func TestE8Transition(t *testing.T) {
	ns := "e8"
	down := time.Now().Add(75 * time.Second).Truncate(time.Second)
	maintenance(t, ns, nil,
		mmv1.Phase{Name: "warn", Action: mmv1.ActionBanner},
		mmv1.Phase{Name: "down", StartsAt: ts(down), Action: mmv1.ActionBlock},
	)
	waitStable(t, time.Minute, 3, "the warn phase is live", func() (bool, string) {
		r := get(ns, "/", browser, nil)
		return fromEcho(r, ns) && strings.Contains(r.header.Get("X-Maintenance-Manager"), "phase=warn"), r.String()
	})
	if time.Now().After(down) {
		t.Fatalf("setup took too long: the block phase already started at %s", down)
	}
	var blockedAt time.Time
	for time.Now().Before(down.Add(10 * time.Second)) {
		r := get(ns, "/", browser, nil)
		if r.status == 503 {
			blockedAt = time.Now()
			break
		}
		if !fromEcho(r, ns) {
			t.Fatalf("before the switch: unexpected %s", r)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if blockedAt.IsZero() {
		t.Fatalf("no block within 10 s of %s", down)
	}
	lag := blockedAt.Sub(down)
	t.Logf("switched to block %s after startsAt", lag.Round(10*time.Millisecond))
	if lag < -time.Second || lag > 2*time.Second {
		t.Fatalf("switch %s after startsAt; want within 2 s (a node clock skew of up to 1 s is allowed)", lag)
	}
}

// fromEcho reports whether a response came from the echo app for ns, not from
// some other 200.
func fromEcho(r response, ns string) bool {
	return r.status == 200 && strings.Contains(r.body, `"hostname":"`+ns+`.example.test"`)
}
