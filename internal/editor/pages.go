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

// previewCSP isolates a preview from the editor: the page's HTML is
// arbitrary, so it runs in a sandbox (an opaque origin, no scripts, no forms).
const previewCSP = "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:; frame-ancestors 'none'"

type localeRow struct {
	Tag, Title, Message string
	Blank               bool // added empty by the form: hidden behind "Add" when scripts run
}

type pageForm struct {
	Name, RV       string
	New            bool
	FallbackLocale string
	HTML           string
	Rows           []localeRow
	Tags           []string // saved locales, for the preview links
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
	s.preview(w, &obj.Spec, r.URL.Query().Get("locale"), r.URL.Query().Get("action"))
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
	s.preview(w, &spec, r.PostFormValue("previewLocale"), r.PostFormValue("previewAction"))
}

// preview answers with the rendered page in a sandbox. It never sends the
// interstitial's Refresh or cookie: only the body is previewed.
func (s *Server) preview(w http.ResponseWriter, spec *mmv1.MaintenancePageSpec, loc, action string) {
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
	body, err := p.Render(render.Input{
		Locale: loc, Title: txt.Title, Message: txt.Message,
		Until:        s.now().Add(time.Hour).Truncate(time.Minute),
		Interstitial: action == string(mmv1.ActionInterstitial),
	})
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
		f.Rows = append(f.Rows, localeRow{Tag: v.Get(pre + "tag"), Title: v.Get(pre + "title"), Message: v.Get(pre + "message")})
	}
	f.Rows = append(f.Rows, localeRow{Blank: true}, localeRow{Blank: true})
	if !isNew {
		f.Tags = slices.Sorted(maps.Keys(spec.Locales))
	}
	return f
}
