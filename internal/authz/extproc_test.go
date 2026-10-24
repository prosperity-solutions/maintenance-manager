package authz

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	procmodev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/prometheus/client_golang/prometheus/testutil"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/metrics"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// procClient serves s over an in-memory gRPC connection, as Envoy's ext_proc
// filter would call it.
func procClient(t *testing.T, s *Server) extprocv3.ExternalProcessorClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.MaxRecvMsgSize(maxRecvMsgSize))
	extprocv3.RegisterExternalProcessorServer(srv, procServer{s: s})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return extprocv3.NewExternalProcessorClient(conn)
}

// procReq is one request's headers as Envoy sends them, with the route name
// Envoy Gateway gives an HTTPRoute's first rule.
func procReq(r req) *extprocv3.ProcessingRequest {
	if r.method == "" {
		r.method = "GET"
	}
	if r.path == "" {
		r.path = "/"
	}
	if r.route == "" {
		r.route = "shop/web"
	}
	hs := []*corev3.HeaderValue{
		{Key: ":method", RawValue: []byte(r.method)},
		{Key: ":path", RawValue: []byte(r.path)},
		{Key: ":authority", RawValue: []byte("shop.example")},
	}
	for k, v := range r.headers {
		hs = append(hs, &corev3.HeaderValue{Key: k, RawValue: []byte(v)})
	}
	attrs := map[string]*structpb.Struct{}
	if r.route != "-" {
		ns, name, _ := strings.Cut(r.route, "/")
		attrs["envoy.filters.http.ext_proc"] = &structpb.Struct{Fields: map[string]*structpb.Value{
			attrRouteName: structpb.NewStringValue("httproute/" + ns + "/" + name + "/rule/0/match/0/shop_example"),
		}}
	}
	return &extprocv3.ProcessingRequest{
		Request:    &extprocv3.ProcessingRequest_RequestHeaders{RequestHeaders: &extprocv3.HttpHeaders{Headers: &corev3.HeaderMap{Headers: hs}}},
		Attributes: attrs,
	}
}

// exchange sends one request's headers and, when Envoy would (no mode
// override to SKIP and no immediate response), its response headers.
func exchange(t *testing.T, c extprocv3.ExternalProcessorClient, r req) (first, second *extprocv3.ProcessingResponse) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := c.Process(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(procReq(r)); err != nil {
		t.Fatal(err)
	}
	first, err = stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if first.GetImmediateResponse() == nil && first.GetModeOverride().GetResponseHeaderMode() != procmodev3.ProcessingMode_SKIP {
		if err := stream.Send(&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extprocv3.HttpHeaders{Headers: &corev3.HeaderMap{}},
		}}); err != nil {
			t.Fatal(err)
		}
		if second, err = stream.Recv(); err != nil {
			t.Fatal(err)
		}
	}
	_ = stream.CloseSend()
	return first, second
}

// rawHeader finds a header in an ext_proc mutation. ext_proc carries values
// in raw_value; a value in value is a bug (Envoy would drop it), so it fails.
func rawHeader(t *testing.T, hs []*corev3.HeaderValueOption, name string) string {
	t.Helper()
	for _, h := range hs {
		if h.GetHeader().GetValue() != "" {
			t.Errorf("header %s uses value, not raw_value", h.GetHeader().GetKey())
		}
		if strings.EqualFold(h.GetHeader().GetKey(), name) {
			return string(h.GetHeader().GetRawValue())
		}
	}
	return ""
}

func immediateHeader(t *testing.T, resp *extprocv3.ProcessingResponse, name string) string {
	t.Helper()
	return rawHeader(t, resp.GetImmediateResponse().GetHeaders().GetSetHeaders(), name)
}

func addedResponseHeader(t *testing.T, resp *extprocv3.ProcessingResponse, name string) string {
	t.Helper()
	return rawHeader(t, resp.GetResponseHeaders().GetResponse().GetHeaderMutation().GetSetHeaders(), name)
}

func removesBypass(resp *extprocv3.ProcessingResponse) bool {
	for _, h := range resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetRemoveHeaders() {
		if h == bypassHeader {
			return true
		}
	}
	return false
}

func TestProcessActions(t *testing.T) {
	cases := []struct {
		name  string
		now   time.Time
		r     req
		check func(t *testing.T, first, second *extprocv3.ProcessingResponse)
	}{
		{"before the window: pass, response headers skipped", tWarn.Add(-time.Minute), req{headers: map[string]string{"accept": browser}},
			func(t *testing.T, first, second *extprocv3.ProcessingResponse) {
				if first.GetRequestHeaders() == nil || second != nil {
					t.Fatalf("want a plain continue that skips the response, got %v / %v", first, second)
				}
			}},
		{"banner: pass, then the info header on the response", tWarn, req{headers: map[string]string{"accept": browser}},
			func(t *testing.T, first, second *extprocv3.ProcessingResponse) {
				if first.GetRequestHeaders() == nil || first.GetModeOverride() != nil {
					t.Fatalf("want continue without override, got %v", first)
				}
				if got := addedResponseHeader(t, second, infoHeader); got != "window=db; phase=warn; action=banner" {
					t.Fatalf("info header %q", got)
				}
			}},
		{"interstitial: page for a browser", tInt, req{headers: map[string]string{"accept": browser, "accept-language": "de-CH,de;q=0.9"}},
			func(t *testing.T, first, _ *extprocv3.ProcessingResponse) {
				ir := first.GetImmediateResponse()
				if ir.GetStatus().GetCode() != 200 || !strings.Contains(string(ir.GetBody()), "Gleich zurück") ||
					!strings.HasPrefix(immediateHeader(t, first, "set-cookie"), snapshot.CookieName("uid-db")+"=") || immediateHeader(t, first, "refresh") != "8" {
					t.Fatalf("interstitial: %v", ir)
				}
			}},
		{"interstitial: pass once the cookie is set", tInt, req{headers: map[string]string{"accept": browser,
			"cookie": snapshot.CookieName("uid-db") + "=" + snapshot.PhaseKey(mmv1.Phase{Name: "hello", StartsAt: mt(tInt)})}},
			func(t *testing.T, first, second *extprocv3.ProcessingResponse) {
				if first.GetImmediateResponse() != nil || addedResponseHeader(t, second, infoHeader) == "" {
					t.Fatalf("want a pass with the info header, got %v / %v", first, second)
				}
			}},
		{"block: HTML for a browser", tDown, req{headers: map[string]string{"accept": browser}},
			func(t *testing.T, first, _ *extprocv3.ProcessingResponse) {
				ir := first.GetImmediateResponse()
				if ir.GetStatus().GetCode() != 503 || !strings.Contains(string(ir.GetBody()), "Back soon") ||
					immediateHeader(t, first, "retry-after") != "Fri, 30 Oct 2026 22:30:00 GMT" || immediateHeader(t, first, "cache-control") != "no-store" ||
					!strings.HasPrefix(immediateHeader(t, first, "content-type"), "text/html") {
					t.Fatalf("block html: %v", ir)
				}
			}},
		{"block: problem+json for an API", tDown, req{headers: map[string]string{"accept": "application/json"}},
			func(t *testing.T, first, _ *extprocv3.ProcessingResponse) {
				var p map[string]any
				if err := json.Unmarshal(first.GetImmediateResponse().GetBody(), &p); err != nil || p["window"] != "db" ||
					immediateHeader(t, first, "content-type") != "application/problem+json" {
					t.Fatalf("problem: %v %v", p, err)
				}
			}},
		{"block: a valid bypass passes, header removed", tDown, req{headers: map[string]string{"accept": browser, bypassHeader: testToken}},
			func(t *testing.T, first, second *extprocv3.ProcessingResponse) {
				if first.GetImmediateResponse() != nil || !removesBypass(first) || second != nil {
					t.Fatalf("bypass: %v / %v", first, second)
				}
			}},
		{"block: a wrong bypass is blocked", tDown, req{headers: map[string]string{"accept": browser, bypassHeader: "wrong"}},
			func(t *testing.T, first, _ *extprocv3.ProcessingResponse) {
				if first.GetImmediateResponse().GetStatus().GetCode() != 503 {
					t.Fatalf("wrong token: %v", first)
				}
			}},
		{"banner: any bypass header is removed, info header kept", tWarn, req{headers: map[string]string{bypassHeader: "wrong"}},
			func(t *testing.T, first, second *extprocv3.ProcessingResponse) {
				if !removesBypass(first) || addedResponseHeader(t, second, infoHeader) == "" {
					t.Fatalf("banner strip: %v / %v", first, second)
				}
			}},
		{"status endpoint", tDown, req{path: statusPath + "?locale=de-CH"},
			func(t *testing.T, first, _ *extprocv3.ProcessingResponse) {
				var m map[string]any
				if err := json.Unmarshal(first.GetImmediateResponse().GetBody(), &m); err != nil || m["action"] != "block" || m["locale"] != "de" {
					t.Fatalf("status: %v %v", m, err)
				}
			}},
		{"another route: pass", tDown, req{route: "shop/api", headers: map[string]string{"accept": browser}},
			func(t *testing.T, first, second *extprocv3.ProcessingResponse) {
				if first.GetImmediateResponse() != nil || second != nil {
					t.Fatalf("another route: %v", first)
				}
			}},
		{"no route attribute: pass, bypass still removed", tDown, req{route: "-", headers: map[string]string{"accept": browser, bypassHeader: testToken}},
			func(t *testing.T, first, _ *extprocv3.ProcessingResponse) {
				if first.GetImmediateResponse() != nil || !removesBypass(first) {
					t.Fatalf("no route: %v", first)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, second := exchange(t, procClient(t, testServer(t, tc.now)), tc.r)
			tc.check(t, first, second)
		})
	}
}

func TestProcessNotReady(t *testing.T) {
	first, second := exchange(t, procClient(t, &Server{}), req{headers: map[string]string{"accept": browser, bypassHeader: testToken}})
	if first.GetImmediateResponse() != nil || !removesBypass(first) || second != nil {
		t.Fatalf("no snapshot yet: fail open, never forward a token: %v", first)
	}
}

// The same decisions in both protocols, for every phase and request shape.
func TestProcessMatchesCheck(t *testing.T) {
	shapes := []req{
		{headers: map[string]string{"accept": browser}},
		{headers: map[string]string{"accept": "application/json"}},
		{method: "POST", headers: map[string]string{"accept": browser}},
		{path: statusPath},
		{method: "HEAD", path: statusPath},
		{headers: map[string]string{bypassHeader: testToken}},
		{method: "POST", path: statusPath},
		{headers: map[string]string{"accept": browser, "cookie": "a=1"}},
	}
	for _, now := range []time.Time{tWarn.Add(-time.Minute), tWarn, tInt, tDown, tEnd} {
		s := testServer(t, now)
		c := procClient(t, s)
		for _, r := range shapes {
			authz, _ := s.Check(t.Context(), r.build())
			first, second := exchange(t, c, r)
			wantStatus := statusOf(authz)
			gotStatus := int(first.GetImmediateResponse().GetStatus().GetCode())
			if gotStatus != wantStatus {
				t.Errorf("%s %s %v at %s: ext_proc status %d, Check %d", r.method, r.path, r.headers, now.Format("15:04"), gotStatus, wantStatus)
			}
			if d := authz.GetDeniedResponse(); d != nil {
				if string(first.GetImmediateResponse().GetBody()) != d.GetBody() {
					t.Errorf("%s %s at %s: bodies differ", r.method, r.path, now.Format("15:04"))
				}
				// Every header (Set-Cookie, Retry-After, Refresh, Allow...)
				// survives the conversion to raw_value.
				for _, h := range d.GetHeaders() {
					k := h.GetHeader().GetKey()
					if got := immediateHeader(t, first, k); got != h.GetHeader().GetValue() {
						t.Errorf("%s %s at %s: header %s = %q, Check %q", r.method, r.path, now.Format("15:04"), k, got, h.GetHeader().GetValue())
					}
				}
				if n, m := len(first.GetImmediateResponse().GetHeaders().GetSetHeaders()), len(d.GetHeaders()); n != m {
					t.Errorf("%s %s at %s: %d headers, Check %d", r.method, r.path, now.Format("15:04"), n, m)
				}
			}
			if want, got := respHeader(authz, infoHeader), addedResponseHeader(t, second, infoHeader); wantStatus == 0 && got != want {
				t.Errorf("%s %s at %s: info header %q, Check %q", r.method, r.path, now.Format("15:04"), got, want)
			}
		}
	}
}

func TestProcRouteKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		want RouteKey
		ok   bool
	}{
		{"httproute/shop/web/rule/0/match/0/shop_example", RouteKey{Namespace: "shop", Name: "web"}, true},
		{"httproute/shop/web/rule/1", RouteKey{Namespace: "shop", Name: "web"}, true},
		{"grpcroute/shop/web/rule/0/match/0/x", RouteKey{}, false},
		{"httproute/shop/web", RouteKey{}, false},
		{"httproute//web/rule/0", RouteKey{}, false},
		{"", RouteKey{}, false},
	} {
		attrs := map[string]*structpb.Struct{"envoy.filters.http.ext_proc": {Fields: map[string]*structpb.Value{attrRouteName: structpb.NewStringValue(tc.name)}}}
		got, ok := procRouteKey(attrs)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q: %v %v, want %v %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// The handler returns as soon as Envoy needs nothing more: after an answer,
// after a pass that skips the response headers, and after the response
// headers of a banner pass. A held stream would be a goroutine per request.
func TestProcessEndsStream(t *testing.T) {
	for _, tc := range []struct {
		name string
		now  time.Time
		r    req
	}{
		{"answered", tDown, req{headers: map[string]string{"accept": browser}}},
		{"passed, response skipped", tEnd, req{}},
		{"banner, after the response headers", tWarn, req{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := procClient(t, testServer(t, tc.now))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			stream, err := c.Process(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Send(procReq(tc.r)); err != nil {
				t.Fatal(err)
			}
			first, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if first.GetModeOverride() == nil && first.GetImmediateResponse() == nil {
				_ = stream.Send(&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
					ResponseHeaders: &extprocv3.HttpHeaders{Headers: &corev3.HeaderMap{}},
				}})
				if _, err := stream.Recv(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
				t.Fatalf("want the server to end the stream, got %v", err)
			}
		})
	}
}

// HTTP/2 may send each cookie as its own header; they are read as one.
func TestProcessSplitCookies(t *testing.T) {
	s := testServer(t, tInt)
	r := procReq(req{headers: map[string]string{"accept": browser}})
	hs := r.GetRequestHeaders().GetHeaders()
	hs.Headers = append(hs.Headers,
		&corev3.HeaderValue{Key: "cookie", RawValue: []byte("a=1")},
		&corev3.HeaderValue{Key: "cookie", RawValue: []byte(snapshot.CookieName("uid-db") + "=" + snapshot.PhaseKey(mmv1.Phase{Name: "hello", StartsAt: mt(tInt)}))},
	)
	now, after := procServer{s: s}.requestHeaders(r, r.GetRequestHeaders())
	if now.GetImmediateResponse() != nil || after == nil {
		t.Fatalf("the second cookie header carries the seen cookie: want a pass, got %v", now)
	}
}

// Reading the headers and deciding allocates nothing (§6 budget); gRPC's own
// decoding is outside it.
func TestProcessAllocs(t *testing.T) {
	for _, now := range []time.Time{tWarn, tInt, tDown} {
		p := procServer{s: testServer(t, now)}
		for name, r := range map[string]req{
			"block html": {headers: map[string]string{"accept": browser, "accept-language": "de-CH,de;q=0.9,en;q=0.8", "cookie": strings.Repeat("x", 300)}},
			"block json": {headers: map[string]string{"accept": "application/json"}},
			"status":     {path: statusPath + "?locale=de"},
			"bypass":     {headers: map[string]string{bypassHeader: testToken}},
		} {
			pr := procReq(r)
			if n := testing.AllocsPerRun(500, func() { p.requestHeaders(pr, pr.GetRequestHeaders()) }); n > 0 {
				t.Errorf("%s at %s: %.0f allocs; the budget is 0", name, now.Format("15:04"), n)
			}
		}
	}
}

// A request over maxRecvMsgSize cannot be read: the stream ends with a
// ResourceExhausted status instead of an answer (Envoy fails open), and the
// error is counted.
func TestProcessOversizeIsCounted(t *testing.T) {
	before := testutil.ToFloat64(metrics.CheckError)
	c := procClient(t, testServer(t, tDown))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := c.Process(ctx)
	if err != nil {
		t.Fatal(err)
	}
	big := procReq(req{headers: map[string]string{"accept": browser, "cookie": strings.Repeat("x", maxRecvMsgSize)}})
	_ = stream.Send(big)
	if resp, err := stream.Recv(); err == nil {
		t.Fatalf("want no answer to an oversize request, got %v", resp)
	}
	waitCounter(t, func() bool { return testutil.ToFloat64(metrics.CheckError) == before+1 })
}

func waitCounter(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("the error was not counted")
}
