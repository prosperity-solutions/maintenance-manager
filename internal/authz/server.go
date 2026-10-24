// Package authz serves Envoy's gRPC ext_authz Check() and, for routes armed
// through an EnvoyExtensionPolicy, ext_proc Process() (docs/HANDOVER.md §6).
//
// The hot path does no I/O, takes no locks and logs nothing per request. Every
// answer is precomputed and shared (a CheckResponse or a ProcessingResponse):
// gRPC only reads it.
package authz

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/metrics"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// Addr is the authz listen address. Hardcoded on purpose (§11): the
// SecurityPolicy and the Service both name port 9001.
const Addr = ":9001"

// gracefulStopTimeout caps GracefulStop on shutdown (§11).
const gracefulStopTimeout = 5 * time.Second

// DrainDelay is how long Check() keeps answering after shutdown starts and
// /readyz fails, so Envoy moves to other endpoints before this one closes
// (§11 SIGTERM: fail /readyz, keep serving 10 s, then GracefulStop).
const DrainDelay = 10 * time.Second

// maxRecvMsgSize bounds one request message. ext_authz sends only the headers
// listed in headersToExtAuth; ext_proc sends every request header (never the
// body), and Envoy caps request headers at 60 KiB by default.
const maxRecvMsgSize = 128 << 10

// okResponse lets a request through, untouched.
var okResponse = &authv3.CheckResponse{
	Status:       &status.Status{Code: int32(codes.OK)},
	HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{}},
}

// Server implements envoy.service.auth.v3.Authorization.
type Server struct {
	authv3.UnimplementedAuthorizationServer
	Addr string
	Log  *slog.Logger

	// OnShutdown runs when shutdown starts, before the drain; serve uses it
	// to fail /readyz.
	OnShutdown func()
	// Drain is how long to keep serving after OnShutdown (DrainDelay in
	// production).
	Drain time.Duration

	// Now is the clock (time.Now when nil). Check() decides by it (§4.4).
	Now func() time.Time

	tbl atomic.Pointer[table]
}

// Update compiles a snapshot and swaps it in atomically. Check() keeps
// answering from the previous table until the swap.
func (s *Server) Update(snap *snapshot.Snapshot) {
	s.tbl.Store(compile(snap, s.Log))
}

// Ready reports whether a snapshot has been loaded.
func (s *Server) Ready() bool { return s.tbl.Load() != nil }

// Check answers whether a request may pass (§6). No I/O, no locks, no
// logging: every answer is a precomputed response from the current table.
func (s *Server) Check(_ context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	http := req.GetAttributes().GetRequest().GetHttp()
	h := http.GetHeaders()
	key, found := routeKey(req)
	token, hasBypass := h[bypassHeader]
	v := s.decide(key, found, reqInfo{
		method: http.GetMethod(), path: http.GetPath(),
		accept: h["accept"], acceptLanguage: h["accept-language"], cookie: h["cookie"],
		bypass: token, hasBypass: hasBypass,
	})
	switch v.kind {
	case verdictPass:
		if v.strip {
			return okStripResponse, nil
		}
		return okResponse, nil
	case verdictInfo:
		return v.d.passFor(v.strip), nil
	}
	return v.answer.authz, nil
}

// verdictKind says how a request is answered; each protocol maps it to its
// own precomputed response.
type verdictKind uint8

const (
	verdictPass   verdictKind = iota // to the app, untouched
	verdictInfo                      // to the app, with X-Maintenance-Manager on the response
	verdictAnswer                    // answered here: page, problem+json or status
)

type verdict struct {
	kind   verdictKind
	strip  bool // a bypass header came in: remove it before the app
	d      *decision
	answer answer
}

// reqInfo is what a decision reads from a request.
type reqInfo struct {
	method, path                   string // path with the query
	accept, acceptLanguage, cookie string
	bypass                         string
	hasBypass                      bool // the bypass header came, even empty
}

// decide is the decision of §6, shared by Check() and ext_proc. It allocates
// nothing and keeps no reference to r after it returns.
func (s *Server) decide(key RouteKey, found bool, r reqInfo) verdict {
	hasBypassHdr := r.hasBypass
	tbl := s.tbl.Load()
	if tbl == nil { // not ready: fail open, but never forward a bypass token
		metrics.CheckNotReady.Inc()
		return verdict{kind: verdictPass, strip: hasBypassHdr}
	}
	pass := verdict{kind: verdictPass, strip: hasBypassHdr}
	path, query, _ := strings.Cut(r.path, "?")
	bypass := hasBypassHdr && tbl.bypass != "" && constantTimeEqual(r.bypass, tbl.bypass)

	if !found {
		metrics.CheckPass.Inc()
		return pass
	}
	timeline := tbl.routes[key]
	if timeline == nil {
		metrics.CheckPass.Inc()
		return pass
	}
	d := at(timeline, s.now())

	// "//__maintenance-manager/status" too, in case :path was not normalized.
	if path == statusPath || (len(path) > len(statusPath) && strings.TrimLeft(path, "/") == statusPath[1:]) {
		metrics.CheckStatus.Inc()
		return verdict{kind: verdictAnswer, answer: statusFor(d, r.method, query, r.acceptLanguage, bypass)}
	}
	if bypass {
		metrics.CheckBypass.Inc()
		return pass
	}
	if d == nil {
		metrics.CheckPass.Inc()
		return pass
	}

	html := wantsHTML(r.method, r.accept)
	info := verdict{kind: verdictInfo, strip: hasBypassHdr, d: d}
	switch d.action {
	case mmv1.ActionBanner:
		metrics.CheckBanner.Inc()
		return info
	case mmv1.ActionInterstitial:
		if !html || cookieValue(r.cookie, d.cookiePrefix) == d.phaseKey {
			metrics.CheckInterstitialPass.Inc()
			return info
		}
		metrics.CheckInterstitialPage.Inc()
		return verdict{kind: verdictAnswer, answer: d.page[d.locales.Resolve("", r.acceptLanguage)]}
	case mmv1.ActionBlock:
		loc := d.locales.Resolve("", r.acceptLanguage)
		if html {
			metrics.CheckBlockHTML.Inc()
			return verdict{kind: verdictAnswer, answer: d.page[loc]}
		}
		metrics.CheckBlockProblem.Inc()
		return verdict{kind: verdictAnswer, answer: d.problem[loc]}
	}
	metrics.CheckPass.Inc()
	return pass
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (d *decision) passFor(hasBypassHdr bool) *authv3.CheckResponse {
	if hasBypassHdr {
		return d.passStrip
	}
	return d.pass
}

// at returns the decision at t. Timelines hold a few entries.
func at(timeline []*decision, t time.Time) *decision {
	for _, d := range timeline {
		if d.src.Contains(t) {
			return d
		}
	}
	return nil
}

// statusFor answers GET/HEAD /__maintenance-manager/status (§7).
func statusFor(d *decision, method, query, acceptLanguage string, bypass bool) answer {
	head := method == "HEAD"
	if !head && method != "GET" {
		return statusMethod
	}
	if d == nil || bypass {
		if head {
			return statusNoneHead
		}
		return statusNone
	}
	loc := d.locales.Resolve(queryParam(query, "locale"), acceptLanguage)
	if head {
		return d.statusHead[loc]
	}
	return d.status[loc]
}

// queryParam returns the raw value of name in a query string, without
// allocating. Locale tags need no unescaping.
func queryParam(query, name string) string {
	for query != "" {
		var kv string
		kv, query, _ = strings.Cut(query, "&")
		k, v, _ := strings.Cut(kv, "=")
		if k == name {
			return v
		}
	}
	return ""
}

// constantTimeEqual compares two strings in time independent of their
// content, without converting them to []byte (which allocates for long
// tokens). Only the length can leak, as with crypto/subtle.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return subtle.ConstantTimeByteEq(v, 0) == 1
}

// cookieValue finds "name=" in a Cookie header at a cookie boundary and
// returns its value, without allocating.
func cookieValue(header, prefix string) string {
	for header != "" {
		i := strings.Index(header, prefix)
		if i < 0 {
			return ""
		}
		// ",": Envoy joins repeated Cookie headers with a comma.
		if i == 0 || header[i-1] == ' ' || header[i-1] == ';' || header[i-1] == ',' {
			v := header[i+len(prefix):]
			if j := strings.IndexAny(v, ";,"); j >= 0 {
				v = v[:j]
			}
			return strings.TrimSpace(v)
		}
		header = header[i+len(prefix):]
	}
	return ""
}

// NeedLeaderElection reports false: every replica answers Check().
func (s *Server) NeedLeaderElection() bool { return false }

// Start serves until ctx is done, then stops gracefully within
// gracefulStopTimeout. It implements controller-runtime's manager.Runnable.
func (s *Server) Start(ctx context.Context) error {
	addr := s.Addr
	if addr == "" {
		addr = Addr
	}
	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("authz: listen on %s: %w", addr, err)
	}
	srv := grpc.NewServer(
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.MaxRecvMsgSize(maxRecvMsgSize),
		grpc.MaxConcurrentStreams(1024),
	)
	authv3.RegisterAuthorizationServer(srv, s)
	extprocv3.RegisterExternalProcessorServer(srv, procServer{s: s})

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(lis) }()
	s.Log.Info("authz serving", "addr", lis.Addr().String())

	select {
	case err := <-errc:
		return fmt.Errorf("authz: serve: %w", err)
	case <-ctx.Done():
	}
	if s.OnShutdown != nil {
		s.OnShutdown()
	}
	if s.Drain > 0 {
		s.Log.Info("authz draining", "for", s.Drain.String())
		time.Sleep(s.Drain)
	}
	stopped := make(chan struct{})
	go func() { srv.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(gracefulStopTimeout):
		srv.Stop()
	}
	if err := <-errc; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("authz: serve: %w", err)
	}
	return nil
}
