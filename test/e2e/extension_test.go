//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/controller"
)

// waitArmedVia waits until our policy of the given kind in ns is Accepted by
// Envoy Gateway and targets the given routes.
func waitArmedVia(t *testing.T, kind, ns string, routes ...string) {
	t.Helper()
	want := strings.Join(routes, ",")
	waitFor(t, 3*time.Minute, "our "+kind+" in "+ns+" is Accepted for "+want, func() (bool, string) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk("gateway.envoyproxy.io", "v1alpha1", kind))
		if err := kc.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: controller.SPName(mmNamespace)}, obj); err != nil {
			return false, err.Error()
		}
		refs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "targetRefs")
		var got []string
		for _, r := range refs {
			got = append(got, r.(map[string]any)["name"].(string))
		}
		if strings.Join(got, ",") != want {
			return false, "targets " + strings.Join(got, ",")
		}
		return accepted(obj)
	})
}

// gone waits until our policy of the given kind in ns no longer exists.
func gone(t *testing.T, kind, ns string) {
	t.Helper()
	waitFor(t, 2*time.Minute, "our "+kind+" in "+ns+" is deleted", func() (bool, string) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk("gateway.envoyproxy.io", "v1alpha1", kind))
		err := kc.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: controller.SPName(mmNamespace)}, obj)
		return client.IgnoreNotFound(err) == nil && err != nil, "still there"
	})
}

// allowAllPolicy is what some platforms put on every route: an IP
// allowlist that admits everyone, which still takes the route's
// SecurityPolicy slot.
func allowAllPolicy(t *testing.T, ns, route string) {
	t.Helper()
	sp := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"targetRefs": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": route}},
		"authorization": map[string]any{
			"defaultAction": "Deny",
			"rules": []any{map[string]any{
				"action":    "Allow",
				"principal": map[string]any{"clientCIDRs": []any{"0.0.0.0/0", "::/0"}},
			}},
		},
	}}}
	sp.SetGroupVersionKind(gvk("gateway.envoyproxy.io", "v1alpha1", "SecurityPolicy"))
	sp.SetNamespace(ns)
	sp.SetName("platform-ip-allow")
	if err := kc.Create(t.Context(), sp); err != nil {
		t.Fatalf("create the platform policy: %v", err)
	}
	t.Cleanup(func() { _ = client.IgnoreNotFound(kc.Delete(context.Background(), sp)) })
	waitPolicyAccepted(t, ns, "platform-ip-allow")
}

// setPhases replaces the window's phases (and end), as the editor does.
func setPhases(t *testing.T, name string, endsAt *metav1.Time, phases ...mmv1.Phase) {
	t.Helper()
	svc, pg := name, name
	for i := range phases {
		phases[i].Routings = []mmv1.Routing{{Services: []string{svc}, Page: pg}}
	}
	if err := retry(t.Context(), 30*time.Second, func() error {
		var w mmv1.MaintenanceWindow
		if err := kc.Get(t.Context(), types.NamespacedName{Namespace: mmNamespace, Name: name}, &w); err != nil {
			return err
		}
		w.Spec.Phases, w.Spec.EndsAt = phases, endsAt
		return kc.Update(t.Context(), &w)
	}); err != nil {
		t.Fatal(err)
	}
}

// E12: a route whose SecurityPolicy slot a platform policy holds is armed
// through our EnvoyExtensionPolicy, and every action behaves as through the
// SecurityPolicy: banner header, status endpoint, interstitial page and
// cookie, 503 page and problem+json, bypass. The platform policy stays
// Accepted throughout. Disabling the window removes our policy.
func TestE12ExtensionPolicy(t *testing.T) {
	ns := "e12"
	newApp(t, ns)
	allowAllPolicy(t, ns, "web")
	svc, pg := service(t, ns), page(t, ns)
	create(t, &mmv1.MaintenanceWindow{ObjectMeta: objMeta(ns), Spec: mmv1.MaintenanceWindowSpec{Enabled: true,
		Phases: []mmv1.Phase{{Name: "warn", Action: mmv1.ActionBanner, Routings: []mmv1.Routing{{Services: []string{svc}, Page: pg}}}}}})
	waitArmedVia(t, "EnvoyExtensionPolicy", ns, "web")

	// Banner.
	waitStable(t, 2*time.Minute, 3, "the banner header arrives through ext_proc", func() (bool, string) {
		r := get(ns, "/", browser, nil)
		return fromEcho(r, ns) && r.header.Get("X-Maintenance-Manager") == "window=e12; phase=warn; action=banner",
			r.String() + " header=" + r.header.Get("X-Maintenance-Manager")
	})
	r := get(ns, "/__maintenance-manager/status?locale=de", "", nil)
	var st map[string]any
	if err := json.Unmarshal([]byte(r.body), &st); err != nil || r.status != 200 || st["action"] != "banner" || st["locale"] != "de" ||
		r.header.Get("Content-Type") != "application/json" || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status: %s (%v)", r, err)
	}

	// Interstitial: the page sets the cookie, and the cookie lets the next
	// request through.
	setPhases(t, ns, nil, mmv1.Phase{Name: "hello", Action: mmv1.ActionInterstitial})
	var cookie *http.Cookie
	waitStable(t, 2*time.Minute, 3, "the interstitial page and its cookie", func() (bool, string) {
		r := get(ns, "/", browser, nil)
		if r.status != 200 || fromEcho(r, ns) || r.header.Get("Refresh") != "8" || !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") {
			return false, r.String()
		}
		cookie = nil
		for _, c := range (&http.Response{Header: r.header}).Cookies() {
			if strings.HasPrefix(c.Name, "mm_seen_") {
				cookie = c
			}
		}
		return cookie != nil && cookie.HttpOnly, "Set-Cookie: " + r.header.Get("Set-Cookie")
	})
	if r := get(ns, "/", browser, map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}); !fromEcho(r, ns) || r.header.Get("X-Maintenance-Manager") == "" {
		t.Fatalf("with the cookie: want the app with the header, got %s", r)
	}

	// Block.
	end := time.Now().Add(time.Hour)
	setPhases(t, ns, ts(end), mmv1.Phase{Name: "down", Action: mmv1.ActionBlock})
	waitStable(t, 2*time.Minute, 3, "blocked through ext_proc", func() (bool, string) {
		r := get(ns, "/", browser, nil)
		return r.status == 503, r.String()
	})
	r = get(ns, "/", browser, map[string]string{"Accept-Language": "de-CH,de;q=0.9"})
	if !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") || !strings.Contains(r.body, "Gleich wieder da") || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("browser: want the German 503 page, got %s", r)
	}
	if ra, err := http.ParseTime(r.header.Get("Retry-After")); err != nil || !ra.Equal(ts(end).Time) {
		t.Fatalf("Retry-After %q, want %s", r.header.Get("Retry-After"), ts(end).Time)
	}
	if r := get(ns, "/api/orders", "application/json", nil); r.status != 503 || r.header.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("API: want 503 problem+json, got %s", r)
	}

	// Bypass: passes, and the app never sees the header.
	r = get(ns, "/", browser, map[string]string{"X-Maintenance-Manager-Bypass": bypassToken, "X-E2e-Canary": "tweet"})
	if !fromEcho(r, ns) || !strings.Contains(strings.ToLower(r.body), "x-e2e-canary") {
		t.Fatalf("bypass: want the app reflecting the canary, got %s", r)
	}
	if strings.Contains(strings.ToLower(r.body), "x-maintenance-manager-bypass") || strings.Contains(r.body, bypassToken) {
		t.Fatalf("the app saw the bypass header: %s", r.body)
	}
	if r := get(ns, "/", browser, map[string]string{"X-Maintenance-Manager-Bypass": "wrong-token-0000000"}); r.status != 503 {
		t.Fatalf("wrong token: want 503, got %s", r)
	}

	// The platform's policy was never pushed out.
	waitPolicyAccepted(t, ns, "platform-ip-allow")

	// Disable: the app is back and our policy is gone.
	if err := retry(t.Context(), 30*time.Second, func() error {
		var w mmv1.MaintenanceWindow
		if err := kc.Get(t.Context(), types.NamespacedName{Namespace: mmNamespace, Name: ns}, &w); err != nil {
			return err
		}
		w.Spec.Enabled = false
		return kc.Update(t.Context(), &w)
	}); err != nil {
		t.Fatal(err)
	}
	gone(t, "EnvoyExtensionPolicy", ns)
	waitStable(t, 2*time.Minute, 3, "the app answers after disabling", func() (bool, string) {
		r := get(ns, "/", browser, nil)
		return fromEcho(r, ns), r.String()
	})
}
