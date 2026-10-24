//go:build e2e

package e2e

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
)

const editorHost = "editor.example.test"

// editor sends a request to the editor through Envoy, as a same-origin
// browser logged in as admin, unless mod changes that.
func editor(method, path string, form url.Values, mod func(*http.Request)) response {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+envoyAddr+path, body)
	if err != nil {
		return response{err: err}
	}
	req.Host = editorHost
	req.SetBasicAuth("admin", editorPassword)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Origin", "http://"+editorHost)
	}
	if mod != nil {
		mod(req)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return response{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return response{status: resp.StatusCode, header: resp.Header, body: string(b), err: err}
}

// TestE11Editor: create a Service, a Page and a Window through the forms; a
// cross-site POST is refused and changes nothing (§13.2). The editor is
// reached through Envoy with an HTTPRoute of our own, as the README suggests
// for users who want a URL; the window stays disabled and selects no route,
// so nothing is armed and no listener drains.
func TestE11Editor(t *testing.T) {
	port := gwapiv1.PortNumber(8080)
	section := gwapiv1.SectionName("http")
	gwNS := gwapiv1.Namespace("envoy-gateway-system")
	route := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "e11-editor"},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{{Name: "eg", Namespace: &gwNS, SectionName: &section}}},
			Hostnames:       []gwapiv1.Hostname{editorHost},
			Rules: []gwapiv1.HTTPRouteRule{{BackendRefs: []gwapiv1.HTTPBackendRef{{BackendRef: gwapiv1.BackendRef{
				BackendObjectReference: gwapiv1.BackendObjectReference{Name: "maintenance-manager", Port: &port}}}}}},
		},
	}
	create(t, route)

	deadline := time.Now().Add(90 * time.Second)
	var r response
	for time.Now().Before(deadline) {
		if r = editor(http.MethodGet, "/", nil, nil); r.err == nil && r.status == http.StatusOK {
			break
		}
		time.Sleep(time.Second)
	}
	if r.status != http.StatusOK || !strings.Contains(r.body, "Dashboard") {
		t.Fatalf("the editor must answer through Envoy: %v", r)
	}

	svcForm := url.Values{"name": {"e11-svc"}, "sel0.key": {"e2e.maintenance-manager.oss.life.li/app"}, "sel0.value": {"e11-none"}}
	for name, mod := range map[string]func(*http.Request){
		"cross-site": func(req *http.Request) { req.Header.Set("Sec-Fetch-Site", "cross-site"); req.Header.Del("Origin") },
		"foreign origin": func(req *http.Request) {
			req.Header.Del("Sec-Fetch-Site")
			req.Header.Set("Origin", "https://evil.example")
		},
	} {
		if r := editor(http.MethodPost, "/services/-/new", svcForm, mod); r.status != http.StatusForbidden {
			t.Fatalf("%s POST: want 403, got %v", name, r)
		}
	}
	var svc mmv1.MaintenanceService
	if err := kc.Get(t.Context(), client.ObjectKey{Namespace: mmNamespace, Name: "e11-svc"}, &svc); err == nil {
		t.Fatal("a refused cross-site POST must not create anything")
	}

	cleanup := func(obj client.Object, name string) {
		obj.SetNamespace(mmNamespace)
		obj.SetName(name)
		t.Cleanup(func() { _ = client.IgnoreNotFound(kc.Delete(context.Background(), obj)) })
	}
	cleanup(&mmv1.MaintenanceWindow{}, "e11-win")
	cleanup(&mmv1.MaintenancePage{}, "e11-page")
	cleanup(&mmv1.MaintenanceService{}, "e11-svc")

	steps := []struct {
		path string
		form url.Values
	}{
		{"/services/-/new", svcForm},
		{"/pages/-/new", url.Values{"name": {"e11-page"}, "fallbackLocale": {"en"}, "l0.tag": {"en"}, "l0.title": {"Back soon"}, "l0.message": {"Line 1\r\nLine 2"}}},
		{"/windows/-/new", url.Values{"name": {"e11-win"}, "formTz": {"Europe/Berlin"}, "endsAt": {"2099-01-01T01:00"},
			"p0.order": {"1"}, "p0.name": {"down"}, "p0.action": {"block"}, "p0.r0.services": {"e11-svc"}, "p0.r0.page": {"e11-page"}}},
	}
	for _, st := range steps {
		if r := editor(http.MethodPost, st.path, st.form, nil); r.status != http.StatusSeeOther {
			t.Fatalf("POST %s: want 303, got %v", st.path, r)
		}
	}

	if err := kc.Get(t.Context(), client.ObjectKey{Namespace: mmNamespace, Name: "e11-svc"}, &svc); err != nil ||
		svc.Spec.RouteSelector.MatchLabels["e2e.maintenance-manager.oss.life.li/app"] != "e11-none" {
		t.Fatalf("service: %v %+v", err, svc.Spec)
	}
	var pg mmv1.MaintenancePage
	if err := kc.Get(t.Context(), client.ObjectKey{Namespace: mmNamespace, Name: "e11-page"}, &pg); err != nil ||
		pg.Spec.Locales["en"].Message != "Line 1\nLine 2" {
		t.Fatalf("page: %v %+v", err, pg.Spec)
	}
	var win mmv1.MaintenanceWindow
	if err := kc.Get(t.Context(), client.ObjectKey{Namespace: mmNamespace, Name: "e11-win"}, &win); err != nil {
		t.Fatal(err)
	}
	if win.Spec.Enabled || win.Spec.EndsAt == nil || !win.Spec.EndsAt.Time.Equal(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) ||
		len(win.Spec.Phases) != 1 || win.Spec.Phases[0].Routings[0].Page != "e11-page" {
		t.Fatalf("window (endsAt 01:00 Berlin = 00:00 UTC): %+v", win.Spec)
	}
	if r := editor(http.MethodGet, "/", nil, nil); !strings.Contains(r.body, "e11-win") {
		t.Fatalf("the dashboard must list the window: %v", r)
	}

	// Delete through the forms: the page and service are refused while the
	// window uses them.
	for _, p := range []string{"/services/e11-svc/delete", "/pages/e11-page/delete"} {
		if r := editor(http.MethodPost, p, url.Values{}, nil); r.status != http.StatusConflict {
			t.Fatalf("deleting a used page or service (%s): want 409, got %v", p, r)
		}
	}
	for _, del := range []struct {
		path string
		obj  client.Object
	}{
		{"/windows/e11-win/delete", &mmv1.MaintenanceWindow{}},
		{"/pages/e11-page/delete", &mmv1.MaintenancePage{}},
		{"/services/e11-svc/delete", &mmv1.MaintenanceService{}},
	} {
		name := strings.Split(del.path, "/")[2]
		if err := kc.Get(t.Context(), client.ObjectKey{Namespace: mmNamespace, Name: name}, del.obj); err != nil {
			t.Fatal(err)
		}
		if r := editor(http.MethodPost, del.path, url.Values{"resourceVersion": {del.obj.GetResourceVersion()}}, nil); r.status != http.StatusSeeOther {
			t.Fatalf("POST %s: want 303, got %v", del.path, r)
		}
	}
	if err := kc.Get(t.Context(), client.ObjectKey{Namespace: mmNamespace, Name: "e11-svc"}, &svc); err == nil {
		t.Fatal("the service must be gone")
	}

	// Last: a wrong guess takes the replica's login slot for a second.
	if r := editor(http.MethodGet, "/", nil, func(req *http.Request) { req.SetBasicAuth("admin", "wrong-password") }); r.status != http.StatusUnauthorized {
		t.Fatalf("a wrong password must get 401: %v", r)
	}
}
