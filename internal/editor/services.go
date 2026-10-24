package editor

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

type kv struct{ Key, Value string }

type routeRow struct {
	Namespace, Name string
	Hostnames       []string
	Conflicts       []string
}

type serviceForm struct {
	Name, RV    string
	New         bool
	Rows        []kv
	Expressions string // matchExpressions, shown read-only and kept on save
	Errors      map[string][]string
	All         []snapshot.Problem // every problem, listed above the form
	Notice      string
	Routes      []routeRow
	RoutesErr   string
}

func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
	var list mmv1.MaintenanceServiceList
	if err := s.Client.List(r.Context(), &list, client.InNamespace(s.Namespace)); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "listing services failed: "+err.Error())
		return
	}
	slices.SortFunc(list.Items, func(a, b mmv1.MaintenanceService) int { return strings.Compare(a.Name, b.Name) })
	s.page(w, r, http.StatusOK, "services.html", list.Items)
}

func (s *Server) newService(w http.ResponseWriter, r *http.Request) {
	s.page(w, r, http.StatusOK, "service.html", s.serviceForm(r, "", "", true, &mmv1.MaintenanceServiceSpec{}, nil, ""))
}

func (s *Server) editService(w http.ResponseWriter, r *http.Request) {
	var obj mmv1.MaintenanceService
	if err := s.API.Get(r.Context(), client.ObjectKey{Namespace: s.Namespace, Name: r.PathValue("name")}, &obj); err != nil {
		s.getFailed(w, r, err)
		return
	}
	f := s.serviceForm(r, obj.Name, obj.ResourceVersion, false, &obj.Spec, nil, "")
	s.page(w, r, http.StatusOK, "service.html", f)
}

func (s *Server) createService(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	spec, ps := DecodeService(r.PostForm, nil)
	ps = append(ps, nameProblems(name)...)
	if len(ps) == 0 {
		ps = ValidateServiceForm(&spec)
	}
	if len(ps) > 0 {
		s.page(w, r, http.StatusUnprocessableEntity, "service.html", s.serviceForm(r, name, "", true, &spec, ps, ""))
		return
	}
	obj := &mmv1.MaintenanceService{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: name}, Spec: spec}
	if err := s.Client.Create(r.Context(), obj); err != nil {
		s.page(w, r, http.StatusUnprocessableEntity, "service.html", s.serviceForm(r, name, "", true, &spec, nil, writeError(err)))
		return
	}
	s.audit(r, "create", "MaintenanceService", name, nil, spec)
	redirect(w, r, "/services/"+pathEscape(name))
}

func (s *Server) updateService(w http.ResponseWriter, r *http.Request) {
	var obj mmv1.MaintenanceService
	if err := s.API.Get(r.Context(), client.ObjectKey{Namespace: s.Namespace, Name: r.PathValue("name")}, &obj); err != nil {
		s.getFailed(w, r, err)
		return
	}
	rv := r.PostFormValue("resourceVersion")
	if rv == "" {
		s.fail(w, r, http.StatusBadRequest, "the form carries no resourceVersion; reload the page and try again")
		return
	}
	spec, ps := DecodeService(r.PostForm, &obj.Spec)
	if len(ps) == 0 {
		ps = ValidateServiceForm(&spec)
	}
	if len(ps) > 0 {
		s.page(w, r, http.StatusUnprocessableEntity, "service.html", s.serviceForm(r, obj.Name, rv, false, &spec, ps, ""))
		return
	}
	before := obj.Spec.DeepCopy()
	obj.ResourceVersion = rv
	obj.Spec = spec
	if err := s.Client.Update(r.Context(), &obj); err != nil {
		s.page(w, r, http.StatusConflict, "service.html", s.serviceForm(r, obj.Name, rv, false, &spec, nil, writeError(err)))
		return
	}
	s.audit(r, "update", "MaintenanceService", obj.Name, before, spec)
	redirect(w, r, "/services/"+pathEscape(obj.Name))
}

func (s *Server) deleteService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	users, err := s.windowsUsing(r, func(spec *mmv1.MaintenanceWindowSpec) bool { return slices.Contains(snapshot.ServicesOf(spec), name) })
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "checking windows failed: "+err.Error())
		return
	}
	if len(users) > 0 {
		s.fail(w, r, http.StatusConflict, "service "+name+" is still used by the windows "+strings.Join(users, ", ")+"; remove it there first")
		return
	}
	s.deleteObject(w, r, &mmv1.MaintenanceService{}, "MaintenanceService", name, "/services")
}

// ValidateServiceForm is snapshot.ValidateService (which the snapshot and
// the controller apply too).
func ValidateServiceForm(spec *mmv1.MaintenanceServiceSpec) []snapshot.Problem {
	return snapshot.ValidateService(spec)
}

func (s *Server) serviceForm(r *http.Request, name, rv string, isNew bool, spec *mmv1.MaintenanceServiceSpec, ps []snapshot.Problem, notice string) serviceForm {
	f := serviceForm{Name: name, RV: rv, New: isNew, Errors: errorsByField(ps), All: ps, Notice: notice}
	v := EncodeService(spec)
	for k := 0; v.Has("sel" + itoa(k) + ".key"); k++ {
		f.Rows = append(f.Rows, kv{Key: v.Get("sel" + itoa(k) + ".key"), Value: v.Get("sel" + itoa(k) + ".value")})
	}
	f.Rows = append(f.Rows, kv{}, kv{})
	if len(spec.RouteSelector.MatchExpressions) > 0 {
		raw, _ := json.Marshal(spec.RouteSelector.MatchExpressions)
		f.Expressions = string(raw)
	}
	if !isNew && len(snapshot.ValidateService(spec)) == 0 {
		f.Routes, f.RoutesErr = s.matchedRoutes(r, spec)
	}
	return f
}

// matchedRoutes lists the HTTPRoutes a selector matches, cluster-wide, with
// hostnames (read uncached: the shared cache keeps labels and parentRefs
// only) and conflicts.
func (s *Server) matchedRoutes(r *http.Request, spec *mmv1.MaintenanceServiceSpec) ([]routeRow, string) {
	sel, err := metav1.LabelSelectorAsSelector(&spec.RouteSelector)
	if err != nil {
		return nil, err.Error()
	}
	var routes gwapiv1.HTTPRouteList
	if err := s.API.List(r.Context(), &routes, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, "listing routes failed: " + err.Error()
	}
	view, verr := s.view(r.Context())
	var out []routeRow
	for _, rt := range routes.Items {
		row := routeRow{Namespace: rt.Namespace, Name: rt.Name}
		for _, h := range rt.Spec.Hostnames {
			row.Hostnames = append(row.Hostnames, string(h))
		}
		if verr == nil {
			row.Conflicts = view.Conflicts[snapshot.RouteKey{Namespace: rt.Namespace, Name: rt.Name}]
		}
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b routeRow) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	if verr != nil {
		return out, "conflicts unknown: " + verr.Error()
	}
	return out, ""
}

// nameProblems checks metadata.name on create (it cannot change later).
func nameProblems(name string) []snapshot.Problem {
	if name == "" {
		return []snapshot.Problem{{Field: "name", Message: "a name is required"}}
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return []snapshot.Problem{{Field: "name", Message: strings.Join(errs, "; ")}}
	}
	return nil
}

// writeError turns an API write error into a message for the form.
func writeError(err error) string {
	switch {
	case apierrors.IsConflict(err):
		return "changed by someone else since you loaded this page: reload it, then redo your change"
	case apierrors.IsAlreadyExists(err):
		return "that name is taken: pick another one"
	case apierrors.IsNotFound(err):
		return "it was deleted by someone else"
	case apierrors.IsInvalid(err):
		return "the API server refused it: " + err.Error()
	}
	return "saving failed: " + err.Error()
}

func (s *Server) getFailed(w http.ResponseWriter, r *http.Request, err error) {
	if apierrors.IsNotFound(err) {
		s.fail(w, r, http.StatusNotFound, "not found: "+r.PathValue("name"))
		return
	}
	s.fail(w, r, http.StatusInternalServerError, "reading failed: "+err.Error())
}

// windowsUsing lists the windows (read uncached) for which used is true.
// Best effort: a window saved between this check and the delete can still
// name the object; it then gets the default page, or skips the service
// (§4.4 rule 9).
func (s *Server) windowsUsing(r *http.Request, used func(*mmv1.MaintenanceWindowSpec) bool) ([]string, error) {
	var list mmv1.MaintenanceWindowList
	if err := s.API.List(r.Context(), &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	var out []string
	for i := range list.Items {
		if used(&list.Items[i].Spec) {
			out = append(out, list.Items[i].Name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// deleteObject deletes name with the form's resourceVersion as precondition.
func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, obj client.Object, kind, name, back string) {
	obj.SetNamespace(s.Namespace)
	obj.SetName(name)
	rv := r.PostFormValue("resourceVersion")
	if rv == "" {
		s.fail(w, r, http.StatusBadRequest, "the form carries no resourceVersion; reload the page and try again")
		return
	}
	if err := s.Client.Delete(r.Context(), obj, client.Preconditions{ResourceVersion: &rv}); err != nil {
		s.fail(w, r, http.StatusConflict, writeError(err))
		return
	}
	s.audit(r, "delete", kind, name, nil, nil)
	redirect(w, r, back)
}
