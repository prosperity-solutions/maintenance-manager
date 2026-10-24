package editor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/html"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/controller"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

const (
	ns       = "mm"
	password = "correct horse battery"
)

var now = time.Date(2026, 10, 30, 17, 0, 0, 0, time.UTC)

func newServer(t *testing.T, objs ...client.Object) (*Server, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := mmv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := gwapiv1.Install(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &Server{
		Namespace: ns, Password: password, Client: c, API: c, Lead: 15 * time.Minute,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now },
		View: func(context.Context) (controller.View, error) {
			return controller.View{Conflicts: map[snapshot.RouteKey][]string{}, Accepted: map[string]controller.Acceptance{}}, nil
		},
	}, c
}

// do sends a request as a same-origin browser with the right password.
func do(t *testing.T, h http.Handler, method, path string, form url.Values, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequestWithContext(t.Context(), method, "http://editor.test"+path, body)
	r.SetBasicAuth("admin", password)
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for _, m := range mod {
		m(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuthAndHeaders(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("no credentials: want 401 with a Basic challenge, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("CSP with frame-ancestors 'none' must be on every response, 401s too")
	}
	if w := do(t, h, http.MethodGet, "/", nil, func(r *http.Request) { r.SetBasicAuth("root", password) }); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong user: want 401, got %d", w.Code)
	}
	time.Sleep(1100 * time.Millisecond) // the failed guess used this second's slot
	w = do(t, h, http.MethodGet, "/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard: want 200, got %d: %s", w.Code, w.Body)
	}
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "Cache-Control": "no-store"} {
		if w.Header().Get(k) != v {
			t.Errorf("%s: want %q, got %q", k, v, w.Header().Get(k))
		}
	}
	if !strings.Contains(w.Body.String(), "<script>"+tzScript+"</script>") {
		t.Fatal("the tz script must be rendered verbatim, or its CSP hash does not match")
	}
}

func TestCSRF(t *testing.T) {
	s, c := newServer(t)
	h := s.Handler()
	form := url.Values{"name": {"shop"}, "sel0.key": {"app"}, "sel0.value": {"shop"}}
	for name, mod := range map[string]func(*http.Request){
		"cross-site":       func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"foreign origin":   func(r *http.Request) { r.Header.Del("Sec-Fetch-Site"); r.Header.Set("Origin", "https://evil.example") },
		"no origin at all": func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") },
	} {
		if w := do(t, h, http.MethodPost, "/services/-/new", form, mod); w.Code != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d", name, w.Code)
		}
	}
	var list mmv1.MaintenanceServiceList
	_ = c.List(t.Context(), &list)
	if len(list.Items) != 0 {
		t.Fatal("a refused POST must change nothing")
	}
	if w := do(t, h, http.MethodPost, "/services/-/new", form); w.Code != http.StatusSeeOther {
		t.Fatalf("same-origin POST: want 303, got %d: %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPut, "/services/shop", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: want 405, got %d", w.Code)
	}
}

func TestServiceCRUD(t *testing.T) {
	win := &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "w"}, Spec: mmv1.MaintenanceWindowSpec{
		Phases: []mmv1.Phase{{Name: "down", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"shop"}}}}}}}
	s, c := newServer(t, win)
	h := s.Handler()
	if w := do(t, h, http.MethodPost, "/services/-/new", url.Values{"name": {"shop"}, "sel0.key": {"app"}, "sel0.value": {"shop"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var svc mmv1.MaintenanceService
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "shop"}, &svc); err != nil || svc.Spec.RouteSelector.MatchLabels["app"] != "shop" {
		t.Fatalf("created service: %v %+v", err, svc.Spec)
	}
	if w := do(t, h, http.MethodPost, "/services/-/new", url.Values{"name": {"Bad Name"}, "sel0.key": {"app"}}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid name: want 422, got %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/services/-/new", url.Values{"name": {"empty"}}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty selector: want 422, got %d", w.Code)
	}
	stale := svc.ResourceVersion
	if w := do(t, h, http.MethodPost, "/services/shop", url.Values{"resourceVersion": {stale}, "sel0.key": {"app"}, "sel0.value": {"web"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	w := do(t, h, http.MethodPost, "/services/shop", url.Values{"resourceVersion": {stale}, "sel0.key": {"app"}, "sel0.value": {"api"}})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "changed by someone else") {
		t.Fatalf("stale save: want 409 'changed by someone else', got %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/services/shop/delete", url.Values{}); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "still used") {
		t.Fatalf("delete of a referenced service: want 409, got %d", w.Code)
	}
}

func TestPageDeleteRefusedAndPreview(t *testing.T) {
	page := &mmv1.MaintenancePage{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p"}, Spec: mmv1.MaintenancePageSpec{
		FallbackLocale: "en", HTML: `<h1>{{.Title}}</h1>{{if .Interstitial}}<a href="">go</a>{{end}}`, Locales: map[string]mmv1.PageText{"en": {Title: "Back soon"}, "de": {Title: "Gleich da"}}}}
	win := &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "w"}, Spec: mmv1.MaintenanceWindowSpec{
		Phases: []mmv1.Phase{{Name: "down", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"x"}, Page: "p"}}}}}}
	s, _ := newServer(t, page, win)
	h := s.Handler()
	if w := do(t, h, http.MethodPost, "/pages/p/delete", url.Values{}); w.Code != http.StatusConflict {
		t.Fatalf("delete of a referenced page: want 409, got %d", w.Code)
	}
	w := do(t, h, http.MethodGet, "/pages/p/preview?locale=de&action=interstitial", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Gleich da") || !strings.Contains(w.Body.String(), "go</a>") {
		t.Fatalf("preview: %d %s", w.Code, w.Body)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.HasPrefix(csp, "sandbox") || w.Header().Get("Refresh") != "" || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("a preview must be sandboxed and carry no Refresh or cookie: CSP %q", csp)
	}
	w = do(t, h, http.MethodPost, "/pages/-/preview", url.Values{"fallbackLocale": {"en"}, "l0.tag": {"en"}, "l0.title": {"Unsaved"}, "previewLocale": {"en"}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Unsaved") || !strings.HasPrefix(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("form preview: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, http.MethodPost, "/pages/-/new", url.Values{"name": {"loop"}, "fallbackLocale": {"en"}, "l0.tag": {"en"}, "html": {"{{range 9}}{{end}}"}}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a page outside the template subset: want 422, got %d", w.Code)
	}
}

// browserSubmit does what a browser does with the first form of a page whose
// action is action: successful controls, in document order, with select
// options in option order and the textarea's leading newline dropped.
func browserSubmit(t *testing.T, page, action string) url.Values {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	attr := func(n *html.Node, k string) (string, bool) {
		for _, a := range n.Attr {
			if a.Key == k {
				return a.Val, true
			}
		}
		return "", false
	}
	var form *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if form != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "form" {
			if a, _ := attr(n, "action"); a == action {
				form = n
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if form == nil {
		t.Fatalf("no form with action %s", action)
	}
	v := url.Values{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			name, _ := attr(n, "name")
			switch n.Data {
			case "input":
				typ, _ := attr(n, "type")
				val, _ := attr(n, "value")
				switch typ {
				case "checkbox":
					if _, on := attr(n, "checked"); on {
						if val == "" {
							val = "on"
						}
						v.Add(name, val)
					}
				default:
					v.Add(name, val)
				}
				return
			case "textarea":
				var b strings.Builder
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					b.WriteString(c.Data)
				}
				v.Add(name, b.String())
				return
			case "select":
				_, multi := attr(n, "multiple")
				var first string
				picked := false
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type != html.ElementNode || c.Data != "option" {
						continue
					}
					val, ok := attr(c, "value")
					if !ok && c.FirstChild != nil {
						val = c.FirstChild.Data
					}
					if first == "" && !picked {
						first = val
					}
					if _, sel := attr(c, "selected"); sel {
						v.Add(name, val)
						picked = true
					}
				}
				if !multi && !picked {
					v.Add(name, first)
				}
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(form)
	return v
}

func TestWindowFormRoundTripThroughHTML(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	// The second 02:30 of the autumn overlap (01:30 UTC), a missing service
	// and page, services in a non-sorted order, a pending phase.
	overlap := metav1.NewTime(time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC))
	ends := metav1.NewTime(time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	spec := mmv1.MaintenanceWindowSpec{Enabled: true, EndsAt: &ends, Phases: []mmv1.Phase{
		{Name: "warn", Action: mmv1.ActionBanner, Routings: []mmv1.Routing{{Services: []string{"zeta", "alpha", "ghost"}, Page: "gone"}}},
		{Name: "down", StartsAt: &overlap, Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"alpha"}, Page: "p"}, {Services: []string{"zeta"}}}},
		{Name: "later", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"zeta"}}}},
	}}
	objs := []client.Object{
		&mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "w"}, Spec: spec},
		&mmv1.MaintenancePage{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p"}, Spec: mmv1.MaintenancePageSpec{FallbackLocale: "en", Locales: map[string]mmv1.PageText{"en": {}}}},
	}
	for _, n := range []string{"alpha", "zeta", "beta"} {
		objs = append(objs, &mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n}, Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"a": n}}}})
	}
	s, c := newServer(t, objs...)
	s.Now = func() time.Time { return time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC) }
	h := s.Handler()
	inBerlin := func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: tzCookie, Value: berlin.String(), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	}
	w := do(t, h, http.MethodGet, "/windows/w", nil, inBerlin)
	if w.Code != http.StatusOK {
		t.Fatalf("edit page: %d %s", w.Code, w.Body)
	}
	form := browserSubmit(t, w.Body.String(), "/windows/w")
	form.Set("tz", "America/New_York") // the browser moved: must not shift anything
	if w := do(t, h, http.MethodPost, "/windows/w", form, inBerlin); w.Code != http.StatusSeeOther {
		t.Fatalf("untouched save: %d %s", w.Code, w.Body)
	}
	var got mmv1.MaintenanceWindow
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "w"}, &got); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(got.Spec, spec) {
		t.Fatalf("an untouched save changed the window:\n got %+v\nwant %+v", got.Spec, spec)
	}
}

func TestPageFormRoundTripThroughHTML(t *testing.T) {
	spec := mmv1.MaintenancePageSpec{FallbackLocale: "de", HTML: "\n<h1>{{.Title}}</h1>\n\n<p>{{.Message}}</p>\n",
		Locales: map[string]mmv1.PageText{"de": {Title: "Gleich zurück", Message: "\nZeile 1\nZeile 2\n"}, "en": {Title: "<b>Back</b> & soon\nsecond line"}}}
	s, c := newServer(t, &mmv1.MaintenancePage{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p"}, Spec: spec})
	h := s.Handler()
	w := do(t, h, http.MethodGet, "/pages/p", nil)
	form := browserSubmit(t, w.Body.String(), "/pages/p")
	for k, vals := range form { // browsers submit textarea line breaks as CRLF
		for i := range vals {
			vals[i] = strings.ReplaceAll(vals[i], "\n", "\r\n")
		}
		form[k] = vals
	}
	if w := do(t, h, http.MethodPost, "/pages/p", form); w.Code != http.StatusSeeOther {
		t.Fatalf("untouched save: %d %s", w.Code, w.Body)
	}
	var got mmv1.MaintenancePage
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "p"}, &got)
	if !equality.Semantic.DeepEqual(got.Spec, spec) {
		t.Fatalf("an untouched save changed the page:\n got %q\nwant %q", got.Spec, spec)
	}
}

func TestServiceFormRoundTripKeepsExpressions(t *testing.T) {
	spec := mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"b": "2", "a": "1"},
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"web"}}}}}
	s, c := newServer(t, &mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "svc"}, Spec: spec})
	h := s.Handler()
	w := do(t, h, http.MethodGet, "/services/svc", nil)
	form := browserSubmit(t, w.Body.String(), "/services/svc")
	if w := do(t, h, http.MethodPost, "/services/svc", form); w.Code != http.StatusSeeOther {
		t.Fatalf("untouched save: %d %s", w.Code, w.Body)
	}
	var got mmv1.MaintenanceService
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "svc"}, &got)
	if !equality.Semantic.DeepEqual(got.Spec, spec) {
		t.Fatalf("an untouched save changed the service:\n got %+v\nwant %+v", got.Spec, spec)
	}
}

func TestDisableAlwaysWorksAndStartNext(t *testing.T) {
	start := metav1.NewTime(now.Add(-time.Hour))
	invalid := &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bad"}, Spec: mmv1.MaintenanceWindowSpec{Enabled: true,
		Phases: []mmv1.Phase{{Name: "Not A Label", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"x"}}}}}}}
	running := &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "run"}, Spec: mmv1.MaintenanceWindowSpec{Enabled: true,
		Phases: []mmv1.Phase{
			{Name: "warn", StartsAt: &start, Action: mmv1.ActionBanner, Routings: []mmv1.Routing{{Services: []string{"x"}}}},
			{Name: "down", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"x"}}}},
		}}}
	s, c := newServer(t, invalid, running)
	h := s.Handler()
	// Disable carries no resourceVersion: an emergency stop never conflicts.
	if w := do(t, h, http.MethodPost, "/windows/bad/enabled", url.Values{"enabled": {"off"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("disable: %d %s", w.Code, w.Body)
	}
	var got mmv1.MaintenanceWindow
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "bad"}, &got)
	if got.Spec.Enabled {
		t.Fatal("an invalid window must still be disableable")
	}
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "run"}, &got)
	if w := do(t, h, http.MethodPost, "/windows/run/next", url.Values{"resourceVersion": {got.ResourceVersion}, "phase": {"2"}}); w.Code != http.StatusConflict {
		t.Fatalf("next for the wrong phase: want 409, got %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/windows/run/next", url.Values{"resourceVersion": {got.ResourceVersion}, "phase": {"1"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("next: %d %s", w.Code, w.Body)
	}
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "run"}, &got)
	if st := got.Spec.Phases[1].StartsAt; st == nil || !st.Time.Equal(now) {
		t.Fatalf("the pending phase must start now, got %v", st)
	}
}

func TestStartNextPhaseNow(t *testing.T) {
	t0 := now.Add(-time.Hour)
	spec := &mmv1.MaintenanceWindowSpec{Enabled: true, Phases: []mmv1.Phase{
		{Name: "a", Action: mmv1.ActionBanner, Routings: []mmv1.Routing{{Services: []string{"x"}}}},
		{Name: "b", StartsAt: &metav1.Time{Time: now.Add(time.Hour)}, Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"x"}}}},
	}}
	// Undated phase 0 is active: phase 1 (dated later) starts early, now.
	out, err := StartNextPhaseNow(spec, now.Add(500*time.Millisecond), 1)
	if err != nil || !out.Phases[1].StartsAt.Time.Equal(now) {
		t.Fatalf("early start: %v %v", out, err)
	}
	if spec.Phases[1].StartsAt.Time.Equal(now) {
		t.Fatal("the input must not be modified")
	}
	// The previous phase started this very second: the next one starts 1 s later.
	spec.Phases[0].StartsAt = &metav1.Time{Time: now}
	out, err = StartNextPhaseNow(spec, now.Add(200*time.Millisecond), 1)
	if err != nil || !out.Phases[1].StartsAt.Time.Equal(now.Add(time.Second)) {
		t.Fatalf("max(now, prev+1s): %v %v", out.Phases[1].StartsAt, err)
	}
	// Past endsAt the start would make the window invalid.
	spec.Phases[0].StartsAt = &metav1.Time{Time: t0}
	ends := metav1.NewTime(now.Add(-time.Minute))
	spec.EndsAt = &ends
	if _, err := StartNextPhaseNow(spec, now, 1); err == nil {
		t.Fatal("an ended window has no active phase")
	}
}

func TestEndsAtProblem(t *testing.T) {
	past := metav1.NewTime(now.Add(-time.Hour))
	other := metav1.NewTime(now.Add(-2 * time.Hour))
	old := &mmv1.MaintenanceWindowSpec{EndsAt: &past}
	if ps := EndsAtProblem(old, &mmv1.MaintenanceWindowSpec{EndsAt: &past}, now); len(ps) != 0 {
		t.Fatal("an unchanged past endsAt is fine (an ended window can be edited)")
	}
	if ps := EndsAtProblem(old, &mmv1.MaintenanceWindowSpec{EndsAt: &other}, now); len(ps) != 1 {
		t.Fatal("a changed endsAt in the past must be refused")
	}
	if ps := EndsAtProblem(nil, &mmv1.MaintenanceWindowSpec{EndsAt: &past}, now); len(ps) != 1 {
		t.Fatal("a new window cannot end in the past")
	}
}

func TestDashboardShowsStatesAndButtons(t *testing.T) {
	start := metav1.NewTime(now.Add(-time.Minute))
	w := &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "live"}, Spec: mmv1.MaintenanceWindowSpec{Enabled: true,
		Phases: []mmv1.Phase{
			{Name: "warn", StartsAt: &start, Action: mmv1.ActionBanner, Routings: []mmv1.Routing{{Services: []string{"shop"}}}},
			{Name: "down", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"shop"}}}},
		}}}
	svc := &mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "shop"}, Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "shop"}}}}
	rt := &gwapiv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web", Labels: map[string]string{"app": "shop"}}}
	s, _ := newServer(t, w, svc, rt)
	s.View = func(context.Context) (controller.View, error) {
		key := snapshot.RouteKey{Namespace: "shop", Name: "web"}
		return controller.View{Desired: controller.Desired{Routes: map[string][]string{"shop": {"web"}}},
			Conflicts: map[snapshot.RouteKey][]string{key: {"SecurityPolicy x/y targets this route"}},
			Accepted:  map[string]controller.Acceptance{"shop": {OK: true}},
			Targets:   map[string]map[string]bool{"shop": {"web": true}}}, nil
	}
	body := do(t, s.Handler(), http.MethodGet, "/", nil).Body.String()
	for _, want := range []string{"state-active", "Start next phase now: down", "Disable now", "shop/web", "targets this route", `class="ok">yes`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
}

func TestEveryScreenRenders(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	for _, p := range []string{"/", "/services", "/services/-/new", "/pages", "/pages/-/new", "/windows", "/windows/-/new"} {
		if w := do(t, h, http.MethodGet, p, nil); w.Code != http.StatusOK {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	if w := do(t, h, http.MethodGet, "/windows/nope", nil); w.Code != http.StatusNotFound {
		t.Errorf("missing window: want 404, got %d", w.Code)
	}
}

func TestChangedFields(t *testing.T) {
	a := mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"a": "1", "b": "2"}}}
	b := mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"a": "1", "c": "3"}}}
	got := strings.Join(changedFields(a, b), ",")
	if got != "routeSelector.matchLabels.b,routeSelector.matchLabels.c" {
		t.Fatalf("changed fields: %s", got)
	}
}

func TestLoginRateLimitedBeforeCompare(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	wrong := func(r *http.Request) { r.SetBasicAuth("admin", "wrong") }
	if w := do(t, h, http.MethodGet, "/", nil, wrong); w.Code != http.StatusUnauthorized {
		t.Fatalf("first wrong guess: want 401, got %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/", nil, wrong); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second guess within the second: want 429, got %d", w.Code)
	}
	// A flood is answered at once, and at most one guess per second is compared.
	start := time.Now()
	codes := make(chan int, 200)
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- do(t, h, http.MethodGet, "/", nil, wrong).Code }()
	}
	wg.Wait()
	close(codes)
	compared := 0
	for c := range codes {
		if c == http.StatusUnauthorized {
			compared++
		}
	}
	if compared > 1 || time.Since(start) > 2*time.Second {
		t.Fatalf("a flood must be refused before the compare and without waiting: %d compared in %s", compared, time.Since(start))
	}
	time.Sleep(1100 * time.Millisecond)
	for i := range 5 { // a correct login returns its slot: the admin is not limited
		if w := do(t, h, http.MethodGet, "/", nil); w.Code != http.StatusOK {
			t.Fatalf("correct login %d: want 200, got %d", i, w.Code)
		}
	}
}

func TestPostRules(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	form := url.Values{"name": {"shop"}, "sel0.key": {"app"}}
	if w := do(t, h, http.MethodPost, "/services/-/new", form, func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("JSON body: want 415, got %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/services/-/new", form, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }); w.Code != http.StatusForbidden {
		t.Errorf("same-site (a sibling subdomain): want 403, got %d", w.Code)
	}
	big := url.Values{"html": {strings.Repeat("x", maxBody)}}
	if w := do(t, h, http.MethodPost, "/pages/-/new", big); w.Code != http.StatusBadRequest {
		t.Errorf("a body over 4 MiB: want 400, got %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, "/windows/w/delete", url.Values{}); w.Code != http.StatusBadRequest {
		t.Errorf("a delete without resourceVersion: want 400, got %d", w.Code)
	}
}

func TestWindowErrorsKeepWhatWasTyped(t *testing.T) {
	s, c := newServer(t)
	h := s.Handler()
	form := url.Values{"name": {"w"}, "formTz": {"Europe/Berlin"}, "enabled": {"on"},
		"p0.order": {"1"}, "p0.name": {"down"}, "p0.startsAt": {"2026-03-29T02:30"}, "p0.action": {"block"}, "p0.r0.services": {"x"}}
	w := do(t, h, http.MethodPost, "/windows/-/new", form)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `value="2026-03-29T02:30"`) {
		t.Fatalf("a DST-gap time must come back as typed: %d", w.Code)
	}
	var list mmv1.MaintenanceWindowList
	_ = c.List(t.Context(), &list)
	if len(list.Items) != 0 {
		t.Fatal("nothing may be saved")
	}
}

func TestZoneMismatchIsNotSaved(t *testing.T) {
	s, c := newServer(t)
	h := s.Handler()
	form := url.Values{"name": {"w"}, "formTz": {"UTC"}, "tz": {"Europe/Berlin"},
		"p0.order": {"1"}, "p0.name": {"down"}, "p0.startsAt": {"2026-11-02T18:00"}, "p0.action": {"block"}, "p0.r0.services": {"x"}}
	w := do(t, h, http.MethodPost, "/windows/-/new", form)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Nothing is saved yet") ||
		!strings.Contains(w.Body.String(), `value="2026-11-02T18:00"`) || !strings.Contains(w.Header().Get("Set-Cookie"), "mm_tz=Europe/Berlin") {
		t.Fatalf("a browser in another zone must see the form again in its zone: %d %s", w.Code, w.Header().Get("Set-Cookie"))
	}
	var list mmv1.MaintenanceWindowList
	_ = c.List(t.Context(), &list)
	if len(list.Items) != 0 {
		t.Fatal("nothing may be saved")
	}
	// With the cookie (the zone the form was shown in), it saves.
	form.Set("formTz", "Europe/Berlin")
	withCookie := func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: tzCookie, Value: "Europe/Berlin", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	}
	if w := do(t, h, http.MethodPost, "/windows/-/new", form, withCookie); w.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	var got mmv1.MaintenanceWindow
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "w"}, &got)
	if !got.Spec.Phases[0].StartsAt.Time.Equal(time.Date(2026, 11, 2, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("18:00 Berlin is 17:00 UTC, got %v", got.Spec.Phases[0].StartsAt)
	}
}

func TestReopenNeedsATick(t *testing.T) {
	ended := metav1.NewTime(now.Add(-time.Hour))
	start := metav1.NewTime(now.Add(-2 * time.Hour))
	win := &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "old"}, Spec: mmv1.MaintenanceWindowSpec{Enabled: true, EndsAt: &ended,
		Phases: []mmv1.Phase{{Name: "down", StartsAt: &start, Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"x"}}}}}}}
	s, c := newServer(t, win)
	h := s.Handler()
	page := do(t, h, http.MethodGet, "/windows/old", nil).Body.String()
	form := browserSubmit(t, page, "/windows/old")
	form.Set("endsAt", "")
	w := do(t, h, http.MethodPost, "/windows/old", form)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `name="reopen"`) {
		t.Fatalf("reopening an ended window must ask first: %d", w.Code)
	}
	form.Set("reopen", "on")
	if w := do(t, h, http.MethodPost, "/windows/old", form); w.Code != http.StatusSeeOther {
		t.Fatalf("with the tick: %d %s", w.Code, w.Body)
	}
	var got mmv1.MaintenanceWindow
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "old"}, &got)
	if got.Spec.EndsAt != nil {
		t.Fatal("endsAt must be cleared")
	}
}

func TestDashboardPendingAndInvalid(t *testing.T) {
	start := metav1.NewTime(now.Add(-time.Minute))
	live := &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "live"}, Spec: mmv1.MaintenanceWindowSpec{Enabled: true,
		Phases: []mmv1.Phase{{Name: "down", StartsAt: &start, Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"shop"}}}}}}}
	bad := &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bad"}, Spec: mmv1.MaintenanceWindowSpec{Enabled: true,
		Phases: []mmv1.Phase{
			{Name: "Bad Name", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"shop"}}}},
			{Name: "later", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"shop"}}}},
		}}}
	svc := &mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "shop"}, Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "shop"}}}}
	rt := &gwapiv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web", Labels: map[string]string{"app": "shop"}}}
	s, _ := newServer(t, live, bad, svc, rt)
	s.View = func(context.Context) (controller.View, error) {
		return controller.View{Desired: controller.Desired{Routes: map[string][]string{"shop": {"web"}}}}, nil
	}
	body := do(t, s.Handler(), http.MethodGet, "/", nil).Body.String()
	if !strings.Contains(body, ">pending<") {
		t.Error("a route the leader has not attached yet must show as pending, not armed")
	}
	if strings.Contains(body, "Start next phase now: later") || strings.Contains(body, "Bad Name (active)") {
		t.Error("an invalid window has no active phase and no Start-next button")
	}
}

func TestFloodDoesNotLockOutOtherAddresses(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	attacker := func(r *http.Request) { r.RemoteAddr = "10.0.0.9:4444"; r.SetBasicAuth("admin", "wrong") }
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() { defer wg.Done(); do(t, h, http.MethodGet, "/", nil, attacker) }()
	}
	wg.Wait()
	portForward := func(r *http.Request) { r.RemoteAddr = "127.0.0.1:5555" }
	if w := do(t, h, http.MethodGet, "/", nil, portForward); w.Code != http.StatusOK {
		t.Fatalf("the admin's port-forward must not be locked out by another address: %d", w.Code)
	}
}

func TestZoneMismatchComesBeforeTimeChecks(t *testing.T) {
	s, _ := newServer(t)
	// 18:00 meant in New York (22:00 UTC) is in the future at 21:00 UTC, but
	// 18:00 UTC is not: the zone notice must win over "in the past".
	s.Now = func() time.Time { return time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC) }
	form := url.Values{"name": {"w"}, "formTz": {"UTC"}, "tz": {"America/New_York"}, "endsAt": {"2026-11-02T18:00"},
		"p0.order": {"1"}, "p0.name": {"down"}, "p0.action": {"block"}, "p0.r0.services": {"x"}}
	w := do(t, s.Handler(), http.MethodPost, "/windows/-/new", form)
	if !strings.Contains(w.Body.String(), "Nothing is saved yet") {
		t.Fatalf("want the zone notice first: %d", w.Code)
	}
}

// TestGetsNeverWrite: GET skips the CSRF check, so a GET handler that writes
// would be reachable from any page the admin visits (an <img> carries the
// cached credentials). Every GET route runs against a client that fails
// every write.
func TestGetsNeverWrite(t *testing.T) {
	start := metav1.NewTime(now.Add(-time.Minute))
	objs := []client.Object{
		&mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "w"}, Spec: mmv1.MaintenanceWindowSpec{Enabled: true,
			Phases: []mmv1.Phase{{Name: "a", StartsAt: &start, Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"s"}, Page: "p"}}}}}},
		&mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "s"}, Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}}},
		&mmv1.MaintenancePage{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p"}, Spec: mmv1.MaintenancePageSpec{FallbackLocale: "en", Locales: map[string]mmv1.PageText{"en": {}}}},
	}
	s, c := newServer(t, objs...)
	fail := func() error { return errors.New("a GET handler wrote") }
	s.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return fail() },
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return fail() },
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return fail()
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return fail() },
	})
	h := s.Handler()
	for _, p := range []string{"/", "/services", "/services/-/new", "/services/s", "/pages", "/pages/-/new", "/pages/p",
		"/pages/p/preview?locale=en&action=block", "/windows", "/windows/-/new", "/windows/w",
		"/windows/w/enabled", "/windows/w/next", "/windows/w/delete", "/services/s/delete", "/pages/p/delete", "/tz"} {
		w := do(t, h, http.MethodGet, p, nil)
		if w.Code >= 500 || strings.Contains(w.Body.String(), "a GET handler wrote") {
			t.Errorf("GET %s: %d %s", p, w.Code, w.Body)
		}
	}
}

// TestFormsCoverEverySpecField fails when a CRD field is added that the
// forms do not encode: an editor save replaces the whole spec with what the
// form decoded, so an unhandled field (one set by GitOps, say) would be
// wiped on every save. Add it to Encode/Decode and the round-trip tests,
// then here.
func TestFormsCoverEverySpecField(t *testing.T) {
	handled := map[string]bool{
		"routeSelector.matchLabels": true, "routeSelector.matchExpressions": true,
		"fallbackLocale": true, "html": true, "locales.title": true, "locales.message": true,
		"enabled": true, "endsAt": true, "phases.name": true, "phases.startsAt": true, "phases.action": true,
		"phases.routings.services": true, "phases.routings.page": true,
	}
	for _, typ := range []reflect.Type{
		reflect.TypeFor[mmv1.MaintenanceServiceSpec](), reflect.TypeFor[mmv1.MaintenancePageSpec](), reflect.TypeFor[mmv1.MaintenanceWindowSpec](),
	} {
		for _, f := range jsonFields(typ, "") {
			if !handled[f] {
				t.Errorf("%s.%s is not handled by the editor's forms: a save would wipe it", typ.Name(), f)
			}
		}
	}
}

// jsonFields lists the JSON paths of a spec type, stopping at types the
// forms treat as a unit (time, label selector parts, strings, maps of text).
func jsonFields(t reflect.Type, prefix string) []string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == reflect.TypeFor[metav1.Time]() {
		return []string{prefix}
	}
	if t == reflect.TypeFor[metav1.LabelSelector]() {
		return []string{prefix + ".matchLabels", prefix + ".matchExpressions"}
	}
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		p := name
		if prefix != "" {
			p = prefix + "." + name
		}
		out = append(out, jsonFields(f.Type, p)...)
	}
	return out
}

func TestZoneChangeKeepsTypedTimesForTheNextSave(t *testing.T) {
	s, c := newServer(t)
	h := s.Handler()
	form := url.Values{"name": {"w"}, "formTz": {"UTC"}, "tz": {"Europe/Berlin"},
		"p0.order": {"1"}, "p0.name": {"down"}, "p0.startsAt": {"2026-11-02T18:00"}, "p0.action": {"block"}, "p0.r0.services": {"x"}}
	w := do(t, h, http.MethodPost, "/windows/-/new", form)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `value="2026-11-02T18:00"`) {
		t.Fatalf("a typed time must come back as typed: %d", w.Code)
	}
	// Save the shown form again, unchanged, now with the cookie.
	again := browserSubmit(t, w.Body.String(), "/windows/-/new")
	withCookie := func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: tzCookie, Value: "Europe/Berlin", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	}
	if w := do(t, h, http.MethodPost, "/windows/-/new", again, withCookie); w.Code != http.StatusSeeOther {
		t.Fatalf("second save: %d %s", w.Code, w.Body)
	}
	var got mmv1.MaintenanceWindow
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "w"}, &got)
	if !got.Spec.Phases[0].StartsAt.Time.Equal(time.Date(2026, 11, 2, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("18:00 typed in Berlin is 17:00 UTC, got %v", got.Spec.Phases[0].StartsAt)
	}
}

func TestUTCAliasesAreNoZoneChange(t *testing.T) {
	s, _ := newServer(t)
	form := url.Values{"name": {"w"}, "formTz": {"UTC"}, "tz": {"Etc/UTC"},
		"p0.order": {"1"}, "p0.name": {"down"}, "p0.action": {"block"}, "p0.r0.services": {"x"}}
	if w := do(t, s.Handler(), http.MethodPost, "/windows/-/new", form); w.Code != http.StatusSeeOther {
		t.Fatalf("Etc/UTC and UTC are the same zone: %d", w.Code)
	}
}

func TestPageOnlyRoutingIsAProblem(t *testing.T) {
	_, ps := DecodeWindow(url.Values{"p0.order": {"1"}, "p0.name": {"down"}, "p0.action": {"block"},
		"p0.r0.services": {"x"}, "p0.r1.page": {"p"}}, time.UTC)
	if len(ps) != 1 || ps[0].Field != "p0.r1.services" {
		t.Fatalf("a routing with a page but no services must be reported: %v", ps)
	}
	_, ps = DecodeWindow(url.Values{"p0.order": {"1"}, "p0.r0.page": {"p"}, "p1.order": {"2"}}, time.UTC)
	if len(ps) == 0 {
		t.Fatal("a phase row with only a page must be reported, not dropped")
	}
}

func TestInvalidStoredValuesSurviveTheForm(t *testing.T) {
	// An action outside the three and a duplicate service (both written with
	// kubectl) must come back as they are: the save is refused, the window
	// stays invalid, and nothing is quietly "fixed" into a banner.
	spec := mmv1.MaintenanceWindowSpec{Phases: []mmv1.Phase{
		{Name: "down", Action: "Block", Routings: []mmv1.Routing{{Services: []string{"a", "a"}}}},
	}}
	s, c := newServer(t, &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "w"}, Spec: spec},
		&mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "a"}, Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"a": "a"}}}})
	h := s.Handler()
	page := do(t, h, http.MethodGet, "/windows/w", nil).Body.String()
	form := browserSubmit(t, page, "/windows/w")
	if got := form["p0.action"]; len(got) != 1 || got[0] != "Block" {
		t.Fatalf("the stored action must be submitted as is, got %v", got)
	}
	if got := form["p0.r0.services"]; len(got) != 2 {
		t.Fatalf("the duplicate service must be submitted twice, got %v", got)
	}
	if w := do(t, h, http.MethodPost, "/windows/w", form); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an invalid window must not save: %d", w.Code)
	}
	var got mmv1.MaintenanceWindow
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "w"}, &got)
	if !equality.Semantic.DeepEqual(got.Spec, spec) {
		t.Fatalf("the window changed: %+v", got.Spec)
	}
	if w := do(t, h, http.MethodPost, "/windows/w", url.Values{"formTz": {"UTC"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("an update without resourceVersion: want 400, got %d", w.Code)
	}
}
