package editor

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

type option struct {
	Value, Label      string
	Selected, Missing bool
}

type routingForm struct {
	J        int
	Services []option
	Pages    []option
}

type phaseForm struct {
	I                                          int
	Order, Name, StartsAt, StartsAtWas, Action string
	Display                                    string // startsAt shown with both zones
	Routings                                   []routingForm
}

type windowForm struct {
	Name, RV, FormTz  string
	New, Enabled      bool
	EndsAt, EndsAtWas string
	EndsAtDisplay     string
	Phases            []phaseForm
	Actions           []string
	Errors            map[string][]string
	All, Warnings     []snapshot.Problem
	Notice            string
	Reopen            bool // show the Reopen tick box
}

func (s *Server) listWindows(w http.ResponseWriter, r *http.Request) {
	var list mmv1.MaintenanceWindowList
	if err := s.Client.List(r.Context(), &list, client.InNamespace(s.Namespace)); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "listing windows failed: "+err.Error())
		return
	}
	slices.SortFunc(list.Items, func(a, b mmv1.MaintenanceWindow) int { return strings.Compare(a.Name, b.Name) })
	type row struct{ Name, State string }
	var rows []row
	for i := range list.Items {
		rows = append(rows, row{Name: list.Items[i].Name, State: WindowState(&list.Items[i].Spec, s.now(), s.Lead)})
	}
	s.page(w, r, http.StatusOK, "windows.html", rows)
}

func (s *Server) newWindow(w http.ResponseWriter, r *http.Request) {
	spec := &mmv1.MaintenanceWindowSpec{Phases: []mmv1.Phase{}}
	f, err := s.windowForm(r, zone(r), "", "", true, spec, nil, "")
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.page(w, r, http.StatusOK, "window.html", f)
}

func (s *Server) editWindow(w http.ResponseWriter, r *http.Request) {
	var obj mmv1.MaintenanceWindow
	if err := s.API.Get(r.Context(), client.ObjectKey{Namespace: s.Namespace, Name: r.PathValue("name")}, &obj); err != nil {
		s.getFailed(w, r, err)
		return
	}
	f, err := s.windowForm(r, zone(r), obj.Name, obj.ResourceVersion, false, &obj.Spec, snapshot.ValidateWindow(&obj.Spec), "")
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.page(w, r, http.StatusOK, "window.html", f)
}

// decodeWindowForm decodes in the zone the form was rendered in (formTz),
// never in the browser's current zone: an untouched time must not move.
func decodeWindowForm(r *http.Request) (submitted, error) {
	loc, err := LoadZone(r.PostFormValue("formTz"))
	if err != nil {
		return submitted{}, errors.New("the form's time zone is missing or unknown; reload the page")
	}
	spec, ps, from := decodeWindow(r.PostForm, loc)
	return submitted{spec: spec, loc: loc, problems: ps, raw: r.PostForm, from: from}, nil
}

// submitted is a decoded window form plus what is needed to show it again.
type submitted struct {
	spec     mmv1.MaintenanceWindowSpec
	loc      *time.Location
	problems []snapshot.Problem
	raw      url.Values // the submission, to put back times that did not decode
	from     []int      // per decoded phase, its submitted row
}

// zoneMismatch: a browser that never told us its zone (no cookie) got the
// form in UTC; if it is elsewhere, what was typed meant local time. Show the
// form again in the browser's zone rather than save times that are off by
// the offset.
func zoneMismatch(r *http.Request, formLoc *time.Location) *time.Location {
	if _, err := r.Cookie(tzCookie); err == nil {
		return nil
	}
	loc, err := LoadZone(r.PostFormValue("tz"))
	if err != nil || loc.String() == formLoc.String() {
		return nil
	}
	return loc
}

func zoneNotice(browser, form *time.Location) string {
	return "Nothing is saved yet. Your browser is in " + browser.String() + ", but this form showed times in " + form.String() +
		". The times are now shown in " + browser.String() + ": check the ones you typed, then save again."
}

// reopenProblem: saving an ended, enabled window so that it no longer ends
// makes its last phase (usually block) active at once. Ask for a tick first.
func reopenProblem(r *http.Request, old, updated *mmv1.MaintenanceWindowSpec, now time.Time) []snapshot.Problem {
	if old == nil || !snapshot.Ended(old, now) || !updated.Enabled || snapshot.Ended(updated, now) || r.PostFormValue("reopen") == "on" {
		return nil
	}
	what := "it runs again from now on"
	if a := snapshot.ActivePhase(updated, now); a >= 0 {
		what = "phase " + updated.Phases[a].Name + " (" + string(updated.Phases[a].Action) + ") becomes active at once"
	}
	return []snapshot.Problem{{Field: "reopen", Message: "this window has ended; with this change " + what + ". Tick Reopen and save again to confirm."}}
}

func (s *Server) createWindow(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	sub, err := decodeWindowForm(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	spec, ps := sub.spec, append(sub.problems, nameProblems(name)...)
	// First, before any time-based check: typed times meant the browser's zone.
	if browser := zoneMismatch(r, sub.loc); browser != nil {
		rememberTZ(w, r)
		s.windowPage(w, r, http.StatusUnprocessableEntity, browser, name, "", true, &spec, ps, zoneNotice(browser, sub.loc), &sub)
		return
	}
	if len(ps) == 0 {
		ps = append(snapshot.ValidateWindow(&spec), EndsAtProblem(nil, &spec, s.now())...)
	}
	if len(ps) > 0 {
		s.windowPage(w, r, http.StatusUnprocessableEntity, sub.loc, name, "", true, &spec, ps, "", &sub)
		return
	}
	obj := &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: name}, Spec: spec}
	if err := s.Client.Create(r.Context(), obj); err != nil {
		s.windowPage(w, r, http.StatusUnprocessableEntity, sub.loc, name, "", true, &spec, nil, writeError(err), nil)
		return
	}
	s.audit(r, "create", "MaintenanceWindow", name, nil, spec)
	redirect(w, r, "/windows/"+pathEscape(name))
}

func (s *Server) updateWindow(w http.ResponseWriter, r *http.Request) {
	var obj mmv1.MaintenanceWindow
	if err := s.API.Get(r.Context(), client.ObjectKey{Namespace: s.Namespace, Name: r.PathValue("name")}, &obj); err != nil {
		s.getFailed(w, r, err)
		return
	}
	rv := r.PostFormValue("resourceVersion")
	sub, err := decodeWindowForm(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	spec, ps := sub.spec, sub.problems
	// First, before any time-based check: typed times meant the browser's zone.
	if browser := zoneMismatch(r, sub.loc); browser != nil {
		rememberTZ(w, r)
		s.windowPage(w, r, http.StatusUnprocessableEntity, browser, obj.Name, rv, false, &spec, ps, zoneNotice(browser, sub.loc), &sub)
		return
	}
	if len(ps) == 0 {
		ps = append(snapshot.ValidateWindow(&spec), EndsAtProblem(&obj.Spec, &spec, s.now())...)
		ps = append(ps, reopenProblem(r, &obj.Spec, &spec, s.now())...)
	}
	if len(ps) > 0 {
		s.windowPage(w, r, http.StatusUnprocessableEntity, sub.loc, obj.Name, rv, false, &spec, ps, "", &sub)
		return
	}
	before := obj.Spec.DeepCopy()
	obj.ResourceVersion = rv
	obj.Spec = spec
	if err := s.Client.Update(r.Context(), &obj); err != nil {
		s.windowPage(w, r, http.StatusConflict, sub.loc, obj.Name, rv, false, &spec, nil, writeError(err), &sub)
		return
	}
	s.audit(r, "update", "MaintenanceWindow", obj.Name, before, spec)
	redirect(w, r, "/windows/"+pathEscape(obj.Name))
}

func (s *Server) deleteWindow(w http.ResponseWriter, r *http.Request) {
	s.deleteObject(w, r, &mmv1.MaintenanceWindow{}, "MaintenanceWindow", r.PathValue("name"), "/windows")
}

// setEnabled is the dashboard's enable/disable. Disable is the emergency
// stop: a merge patch without a resourceVersion, so it works on a window
// someone just changed, an invalid one or an ended one. Enable checks the
// resourceVersion the button was shown with.
func (s *Server) setEnabled(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	obj := &mmv1.MaintenanceWindow{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: name}}
	if r.PostFormValue("enabled") != "on" {
		if err := s.Client.Patch(r.Context(), obj, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"enabled":false}}`))); err != nil {
			s.fail(w, r, http.StatusConflict, writeError(err))
			return
		}
		s.audit(r, "disable", "MaintenanceWindow", name, nil, map[string]any{"enabled": false})
		redirect(w, r, "/")
		return
	}
	if err := s.API.Get(r.Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
		s.getFailed(w, r, err)
		return
	}
	if rv := r.PostFormValue("resourceVersion"); rv != obj.ResourceVersion {
		s.fail(w, r, http.StatusConflict, writeErrorConflict)
		return
	}
	base := obj.DeepCopy()
	obj.Spec.Enabled = true
	if err := s.Client.Patch(r.Context(), obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		s.fail(w, r, http.StatusConflict, writeError(err))
		return
	}
	s.audit(r, "enable", "MaintenanceWindow", name, nil, map[string]any{"enabled": true})
	redirect(w, r, "/")
}

const writeErrorConflict = "changed by someone else since you loaded this page: reload it, then redo your change"

// startNext is "Start next phase now" (§4.4 rule 5).
func (s *Server) startNext(w http.ResponseWriter, r *http.Request) {
	var obj mmv1.MaintenanceWindow
	if err := s.API.Get(r.Context(), client.ObjectKey{Namespace: s.Namespace, Name: r.PathValue("name")}, &obj); err != nil {
		s.getFailed(w, r, err)
		return
	}
	if rv := r.PostFormValue("resourceVersion"); rv != obj.ResourceVersion {
		s.fail(w, r, http.StatusConflict, writeErrorConflict)
		return
	}
	want, err := strconv.Atoi(r.PostFormValue("phase"))
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "which phase? reload the page")
		return
	}
	spec, err := StartNextPhaseNow(&obj.Spec, s.now(), want)
	if err != nil {
		s.fail(w, r, http.StatusConflict, err.Error())
		return
	}
	before := obj.Spec.DeepCopy()
	obj.Spec = *spec
	if err := s.Client.Update(r.Context(), &obj); err != nil {
		s.fail(w, r, http.StatusConflict, writeError(err))
		return
	}
	s.audit(r, "next", "MaintenanceWindow", obj.Name, before, obj.Spec)
	redirect(w, r, "/")
}

func (s *Server) windowPage(w http.ResponseWriter, r *http.Request, status int, loc *time.Location, name, rv string, isNew bool, spec *mmv1.MaintenanceWindowSpec, ps []snapshot.Problem, notice string, sub *submitted) {
	f, err := s.windowForm(r, loc, name, rv, isNew, spec, ps, notice)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if sub != nil {
		// A time that did not decode is nil in spec: put back what was typed,
		// so fixing another error and saving cannot silently clear it.
		if len(f.Errors["endsAt"]) > 0 {
			f.EndsAt, f.EndsAtWas = sub.raw.Get("endsAt"), sub.raw.Get("endsAtWas")
		}
		for i := range f.Phases {
			if i < len(sub.from) && len(f.Errors["p"+itoa(i)+".startsAt"]) > 0 {
				row := "p" + itoa(sub.from[i]) + "."
				f.Phases[i].StartsAt, f.Phases[i].StartsAtWas = sub.raw.Get(row+"startsAt"), sub.raw.Get(row+"startsAtWas")
			}
		}
	}
	f.Reopen = len(f.Errors["reopen"]) > 0
	s.page(w, r, status, "window.html", f)
}

// windowForm builds the form from EncodeWindow, so the field values come from
// the same code that decodes them. loc is the zone the inputs are shown in;
// it travels in formTz and decodes the submission.
func (s *Server) windowForm(r *http.Request, loc *time.Location, name, rv string, isNew bool, spec *mmv1.MaintenanceWindowSpec, ps []snapshot.Problem, notice string) (windowForm, error) {
	var services mmv1.MaintenanceServiceList
	var pagesList mmv1.MaintenancePageList
	if err := s.Client.List(r.Context(), &services, client.InNamespace(s.Namespace)); err != nil {
		return windowForm{}, err
	}
	if err := s.Client.List(r.Context(), &pagesList, client.InNamespace(s.Namespace)); err != nil {
		return windowForm{}, err
	}
	svcNames, pageNames := map[string]bool{}, map[string]bool{}
	for _, x := range services.Items {
		svcNames[x.Name] = true
	}
	for _, x := range pagesList.Items {
		pageNames[x.Name] = true
	}
	v := EncodeWindow(spec, loc)
	f := windowForm{
		Name: name, RV: rv, FormTz: loc.String(), New: isNew, Enabled: spec.Enabled,
		EndsAt: v.Get("endsAt"), EndsAtWas: v.Get("endsAtWas"),
		Actions: []string{string(mmv1.ActionBanner), string(mmv1.ActionInterstitial), string(mmv1.ActionBlock)},
		Errors:  errorsByField(ps), All: ps, Notice: notice,
		Warnings: snapshot.Warnings(spec, svcNames, pageNames),
	}
	if spec.EndsAt != nil {
		f.EndsAtDisplay = FormatDisplay(spec.EndsAt.Time, loc)
	}
	for i, ph := range spec.Phases {
		pre := "p" + itoa(i) + "."
		pf := phaseForm{I: i, Order: v.Get(pre + "order"), Name: v.Get(pre + "name"), StartsAt: v.Get(pre + "startsAt"),
			StartsAtWas: v.Get(pre + "startsAtWas"), Action: v.Get(pre + "action")}
		if ph.StartsAt != nil {
			pf.Display = FormatDisplay(ph.StartsAt.Time, loc)
		}
		for j, rt := range ph.Routings {
			pf.Routings = append(pf.Routings, routingForm{J: j, Services: serviceOptions(rt.Services, svcNames), Pages: pageOptions(rt.Page, pageNames)})
		}
		pf.Routings = appendBlankRoutings(pf.Routings, svcNames, pageNames)
		f.Phases = append(f.Phases, pf)
	}
	for k := range blankRows {
		i := len(spec.Phases) + k
		pf := phaseForm{I: i, Order: itoa(i + 1), Action: string(mmv1.ActionBlock)}
		pf.Routings = appendBlankRoutings(nil, svcNames, pageNames)
		f.Phases = append(f.Phases, pf)
	}
	return f, nil
}

func appendBlankRoutings(rs []routingForm, svcNames, pageNames map[string]bool) []routingForm {
	for range blankRows {
		rs = append(rs, routingForm{J: len(rs), Services: serviceOptions(nil, svcNames), Pages: pageOptions("", pageNames)})
	}
	return rs
}

// serviceOptions lists the selected services first, in stored order (a
// browser submits a multi-select in option order, so this keeps the order on
// save), including names that do not exist (marked missing, so a save keeps
// them), then the other services by name.
func serviceOptions(selected []string, existing map[string]bool) []option {
	var out []option
	seen := map[string]bool{}
	for _, s := range selected {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, option{Value: s, Label: s, Selected: true, Missing: !existing[s]})
	}
	for _, s := range sortedKeys(existing) {
		if !seen[s] {
			out = append(out, option{Value: s, Label: s})
		}
	}
	return out
}

// pageOptions: the default page, then every page; a missing selected page
// stays selectable (marked missing).
func pageOptions(selected string, existing map[string]bool) []option {
	out := []option{{Value: "", Label: "(built-in default page)", Selected: selected == ""}}
	if selected != "" && !existing[selected] {
		out = append(out, option{Value: selected, Label: selected, Selected: true, Missing: true})
	}
	for _, p := range sortedKeys(existing) {
		out = append(out, option{Value: p, Label: p, Selected: p == selected})
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
