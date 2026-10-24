package authz

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

const testToken = "s3cret-bypass-token"

var (
	tWarn = time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	tInt  = time.Date(2026, 10, 30, 18, 0, 0, 0, time.UTC)
	tDown = time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)
	tEnd  = time.Date(2026, 10, 30, 22, 30, 0, 0, time.UTC)
)

func mt(t time.Time) *metav1.Time { m := metav1.NewTime(t); return &m }

// testServer serves the "shop/web" route through a window with a banner,
// an interstitial and a block phase, and a page in en and de.
func testServer(t testing.TB, now time.Time) *Server {
	t.Helper()
	page := mmv1.MaintenancePage{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: mmv1.MaintenancePageSpec{
		FallbackLocale: "en",
		Locales: map[string]mmv1.PageText{
			"en": {Title: "Back soon", Message: "Upgrading."},
			"de": {Title: "Gleich zurück", Message: "Wir aktualisieren."},
		},
	}}
	route := func(ph string, start time.Time, a mmv1.Action) mmv1.Phase {
		return mmv1.Phase{Name: ph, StartsAt: mt(start), Action: a, Routings: []mmv1.Routing{{Services: []string{"shop"}, Page: "p"}}}
	}
	w := mmv1.MaintenanceWindow{
		ObjectMeta: metav1.ObjectMeta{Name: "db", UID: types.UID("uid-db")},
		Spec: mmv1.MaintenanceWindowSpec{Enabled: true, EndsAt: mt(tEnd), Phases: []mmv1.Phase{
			route("warn", tWarn, mmv1.ActionBanner),
			route("hello", tInt, mmv1.ActionInterstitial),
			route("down", tDown, mmv1.ActionBlock),
		}},
	}
	svc := mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "shop"}}}}
	snap := snapshot.Build(snapshot.Input{
		Windows: []mmv1.MaintenanceWindow{w}, Pages: []mmv1.MaintenancePage{page}, Services: []mmv1.MaintenanceService{svc},
		Routes: []snapshot.RouteInfo{{Key: snapshot.RouteKey{Namespace: "shop", Name: "web"}, Labels: map[string]string{"app": "shop"}}},
		Bypass: testToken, Now: tWarn.Add(-time.Hour),
	})
	s := &Server{Now: func() time.Time { return now }}
	s.Update(snap)
	return s
}

type req struct {
	method, path string
	headers      map[string]string
	route        string // "ns/name"; "" = no route metadata
}

func (r req) build() *authv3.CheckRequest {
	if r.method == "" {
		r.method = "GET"
	}
	if r.path == "" {
		r.path = "/"
	}
	if r.route == "" {
		r.route = "shop/web"
	}
	ns, name, _ := strings.Cut(r.route, "/")
	return request(r.method, r.headers["accept"], route(ns, name), func(cr *authv3.CheckRequest) {
		h := cr.Attributes.Request.Http
		h.Path = r.path
		for k, v := range r.headers {
			h.Headers[k] = v
		}
	})
}

func respHeader(resp *authv3.CheckResponse, name string) string {
	var hs []*corev3.HeaderValueOption
	if d := resp.GetDeniedResponse(); d != nil {
		hs = d.GetHeaders()
	} else {
		hs = resp.GetOkResponse().GetResponseHeadersToAdd()
	}
	for _, h := range hs {
		if strings.EqualFold(h.GetHeader().GetKey(), name) {
			return h.GetHeader().GetValue()
		}
	}
	return ""
}

func stripsBypass(resp *authv3.CheckResponse) bool {
	for _, h := range resp.GetOkResponse().GetHeadersToRemove() {
		if h == bypassHeader {
			return true
		}
	}
	return false
}

func statusOf(resp *authv3.CheckResponse) int {
	if d := resp.GetDeniedResponse(); d != nil {
		return int(d.GetStatus().GetCode())
	}
	return 0 // passed to the app
}

const browser = "text/html,application/xhtml+xml,*/*;q=0.8"

func TestCheckActions(t *testing.T) {
	cases := []struct {
		name       string
		now        time.Time
		r          req
		wantStatus int // 0 = passed
		check      func(t *testing.T, resp *authv3.CheckResponse)
	}{
		{"before the window: pass", tWarn.Add(-time.Minute), req{headers: map[string]string{"accept": browser}}, 0, func(t *testing.T, resp *authv3.CheckResponse) {
			if respHeader(resp, infoHeader) != "" {
				t.Fatal("no header before the window")
			}
		}},
		{"banner: pass with header", tWarn, req{headers: map[string]string{"accept": browser}}, 0, func(t *testing.T, resp *authv3.CheckResponse) {
			if got := respHeader(resp, infoHeader); got != "window=db; phase=warn; action=banner" {
				t.Fatalf("header %q", got)
			}
		}},
		{"interstitial: page for a browser", tInt, req{headers: map[string]string{"accept": browser, "accept-language": "de-CH,de;q=0.9"}}, 200, func(t *testing.T, resp *authv3.CheckResponse) {
			if c := respHeader(resp, "set-cookie"); !strings.HasPrefix(c, snapshot.CookieName("uid-db")+"=") || !strings.Contains(c, "HttpOnly; SameSite=Lax") || strings.Contains(c, "Secure") || strings.Contains(c, "Max-Age") {
				t.Fatalf("cookie %q", c)
			}
			if respHeader(resp, "refresh") != "8" || !strings.Contains(resp.GetDeniedResponse().GetBody(), "Gleich zurück") {
				t.Fatalf("refresh/body: %q", resp.GetDeniedResponse().GetBody())
			}
		}},
		{"interstitial: pass once the cookie is set", tInt, req{headers: map[string]string{"accept": browser, "cookie": "a=1; " + snapshot.CookieName("uid-db") + "=" + snapshot.PhaseKey(mmv1.Phase{Name: "hello", StartsAt: mt(tInt)}) + "; b=2"}}, 0, nil},
		{"interstitial: a stale cookie shows the page again", tInt, req{headers: map[string]string{"accept": browser, "cookie": snapshot.CookieName("uid-db") + "=00000000"}}, 200, nil},
		{"interstitial: POST passes", tInt, req{method: "POST", headers: map[string]string{"accept": browser}}, 0, nil},
		{"interstitial: API passes", tInt, req{headers: map[string]string{"accept": "application/json"}}, 0, nil},
		{"block: HTML for a browser", tDown, req{headers: map[string]string{"accept": browser}}, 503, func(t *testing.T, resp *authv3.CheckResponse) {
			if respHeader(resp, "retry-after") != "Fri, 30 Oct 2026 22:30:00 GMT" || respHeader(resp, "cache-control") != "no-store" {
				t.Fatalf("retry-after %q cache-control %q", respHeader(resp, "retry-after"), respHeader(resp, "cache-control"))
			}
			if !strings.HasPrefix(respHeader(resp, "content-type"), "text/html") || !strings.Contains(resp.GetDeniedResponse().GetBody(), "Back soon") {
				t.Fatal("not the English HTML page")
			}
		}},
		{"block: problem+json for an API", tDown, req{headers: map[string]string{"accept": "application/json"}}, 503, func(t *testing.T, resp *authv3.CheckResponse) {
			var p map[string]any
			if err := json.Unmarshal([]byte(resp.GetDeniedResponse().GetBody()), &p); err != nil || p["window"] != "db" || p["until"] != "2026-10-30T22:30:00Z" {
				t.Fatalf("problem %v %v", p, err)
			}
			if respHeader(resp, "content-type") != "application/problem+json" || respHeader(resp, "retry-after") == "" {
				t.Fatal("problem+json headers")
			}
		}},
		{"after the window: pass", tEnd, req{headers: map[string]string{"accept": browser}}, 0, nil},
		{"another route: pass", tDown, req{route: "shop/api", headers: map[string]string{"accept": browser}}, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t, tc.now)
			resp, err := s.Check(t.Context(), tc.r.build())
			if err != nil {
				t.Fatal(err)
			}
			if got := statusOf(resp); got != tc.wantStatus {
				t.Fatalf("status %d, want %d", got, tc.wantStatus)
			}
			if tc.check != nil {
				tc.check(t, resp)
			}
		})
	}
}

func TestCheckBypass(t *testing.T) {
	s := testServer(t, tDown)
	resp, _ := s.Check(t.Context(), req{headers: map[string]string{"accept": browser, bypassHeader: testToken}}.build())
	if statusOf(resp) != 0 || !stripsBypass(resp) {
		t.Fatalf("valid token: want pass with the header stripped, got %d", statusOf(resp))
	}
	resp, _ = s.Check(t.Context(), req{headers: map[string]string{"accept": browser, bypassHeader: "wrong"}}.build())
	if statusOf(resp) != 503 {
		t.Fatalf("wrong token: want 503, got %d", statusOf(resp))
	}
	// Banner with a (wrong) header present: pass, header stripped.
	s = testServer(t, tWarn)
	resp, _ = s.Check(t.Context(), req{headers: map[string]string{bypassHeader: "wrong"}}.build())
	if statusOf(resp) != 0 || !stripsBypass(resp) || respHeader(resp, infoHeader) == "" {
		t.Fatal("banner must strip any bypass header and keep its info header")
	}
	// No snapshot route: still strip.
	resp, _ = s.Check(t.Context(), req{route: "x/y", headers: map[string]string{bypassHeader: testToken}}.build())
	if !stripsBypass(resp) {
		t.Fatal("the bypass header must never reach an app on an armed route")
	}
}

func TestCheckStatusEndpoint(t *testing.T) {
	s := testServer(t, tDown)
	body := func(resp *authv3.CheckResponse) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(resp.GetDeniedResponse().GetBody()), &m); err != nil {
			t.Fatalf("status body: %v", err)
		}
		return m
	}
	resp, _ := s.Check(t.Context(), req{path: statusPath + "?locale=de-CH"}.build())
	m := body(resp)
	if statusOf(resp) != 200 || m["action"] != "block" || m["locale"] != "de" || m["title"] != "Gleich zurück" || respHeader(resp, infoHeader) != "1" || respHeader(resp, "cache-control") != "no-store" {
		t.Fatalf("status: %d %v", statusOf(resp), m)
	}
	resp, _ = s.Check(t.Context(), req{method: "HEAD", path: statusPath}.build())
	if statusOf(resp) != 200 || resp.GetDeniedResponse().GetBody() != "" || respHeader(resp, infoHeader) != "1" {
		t.Fatal("HEAD: same headers, no body")
	}
	resp, _ = s.Check(t.Context(), req{method: "POST", path: statusPath}.build())
	if statusOf(resp) != 405 {
		t.Fatalf("POST: %d", statusOf(resp))
	}
	resp, _ = s.Check(t.Context(), req{path: statusPath, headers: map[string]string{bypassHeader: testToken}}.build())
	if body(resp)["action"] != "none" {
		t.Fatal("a valid bypass sees action none")
	}
	s = testServer(t, tEnd)
	resp, _ = s.Check(t.Context(), req{path: statusPath}.build())
	if body(resp)["action"] != "none" {
		t.Fatal("after the window: action none")
	}
}

func TestCheckNotReady(t *testing.T) {
	s := &Server{}
	resp, _ := s.Check(t.Context(), req{headers: map[string]string{"accept": browser}}.build())
	if statusOf(resp) != 0 || s.Ready() {
		t.Fatal("no snapshot yet: fail open")
	}
}

func TestCookieValue(t *testing.T) {
	cases := []struct{ header, want string }{
		{"mm_seen_x=abc", "abc"},
		{"a=1; mm_seen_x=abc; b=2", "abc"},
		{"a=1;mm_seen_x=abc", "abc"},
		{"xmm_seen_x=abc", ""},
		{"xmm_seen_x=no; mm_seen_x=yes", "yes"},
		{"a=1,mm_seen_x=abc", "abc"},
		{"mm_seen_x=abc,b=2", "abc"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := cookieValue(tc.header, "mm_seen_x="); got != tc.want {
			t.Errorf("cookieValue(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

func FuzzCookieValue(f *testing.F) {
	for _, s := range []string{"mm_seen_x=abc", "a=1; mm_seen_x=abc; b=2", ";;;", "mm_seen_x=", "mm_seen_x=mm_seen_x="} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, h string) {
		v := cookieValue(h, "mm_seen_x=")
		if v != "" && !strings.Contains(h, v) {
			t.Fatalf("value %q not in header %q", v, h)
		}
	})
}

// ---- hot-path budget (§6) ----

func hotRequests() map[string]*authv3.CheckRequest {
	return map[string]*authv3.CheckRequest{
		"block html":   req{headers: map[string]string{"accept": browser, "accept-language": "de-CH,de;q=0.9,en;q=0.8"}}.build(),
		"block json":   req{headers: map[string]string{"accept": "application/json"}}.build(),
		"status":       req{path: statusPath + "?locale=de"}.build(),
		"bypass":       req{headers: map[string]string{bypassHeader: testToken}}.build(),
		"long token":   req{headers: map[string]string{bypassHeader: strings.Repeat("f", 64)}}.build(),
		"other route":  req{route: "a/b"}.build(),
		"cookie+block": req{headers: map[string]string{"accept": browser, "cookie": "a=1; b=2; c=3"}}.build(),
	}
}

func TestCheckAllocs(t *testing.T) {
	for _, now := range []time.Time{tWarn, tInt, tDown} {
		s := testServer(t, now)
		for name, r := range hotRequests() {
			ctx := t.Context()
			if n := testing.AllocsPerRun(500, func() { _, _ = s.Check(ctx, r) }); n > 2 {
				t.Errorf("%s at %s: %.0f allocs per Check; the budget is 2", name, now.Format("15:04"), n)
			}
		}
	}
}

func BenchmarkCheck(b *testing.B) {
	s := testServer(b, tDown)
	r := hotRequests()["block html"]
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = s.Check(ctx, r)
	}
}

// TestCheckBudget is the CI benchmark gate (§13.1): at most 2 allocs/op and
// under 50 µs/op. It runs only with MM_BENCH_GATE=1 (the test job sets it),
// because -race and loaded laptops distort the time.
func TestCheckBudget(t *testing.T) {
	if os.Getenv("MM_BENCH_GATE") != "1" {
		t.Skip("set MM_BENCH_GATE=1 to run the benchmark gate")
	}
	res := testing.Benchmark(BenchmarkCheck)
	ns, allocs := res.NsPerOp(), res.AllocsPerOp()
	t.Logf("Check: %d ns/op, %d allocs/op", ns, allocs)
	if allocs > 2 {
		t.Errorf("%d allocs/op; the budget is 2", allocs)
	}
	if ns > 50_000 {
		t.Errorf("%d ns/op; the CI budget is 50 µs", ns)
	}
}

func TestCheckMorePaths(t *testing.T) {
	noRoute := func(headers map[string]string) *authv3.CheckRequest {
		r := request("GET", headers["accept"], nil)
		for k, v := range headers {
			r.Attributes.Request.Http.Headers[k] = v
		}
		return r
	}
	s := testServer(t, tDown)
	if resp, _ := s.Check(t.Context(), noRoute(map[string]string{"accept": browser})); statusOf(resp) != 0 || stripsBypass(resp) {
		t.Fatal("no route metadata: plain pass")
	}
	if resp, _ := s.Check(t.Context(), noRoute(map[string]string{bypassHeader: "x"})); statusOf(resp) != 0 || !stripsBypass(resp) {
		t.Fatal("no route metadata with a bypass header: pass, header stripped")
	}
	if resp, _ := s.Check(t.Context(), req{path: "//__maintenance-manager/status"}.build()); statusOf(resp) != 200 {
		t.Fatal("an unnormalized status path still answers")
	}
	if resp, _ := s.Check(t.Context(), req{path: "/x?y=1", headers: map[string]string{"accept": browser}}.build()); statusOf(resp) != 503 {
		t.Fatal("a query string must not hide the block")
	}
	if resp, _ := s.Check(t.Context(), req{method: "HEAD", path: statusPath, headers: map[string]string{bypassHeader: testToken}}.build()); resp != statusNoneHead.authz {
		t.Fatal("HEAD status with a valid bypass: none, no body")
	}
	resp, _ := s.Check(t.Context(), req{path: statusPath, headers: map[string]string{bypassHeader: "wrong"}}.build())
	if !strings.Contains(resp.GetDeniedResponse().GetBody(), `"action":"block"`) {
		t.Fatal("status with a wrong token still reports the block")
	}
	s = testServer(t, tInt)
	resp, _ = s.Check(t.Context(), req{path: statusPath}.build())
	if !strings.Contains(resp.GetDeniedResponse().GetBody(), `"action":"interstitial"`) {
		t.Fatalf("status during the interstitial: %s", resp.GetDeniedResponse().GetBody())
	}
	// Not ready yet: never forward a bypass token.
	if resp, _ := (&Server{}).Check(t.Context(), req{headers: map[string]string{bypassHeader: testToken}}.build()); !stripsBypass(resp) {
		t.Fatal("no snapshot yet: the bypass header must still be stripped")
	}
}

func TestRenderFallback(t *testing.T) {
	// A template that passes the parse-time sample (en) but fails for "de"
	// falls back to the built-in page for that locale only.
	page := mmv1.MaintenancePage{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: mmv1.MaintenancePageSpec{
		FallbackLocale: "en",
		HTML:           `<p>{{if eq .Locale "de"}}{{lt .Title 1}}{{end}}custom {{.Title}}</p>`,
		Locales:        map[string]mmv1.PageText{"en": {Title: "Back soon"}, "de": {Title: "Gleich zurück"}},
	}}
	svc := mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "shop"}}}}
	w := mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Name: "w", UID: "u"}, Spec: mmv1.MaintenanceWindowSpec{Enabled: true,
		Phases: []mmv1.Phase{{Name: "down", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"shop"}, Page: "p"}}}}}}
	snap := snapshot.Build(snapshot.Input{Windows: []mmv1.MaintenanceWindow{w}, Pages: []mmv1.MaintenancePage{page},
		Services: []mmv1.MaintenanceService{svc}, Now: tDown,
		Routes: []snapshot.RouteInfo{{Key: snapshot.RouteKey{Namespace: "shop", Name: "web"}, Labels: map[string]string{"app": "shop"}}}})
	if snap.Routes[snapshot.RouteKey{Namespace: "shop", Name: "web"}].Timeline[0].Page.Name != "p" {
		t.Fatal("the template must pass the parse-time sample (en) for this test")
	}
	s := &Server{Now: func() time.Time { return tDown }}
	s.Update(snap)
	en, _ := s.Check(t.Context(), req{headers: map[string]string{"accept": browser}}.build())
	de, _ := s.Check(t.Context(), req{headers: map[string]string{"accept": browser, "accept-language": "de"}}.build())
	if !strings.Contains(en.GetDeniedResponse().GetBody(), "custom Back soon") {
		t.Fatalf("en: want the custom page, got %q", en.GetDeniedResponse().GetBody())
	}
	if body := de.GetDeniedResponse().GetBody(); strings.Contains(body, "custom") || !strings.Contains(body, "Gleich zurück") {
		t.Fatalf("de: want the built-in page with the German title, got %q", body)
	}
}

func TestConstantTimeEqual(t *testing.T) {
	long := strings.Repeat("ab", 40)
	for _, tc := range []struct {
		a, b string
		want bool
	}{{"", "", true}, {"x", "x", true}, {long, long, true}, {"x", "y", false}, {"x", "xy", false}, {long, long[:79] + "c", false}} {
		if got := constantTimeEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("constantTimeEqual(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}
