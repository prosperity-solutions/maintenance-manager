package editor

import (
	"net/http"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/controller"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

type dashRoute struct {
	Namespace, Name string
	Armed           string // "yes" (one of our policies targets it), "pending" (the leader has yet to write it) or "no"
	Extension       bool   // armed through our EnvoyExtensionPolicy, not our SecurityPolicy
	Accepted        string // "yes", "no" or "" (not armed / unknown)
	AcceptedWhy     string
	Conflicts       []string
	Via             string // why it is (to be) armed through the EnvoyExtensionPolicy
}

type dashPhase struct {
	Name, Action, Starts string
	Active               bool
}

type dashWindow struct {
	Name, State, RV string
	Enabled         bool
	Active          string // the active phase's name
	Next            int    // phase index for "Start next phase now", or -1
	NextName        string
	EnableStarts    string // what Enable makes active at once, e.g. "down (block)"
	Ends            string
	Phases          []dashPhase
	Routes          []dashRoute
	Problems        []snapshot.Problem
}

type dashboard struct {
	Windows []dashWindow
	ViewErr string
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	var windows mmv1.MaintenanceWindowList
	var services mmv1.MaintenanceServiceList
	var routes gwapiv1.HTTPRouteList
	// Windows uncached: their resourceVersion goes into the buttons, and the
	// page right after "Disable now" must show the window disabled.
	if err := s.API.List(r.Context(), &windows, client.InNamespace(s.Namespace)); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "listing windows failed: "+err.Error())
		return
	}
	if err := s.Client.List(r.Context(), &services, client.InNamespace(s.Namespace)); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "listing services failed: "+err.Error())
		return
	}
	if err := s.Client.List(r.Context(), &routes); err != nil {
		s.fail(w, r, http.StatusInternalServerError, "listing routes failed: "+err.Error())
		return
	}
	infos := make([]snapshot.RouteInfo, 0, len(routes.Items))
	for _, rt := range routes.Items {
		infos = append(infos, snapshot.RouteInfo{Key: snapshot.RouteKey{Namespace: rt.Namespace, Name: rt.Name}, Labels: rt.Labels})
	}
	selectors := snapshot.Selectors(services.Items)

	d := dashboard{}
	view, verr := s.view(r.Context())
	if verr != nil {
		d.ViewErr = verr.Error()
	}
	desired := map[snapshot.RouteKey]bool{}
	for _, m := range []map[string][]string{view.Desired.Routes, view.Desired.ExtRoutes} {
		for ns, names := range m {
			for _, n := range names {
				desired[snapshot.RouteKey{Namespace: ns, Name: n}] = true
			}
		}
	}

	now, loc := s.now(), zone(r)
	slices.SortFunc(windows.Items, func(a, b mmv1.MaintenanceWindow) int { return strings.Compare(a.Name, b.Name) })
	for i := range windows.Items {
		mw := &windows.Items[i]
		dw := dashWindow{Name: mw.Name, RV: mw.ResourceVersion, Enabled: mw.Spec.Enabled,
			State: WindowState(&mw.Spec, now, s.Lead), Next: NextPhase(&mw.Spec, now), Problems: snapshot.ValidateWindow(&mw.Spec)}
		if mw.Spec.EndsAt != nil {
			dw.Ends = FormatDisplay(mw.Spec.EndsAt.Time, loc)
		}
		if !mw.Spec.Enabled && dw.State != StateInvalid {
			on := mw.Spec.DeepCopy()
			on.Enabled = true
			if a := snapshot.ActivePhase(on, now); a >= 0 {
				dw.EnableStarts = on.Phases[a].Name + " (" + string(on.Phases[a].Action) + ")"
			}
		}
		active := snapshot.ActivePhase(&mw.Spec, now)
		if dw.State == StateInvalid {
			active, dw.Next = -1, -1 // never armed, so no phase is active
		}
		for j, ph := range mw.Spec.Phases {
			dp := dashPhase{Name: ph.Name, Action: string(ph.Action), Active: j == active}
			switch {
			case ph.StartsAt != nil:
				dp.Starts = FormatDisplay(ph.StartsAt.Time, loc)
			case j == 0:
				dp.Starts = "when enabled"
			default:
				dp.Starts = "pending"
			}
			dw.Phases = append(dw.Phases, dp)
		}
		if active >= 0 {
			dw.Active = mw.Spec.Phases[active].Name
		}
		if dw.Next >= 0 {
			dw.NextName = mw.Spec.Phases[dw.Next].Name
		}
		if dw.State == StateArmed || dw.State == StateActive {
			for _, rt := range snapshot.WindowRoutes(&mw.Spec, selectors, infos) {
				dr := dashRoute{Namespace: rt.Key.Namespace, Name: rt.Key.Name, Armed: "no", Conflicts: view.Conflicts[rt.Key], Via: view.Via[rt.Key]}
				kind := view.Targets[rt.Key.Namespace][rt.Key.Name]
				switch {
				case verr != nil:
					dr.Armed = "unknown"
				case kind != "":
					dr.Armed = "yes"
					dr.Extension = kind == controller.ExtensionPolicyGVK.Kind
				case desired[rt.Key]:
					dr.Armed = "pending"
				}
				if acc, ok := view.Accepted[controller.PolicyRef{Namespace: rt.Key.Namespace, Kind: kind}]; ok && dr.Armed == "yes" {
					dr.Accepted, dr.AcceptedWhy = "no", acc.Reason
					if acc.OK {
						dr.Accepted, dr.AcceptedWhy = "yes", ""
					}
				}
				dw.Routes = append(dw.Routes, dr)
			}
			slices.SortFunc(dw.Routes, func(a, b dashRoute) int {
				return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
			})
		}
		d.Windows = append(d.Windows, dw)
	}
	s.page(w, r, http.StatusOK, "dashboard.html", d)
}
