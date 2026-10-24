package authz

import (
	"fmt"
	"log/slog"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/locale"
	"github.com/prosperity-solutions/maintenance-manager/internal/render"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// Header names. Envoy hands ext_authz lowercased header keys.
const (
	bypassHeader = "x-maintenance-manager-bypass"
	infoHeader   = "X-Maintenance-Manager"
	statusPath   = "/__maintenance-manager/status"
)

// interstitialRefresh is hardcoded on purpose (§11).
const interstitialRefresh = "8"

// lastResortPage is served when even the built-in page cannot render.
const lastResortPage = `<!doctype html><html lang="en"><meta charset="utf-8"><title>Maintenance</title><h1>Maintenance</h1><p>This service is temporarily unavailable.</p></html>`

// table is a compiled snapshot: every response Check() can give, built once.
type table struct {
	routes map[snapshot.RouteKey][]*decision
	bypass string
}

// decision is a snapshot.Decision with its responses.
type decision struct {
	src          *snapshot.Decision
	action       mmv1.Action
	cookiePrefix string // "mm_seen_xxxxxxxx="
	phaseKey     string
	locales      *locale.Matcher

	pass, passStrip *authv3.CheckResponse            // OK + X-Maintenance-Manager (banner, interstitial pass)
	page            map[string]*authv3.CheckResponse // per locale: interstitial page or block HTML
	problem         map[string]*authv3.CheckResponse // per locale: block problem+json
	status          map[string]*authv3.CheckResponse // per locale: status JSON (GET)
	statusHead      map[string]*authv3.CheckResponse // per locale: status headers only (HEAD)
}

// Responses that do not depend on a snapshot.
var (
	okStripResponse = &authv3.CheckResponse{
		Status: okResponse.Status,
		HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{
			HeadersToRemove: []string{bypassHeader},
		}},
	}
	statusNone     = statusResponse(render.StatusJSON("none", "", "", "", "", time.Time{}, ""), true)
	statusNoneHead = statusResponse("", false)
	statusMethod   = denied(typev3.StatusCode_MethodNotAllowed, "text/plain; charset=utf-8", "405 method not allowed: use GET or HEAD\n",
		header("allow", "GET, HEAD"), header(infoHeader, "1"))
)

func statusResponse(body string, withBody bool) *authv3.CheckResponse {
	if !withBody {
		body = ""
	}
	return denied(typev3.StatusCode_OK, "application/json", body, header(infoHeader, "1"))
}

// compile builds the response table for a snapshot. A page that fails to
// render for a locale falls back to the built-in page, so compile never fails.
func compile(snap *snapshot.Snapshot, log *slog.Logger) *table {
	t := &table{routes: make(map[snapshot.RouteKey][]*decision, len(snap.Routes)), bypass: snap.Bypass}
	// Decisions are shared between routes of the same window and phase;
	// compile each once.
	done := map[*snapshot.Decision]*decision{}
	for key, r := range snap.Routes {
		tl := make([]*decision, 0, len(r.Timeline))
		for _, d := range r.Timeline {
			cd, ok := done[d]
			if !ok {
				cd = compileDecision(d, log)
				done[d] = cd
			}
			tl = append(tl, cd)
		}
		t.routes[key] = tl
	}
	return t
}

func compileDecision(d *snapshot.Decision, log *slog.Logger) *decision {
	cd := &decision{
		src:          d,
		action:       d.Action,
		cookiePrefix: d.Cookie + "=",
		phaseKey:     d.PhaseKey,
		locales:      d.Page.Locales,
		page:         map[string]*authv3.CheckResponse{},
		problem:      map[string]*authv3.CheckResponse{},
		status:       map[string]*authv3.CheckResponse{},
		statusHead:   map[string]*authv3.CheckResponse{},
	}
	info := header(infoHeader, fmt.Sprintf("window=%s; phase=%s; action=%s", d.Window, d.Phase, d.Action))
	cd.pass = &authv3.CheckResponse{
		Status: okResponse.Status,
		HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{
			ResponseHeadersToAdd: []*corev3.HeaderValueOption{info},
		}},
	}
	cd.passStrip = &authv3.CheckResponse{
		Status: okResponse.Status,
		HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{
			ResponseHeadersToAdd: []*corev3.HeaderValueOption{info},
			HeadersToRemove:      []string{bypassHeader},
		}},
	}

	for _, loc := range d.Page.Locales.Locales() {
		text := d.Page.Text(loc)
		cd.status[loc] = statusResponse(render.StatusJSON(string(d.Action), d.Window, d.Phase, text.Title, text.Message, d.Until, loc), true)
		cd.statusHead[loc] = statusResponse("", false)

		switch d.Action {
		case mmv1.ActionInterstitial:
			body := renderPage(d, loc, text, true, log)
			cd.page[loc] = denied(typev3.StatusCode_OK, "text/html; charset=utf-8", body,
				header("set-cookie", d.Cookie+"="+d.PhaseKey+"; Path=/; HttpOnly; SameSite=Lax"),
				header("refresh", interstitialRefresh))
		case mmv1.ActionBlock:
			var extra []*corev3.HeaderValueOption
			if ra := render.RetryAfter(d.RetryAfter); ra != "" {
				extra = append(extra, header("retry-after", ra))
			}
			cd.page[loc] = denied(typev3.StatusCode_ServiceUnavailable, "text/html; charset=utf-8",
				renderPage(d, loc, text, false, log), extra...)
			cd.problem[loc] = denied(typev3.StatusCode_ServiceUnavailable, "application/problem+json",
				render.ProblemJSON(text.Title, text.Message, d.Window, d.Until), extra...)
		}
	}
	return cd
}

func renderPage(d *snapshot.Decision, loc string, text mmv1.PageText, interstitial bool, log *slog.Logger) string {
	in := render.Input{Locale: loc, Title: text.Title, Message: text.Message, Until: d.Until, Interstitial: interstitial}
	body, err := d.Page.Template.Render(in)
	if err == nil {
		return body
	}
	if log != nil {
		log.Warn("page failed to render; serving the built-in page", "page", d.Page.Name, "locale", loc, "err", err)
	}
	body, err = render.Default.Render(in)
	if err != nil {
		// Only oversized texts get here; never serve them raw.
		return lastResortPage
	}
	return body
}
