package editor

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/render"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// previewInput is a plausible place for a previewed page: a window that
// warns, shows the interstitial, then blocks, with the previewed phase
// running now and ending in an hour.
func previewInput(now time.Time, interstitial bool) render.Input {
	at := now.Truncate(time.Minute)
	h := time.Hour
	warn := render.Phase{Window: "preview", Name: "warn", Action: "banner", Number: 1}
	hello := render.Phase{Window: "preview", Name: "hello", Action: "interstitial", Number: 2}
	down := render.Phase{Window: "preview", Name: "down", Action: "block", Number: 3}
	in := render.Input{Interstitial: interstitial, Until: at.Add(h)}
	if interstitial {
		warn.Start, warn.End, hello.Start, hello.End, down.Start, down.End = at.Add(-h), at, at, at.Add(h), at.Add(h), at.Add(3*h)
		in.Context = render.Context{
			Window:   render.Window{Name: "preview", Start: warn.Start, End: down.End, Phases: 3},
			Previous: warn, Phase: hello, Next: down,
			Downtime: render.Span{Start: down.Start, End: down.End}, HasDowntime: true,
			ContinueAfter: mmv1.DefaultAutoContinueSeconds,
		}
		return in
	}
	warn.Start, warn.End, hello.Start, hello.End, down.Start, down.End = at.Add(-2*h), at.Add(-h), at.Add(-h), at, at, at.Add(h)
	in.Context = render.Context{
		Window:   render.Window{Name: "preview", Start: warn.Start, End: down.End, Phases: 3},
		Previous: hello, Phase: down,
		Downtime: render.Span{Start: down.Start, End: down.End}, HasDowntime: true,
	}
	return in
}

// previewCSP isolates a preview from the editor: the page's HTML is
// arbitrary, so it runs in a sandbox (an opaque origin, no scripts, no forms).
const previewCSP = "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:; frame-ancestors 'none'"

type localeRow struct {
	Tag, Title, Message, Continue string
	ContinueDefault               string // the built-in text an empty field gives
	Blank                         bool   // added empty by the form: hidden behind "Add" when scripts run
}

type pageForm struct {
	Name, RV       string
	New            bool
	FallbackLocale string
	HTML           string
	Rows           []localeRow
	Tags           []string // saved locales, for the preview links
	BuiltIn        string   // the built-in page's template, to copy from
	Errors         map[string][]string
	All            []snapshot.Problem
	Notice         string
}

func (s *Server) listPages(w http.ResponseWriter, r *http.Request) {
	var list mmv1.MaintenancePageList
	if err := s.Client.List(r.Context(), &list, client.InNamespace(s.Namespace)); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "listing pages failed: "+err.Error())
		return
	}
	slices.SortFunc(list.Items, func(a, b mmv1.MaintenancePage) int { return strings.Compare(a.Name, b.Name) })
	s.page(w, r, http.StatusOK, "pages.html", list.Items)
}

func (s *Server) newPage(w http.ResponseWriter, r *http.Request) {
	spec := &mmv1.MaintenancePageSpec{FallbackLocale: "en", Locales: map[string]mmv1.PageText{"en": {}}}
	s.page(w, r, http.StatusOK, "page.html", pageFormOf("", "", true, spec, nil, ""))
}

func (s *Server) editPage(w http.ResponseWriter, r *http.Request) {
	var obj mmv1.MaintenancePage
	if err := s.API.Get(r.Context(), client.ObjectKey{Namespace: s.Namespace, Name: r.PathValue("name")}, &obj); err != nil {
		s.getFailed(w, r, err)
		return
	}
	s.page(w, r, http.StatusOK, "page.html", pageFormOf(obj.Name, obj.ResourceVersion, false, &obj.Spec, nil, ""))
}

func (s *Server) createPage(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	spec, ps := DecodePage(r.PostForm)
	ps = append(ps, nameProblems(name)...)
	if len(ps) == 0 {
		ps = PageProblems(&spec)
	}
	if len(ps) > 0 {
		s.page(w, r, http.StatusUnprocessableEntity, "page.html", pageFormOf(name, "", true, &spec, ps, ""))
		return
	}
	obj := &mmv1.MaintenancePage{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: name}, Spec: spec}
	if err := s.Client.Create(r.Context(), obj); err != nil {
		s.page(w, r, http.StatusUnprocessableEntity, "page.html", pageFormOf(name, "", true, &spec, nil, writeError(err)))
		return
	}
	s.audit(r, "create", "MaintenancePage", name, nil, spec)
	redirect(w, r, "/pages/"+pathEscape(name)+"?saved=1")
}

func (s *Server) updatePage(w http.ResponseWriter, r *http.Request) {
	var obj mmv1.MaintenancePage
	if err := s.API.Get(r.Context(), client.ObjectKey{Namespace: s.Namespace, Name: r.PathValue("name")}, &obj); err != nil {
		s.getFailed(w, r, err)
		return
	}
	rv := r.PostFormValue("resourceVersion")
	if rv == "" {
		s.fail(w, r, http.StatusBadRequest, "the form carries no resourceVersion; reload the page and try again")
		return
	}
	spec, ps := DecodePage(r.PostForm)
	if len(ps) == 0 {
		ps = PageProblems(&spec)
	}
	if len(ps) > 0 {
		s.page(w, r, http.StatusUnprocessableEntity, "page.html", pageFormOf(obj.Name, rv, false, &spec, ps, ""))
		return
	}
	before := obj.Spec.DeepCopy()
	obj.ResourceVersion = rv
	obj.Spec = spec
	if err := s.Client.Update(r.Context(), &obj); err != nil {
		s.page(w, r, http.StatusConflict, "page.html", pageFormOf(obj.Name, rv, false, &spec, nil, writeError(err)))
		return
	}
	s.audit(r, "update", "MaintenancePage", obj.Name, before, spec)
	redirect(w, r, "/pages/"+pathEscape(obj.Name)+"?saved=1")
}

func (s *Server) deletePage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	users, err := s.windowsUsing(r, func(spec *mmv1.MaintenanceWindowSpec) bool {
		for _, ph := range spec.Phases {
			for _, rt := range ph.Routings {
				if rt.Page == name {
					return true
				}
			}
		}
		return false
	})
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "checking windows failed: "+err.Error())
		return
	}
	if len(users) > 0 {
		s.fail(w, r, http.StatusConflict, "page "+name+" is still used by the windows "+strings.Join(users, ", ")+"; remove it there first")
		return
	}
	s.deleteObject(w, r, &mmv1.MaintenancePage{}, "MaintenancePage", name, "/pages")
}

// previewSaved renders a saved page with the real renderer.
func (s *Server) previewSaved(w http.ResponseWriter, r *http.Request) {
	var obj mmv1.MaintenancePage
	if err := s.API.Get(r.Context(), client.ObjectKey{Namespace: s.Namespace, Name: r.PathValue("name")}, &obj); err != nil {
		s.getFailed(w, r, err)
		return
	}
	s.preview(w, r, &obj.Spec, r.URL.Query().Get("locale"), r.URL.Query().Get("action"))
}

// previewForm renders the unsaved form, so an edit can be checked first.
func (s *Server) previewForm(w http.ResponseWriter, r *http.Request) {
	spec, ps := DecodePage(r.PostForm)
	if len(ps) == 0 {
		ps = PageProblems(&spec)
	}
	if len(ps) > 0 {
		w.Header().Set("Content-Security-Policy", previewCSP)
		http.Error(w, "the page is not valid yet: "+ps[0].Error(), http.StatusUnprocessableEntity)
		return
	}
	s.preview(w, r, &spec, r.PostFormValue("previewLocale"), r.PostFormValue("previewAction"))
}

// preview answers with the rendered page in a sandbox. It never sends the
// interstitial's Refresh or cookie: only the body is previewed.
// Previews run no scripts, so the time shows in the editor's zone (zone(r)),
// as a visitor's browser would show it in theirs.
func (s *Server) preview(w http.ResponseWriter, r *http.Request, spec *mmv1.MaintenancePageSpec, loc, action string) {
	w.Header().Set("Content-Security-Policy", previewCSP)
	p, err := render.Parse(spec.HTML)
	if err != nil {
		http.Error(w, "the page template is not valid: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if _, ok := spec.Locales[loc]; !ok {
		loc = spec.FallbackLocale
	}
	txt := spec.Locales[loc]
	in := previewInput(s.now(), action == string(mmv1.ActionInterstitial))
	in.Locale, in.Title, in.Message, in.Continue, in.Zone = loc, txt.Title, txt.Message, txt.ContinueLabel, zone(r)
	body, err := p.Render(in)
	if err != nil {
		http.Error(w, "rendering failed: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(body)) //nolint:gosec // G705: a preview serves the page's own HTML on purpose, inside the sandbox CSP set above
}

func pageFormOf(name, rv string, isNew bool, spec *mmv1.MaintenancePageSpec, ps []snapshot.Problem, notice string) pageForm {
	f := pageForm{Name: name, RV: rv, New: isNew, FallbackLocale: spec.FallbackLocale, HTML: spec.HTML,
		Errors: errorsByField(ps), All: ps, Notice: notice}
	v := EncodePage(spec)
	for k := 0; v.Has("l" + itoa(k) + ".tag"); k++ {
		pre := "l" + itoa(k) + "."
		f.Rows = append(f.Rows, localeRow{Tag: v.Get(pre + "tag"), Title: v.Get(pre + "title"), Message: v.Get(pre + "message"),
			Continue: v.Get(pre + "continue"), ContinueDefault: render.ContinueFor(v.Get(pre + "tag"))})
	}
	f.Rows = append(f.Rows, localeRow{Blank: true, ContinueDefault: "built-in for the language"}, localeRow{Blank: true, ContinueDefault: "built-in for the language"})
	f.BuiltIn = render.DefaultSource()
	if !isNew {
		f.Tags = slices.Sorted(maps.Keys(spec.Locales))
	}
	return f
}
