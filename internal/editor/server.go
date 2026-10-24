package editor

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/prosperity-solutions/maintenance-manager/internal/controller"
)

// Addr is the editor's port (§10, §11).
const Addr = ":8080"

// MinPassword is the shortest editor password the editor accepts.
const MinPassword = 12

const (
	user        = "admin"
	tzCookie    = "mm_tz"
	maxBody     = 4 << 20 // a 64 KiB page of non-ASCII text triples when urlencoded, plus 32 locales
	viewMemo    = 2 * time.Second
	viewTimeout = 10 * time.Second
)

// tzScript is the editor's only JavaScript (§10): it fills every hidden tz
// field with the browser's time zone. The CSP allows exactly this script by
// its hash.
const tzScript = `document.querySelectorAll("input[name=tz]").forEach(function(e){e.value=Intl.DateTimeFormat().resolvedOptions().timeZone})`

var (
	//go:embed templates/*.html
	templateFS embed.FS
	pages      = template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html"))
	csp        = "default-src 'none'; script-src 'sha256-" + scriptHash() + "'; style-src 'unsafe-inline'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
)

func scriptHash() string {
	h := sha256.Sum256([]byte(tzScript))
	return base64.StdEncoding.EncodeToString(h[:])
}

// Server is the editor (§10): server-rendered forms on port 8080, behind
// HTTP basic auth. Every replica serves it; it reads through the API (not the
// leader's caches), so any replica a port-forward reaches shows the same.
type Server struct {
	Addr      string
	Namespace string // release namespace: our CRs live here
	Password  string
	Client    client.Client // writes, and lists of our CRs and HTTPRoutes from the shared cache
	API       client.Reader // uncached reads: edit forms, matched routes with hostnames
	// View computes the controller's picture (armed routes, conflicts, Envoy
	// Accepted). Nil leaves those parts of the dashboard empty.
	View func(context.Context) (controller.View, error)
	Lead time.Duration
	Log  *slog.Logger
	Now  func() time.Time

	viewMu  sync.Mutex
	viewAt  time.Time
	viewVal controller.View
	viewErr error
}

// NeedLeaderElection: every replica serves the editor.
func (s *Server) NeedLeaderElection() bool { return false }

// Start serves until ctx is done. It never returns an error: a broken editor
// must not take authz down with the manager, so it logs and waits instead.
func (s *Server) Start(ctx context.Context) error {
	if s.Password == "" {
		s.Log.Error("editor off: MM_EDITOR_PASSWORD is empty. The chart sets it from editor.password or editor.existingSecret; authz keeps serving")
		<-ctx.Done()
		return nil
	}
	if len(s.Password) < MinPassword {
		s.Log.Error("editor off: its password is shorter than 12 characters (check editor.existingSecret); whoever holds it can serve any HTML on every gated app's origin. authz keeps serving")
		<-ctx.Done()
		return nil
	}
	addr := s.Addr
	if addr == "" {
		addr = Addr
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.Log.Info("editor listening", "addr", addr)
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			s.Log.Error("editor stopped; authz keeps serving", "err", err)
		}
		<-ctx.Done()
	}
	return nil
}

// Handler is the editor's HTTP handler with every protection applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("POST /tz", s.setTZ)

	mux.HandleFunc("GET /services", s.listServices)
	mux.HandleFunc("GET /services/-/new", s.newService)
	mux.HandleFunc("POST /services/-/new", s.createService)
	mux.HandleFunc("GET /services/{name}", s.editService)
	mux.HandleFunc("POST /services/{name}", s.updateService)
	mux.HandleFunc("POST /services/{name}/delete", s.deleteService)

	mux.HandleFunc("GET /pages", s.listPages)
	mux.HandleFunc("GET /pages/-/new", s.newPage)
	mux.HandleFunc("POST /pages/-/new", s.createPage)
	mux.HandleFunc("POST /pages/-/preview", s.previewForm)
	mux.HandleFunc("GET /pages/{name}", s.editPage)
	mux.HandleFunc("GET /pages/{name}/preview", s.previewSaved)
	mux.HandleFunc("POST /pages/{name}", s.updatePage)
	mux.HandleFunc("POST /pages/{name}/delete", s.deletePage)

	mux.HandleFunc("GET /windows", s.listWindows)
	mux.HandleFunc("GET /windows/-/new", s.newWindow)
	mux.HandleFunc("POST /windows/-/new", s.createWindow)
	mux.HandleFunc("GET /windows/{name}", s.editWindow)
	mux.HandleFunc("POST /windows/{name}", s.updateWindow)
	mux.HandleFunc("POST /windows/{name}/delete", s.deleteWindow)
	mux.HandleFunc("POST /windows/{name}/enabled", s.setEnabled)
	mux.HandleFunc("POST /windows/{name}/next", s.startNext)

	cop := http.NewCrossOriginProtection()
	return headers(s.auth(csrf(cop, mux)))
}

// headers sets the security headers on every response, before anything
// else can write.
func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// auth is HTTP basic auth for user admin. Both values are hashed before the
// constant-time compare, so neither length leaks.
//
// Guessing is limited to one attempt per second per replica, checked before
// the compare: a request with credentials takes the one token, or gets 429 at
// once without being compared (so a flood learns nothing and parks no
// goroutines). A correct login gives its token back, so the admin is only
// slowed while someone is guessing.
func (s *Server) auth(h http.Handler) http.Handler {
	want := sha256.Sum256([]byte(s.Password))
	wantUser := sha256.Sum256([]byte(user))
	var next atomic.Int64 // unix nanos before which no guess is compared
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="maintenance-manager", charset="UTF-8"`)
			http.Error(w, "login required (user admin)", http.StatusUnauthorized)
			return
		}
		// Take the slot: only one request per second gets compared.
		now, prev := time.Now().UnixNano(), next.Load()
		claimed := now + int64(time.Second)
		if now < prev || !next.CompareAndSwap(prev, claimed) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many login attempts; try again in a second", http.StatusTooManyRequests)
			return
		}
		gotUser, got := sha256.Sum256([]byte(u)), sha256.Sum256([]byte(p))
		if subtle.ConstantTimeCompare(gotUser[:], wantUser[:])&subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			next.CompareAndSwap(claimed, prev) // a correct login gives the slot back
			h.ServeHTTP(w, r)
			return
		}
		s.Log.Warn("editor login failed", "client", r.RemoteAddr)
		w.Header().Set("WWW-Authenticate", `Basic realm="maintenance-manager", charset="UTF-8"`)
		http.Error(w, "login required (user admin)", http.StatusUnauthorized)
	})
}

// csrf lets GET and HEAD through and checks every other request with Go's
// CrossOriginProtection (Sec-Fetch-Site, then Origin). It also refuses a
// request that carries none of Sec-Fetch-Site, Origin and Referer, which
// CrossOriginProtection would let through (§10).
func csrf(cop *http.CrossOriginProtection, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			next.ServeHTTP(w, r)
			return
		case http.MethodPost:
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := cop.Check(r); err != nil {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if r.Header.Get("Sec-Fetch-Site") == "" && r.Header.Get("Origin") == "" && r.Header.Get("Referer") == "" {
			http.Error(w, "request refused: no Sec-Fetch-Site, Origin or Referer header", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			http.Error(w, "forms only (application/x-www-form-urlencoded)", http.StatusUnsupportedMediaType)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// zone is the zone pages are shown in: the mm_tz cookie, else UTC.
func zone(r *http.Request) *time.Location {
	if c, err := r.Cookie(tzCookie); err == nil {
		if loc, err := LoadZone(c.Value); err == nil {
			return loc
		}
	}
	return time.UTC
}

// rememberTZ stores the browser's zone (the JS-filled tz field) in a cookie,
// so the next page shows times there. Forms are always decoded in the zone
// they were rendered in (formTz), never in this one.
func rememberTZ(w http.ResponseWriter, r *http.Request) {
	tz := r.PostFormValue("tz")
	if _, err := LoadZone(tz); err != nil {
		return
	}
	if c, err := r.Cookie(tzCookie); err == nil && c.Value == tz {
		return
	}
	// Not Secure: the editor is usually reached over plain HTTP through
	// kubectl port-forward, and the cookie holds only a display time zone.
	http.SetCookie(w, &http.Cookie{Name: tzCookie, Value: tz, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 365 * 24 * 3600}) //nolint:gosec // G124: see above
}

// setTZ is the "show times in my zone" button.
func (s *Server) setTZ(w http.ResponseWriter, r *http.Request) {
	rememberTZ(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// view returns the controller's picture, memoised for 2 s so a burst of
// page loads costs one round of API reads.
func (s *Server) view(ctx context.Context) (controller.View, error) {
	if s.View == nil {
		return controller.View{}, errors.New("no controller view")
	}
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	if time.Since(s.viewAt) < viewMemo {
		return s.viewVal, s.viewErr
	}
	// Not the first caller's context: a browser navigating away must not
	// hand everyone else "context canceled" for the next 2 s.
	vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), viewTimeout)
	defer cancel()
	s.viewVal, s.viewErr = s.View(vctx)
	s.viewAt = time.Now()
	return s.viewVal, s.viewErr
}

// page renders a template inside the layout.
func (s *Server) page(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	var b strings.Builder
	script := template.JS(tzScript) //nolint:gosec // G203: a constant, allowed by its CSP hash
	if err := pages.ExecuteTemplate(&b, name, map[string]any{"D": data, "Zone": zone(r).String(), "Script": script, "Path": r.URL.Path}); err != nil {
		s.Log.Error("editor template", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(b.String()))
}

// fail shows a plain error page.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.page(w, r, status, "error.html", msg)
}

// audit writes the one log line per write (§10): object, changed fields,
// client. Behind a port-forward the client is always 127.0.0.1.
func (s *Server) audit(r *http.Request, verb, kind, name string, before, after any) {
	s.Log.Info("editor write", "verb", verb, "kind", kind, "name", name, "fields", changedFields(before, after), "client", r.RemoteAddr)
}

// changedFields lists the JSON paths that differ between two specs.
func changedFields(before, after any) []string {
	a, b := flatten(before), flatten(after)
	var out []string
	for k, v := range b {
		if old, ok := a[k]; !ok || !reflect.DeepEqual(old, v) {
			out = append(out, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	if len(out) > 50 {
		out = append(out[:50], "…")
	}
	return out
}

func flatten(v any) map[string]any {
	out := map[string]any{}
	if v == nil || (reflect.ValueOf(v).Kind() == reflect.Pointer && reflect.ValueOf(v).IsNil()) {
		return out
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return out
	}
	var m any
	if json.Unmarshal(raw, &m) != nil {
		return out
	}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				walk(join(prefix, k), c)
			}
		case []any:
			for i, c := range t {
				walk(join(prefix, itoa(i)), c)
			}
		default:
			out[prefix] = t
		}
	}
	walk("", m)
	return out
}

func join(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

// redirect answers a successful POST with 303 to target, always one of our
// own paths ("/", or "/<kind>/" plus an escaped object name).
func redirect(w http.ResponseWriter, r *http.Request, target string) {
	rememberTZ(w, r)
	http.Redirect(w, r, target, http.StatusSeeOther) //nolint:gosec // G710: target is built from our own paths, see above
}

func pathEscape(s string) string { return url.PathEscape(s) }
