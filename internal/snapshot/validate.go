package snapshot

import (
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
)

// Problem is one validation failure, addressed by the editor's form field
// names (§10) so the editor can show it inline.
type Problem struct {
	Field   string // e.g. "p1.startsAt", "p0.r1.services", "endsAt"
	Message string
}

func (p Problem) Error() string { return p.Field + ": " + p.Message }

// ValidateWindow applies the phase rules of §4.4. A window with problems is
// invalid and never armed. Missing pages and services are warnings, not
// problems (see Warnings).
func ValidateWindow(spec *mmv1.MaintenanceWindowSpec) []Problem {
	var ps []Problem
	add := func(field, format string, a ...any) {
		ps = append(ps, Problem{Field: field, Message: fmt.Sprintf(format, a...)})
	}
	if len(spec.Phases) == 0 {
		add("phases", "a window needs at least one phase")
		return ps
	}
	names := map[string]int{}
	var lastDated *metav1.Time
	pending := false
	for i, ph := range spec.Phases {
		f := fmt.Sprintf("p%d", i)
		if ph.Name == "" {
			add(f+".name", "every phase needs a name")
		} else {
			if j, dup := names[ph.Name]; dup {
				add(f+".name", "phase name %q is already used by phase %d", ph.Name, j+1)
			}
			names[ph.Name] = i
		}
		switch ph.Action {
		case mmv1.ActionBanner, mmv1.ActionInterstitial, mmv1.ActionBlock:
		default:
			add(f+".action", "action must be banner, interstitial or block, not %q", ph.Action)
		}

		switch {
		case ph.StartsAt == nil && i > 0:
			pending = true
		case ph.StartsAt != nil && pending:
			add(f+".startsAt", "a phase after a pending one must be pending too; clear its start time or date the phase before it")
		case ph.StartsAt != nil:
			if lastDated != nil && !ph.StartsAt.After(lastDated.Time) {
				add(f+".startsAt", "must be after the previous phase's start (%s)", lastDated.UTC().Format("2006-01-02T15:04:05Z"))
			}
			lastDated = ph.StartsAt
		}

		if len(ph.Routings) == 0 {
			add(f+".r0.services", "a phase needs at least one routing")
		}
		seen := map[string]int{}
		for j, r := range ph.Routings {
			rf := fmt.Sprintf("%s.r%d.services", f, j)
			if len(r.Services) == 0 {
				add(rf, "a routing needs at least one service")
			}
			for _, s := range r.Services {
				if k, dup := seen[s]; dup {
					add(rf, "service %q appears twice in this phase (also in routing %d)", s, k+1)
					continue
				}
				seen[s] = j
			}
		}
	}
	if spec.EndsAt != nil && lastDated != nil && !spec.EndsAt.After(lastDated.Time) {
		add("endsAt", "must be after the last dated phase start (%s)", lastDated.UTC().Format("2006-01-02T15:04:05Z"))
	}
	return ps
}

// Warnings lists references that fall back instead of failing: a routing
// whose page does not exist uses the default page, and a missing service is
// skipped (§4.4 rule 9).
func Warnings(spec *mmv1.MaintenanceWindowSpec, services, pages map[string]bool) []Problem {
	var ws []Problem
	for i, ph := range spec.Phases {
		for j, r := range ph.Routings {
			if r.Page != "" && !pages[r.Page] {
				ws = append(ws, Problem{Field: fmt.Sprintf("p%d.r%d.page", i, j), Message: fmt.Sprintf("page %q does not exist; the default page is used", r.Page)})
			}
			for _, s := range r.Services {
				if s != "" && !services[s] {
					ws = append(ws, Problem{Field: fmt.Sprintf("p%d.r%d.services", i, j), Message: fmt.Sprintf("service %q does not exist; it is skipped", s)})
				}
			}
		}
	}
	return ws
}

// ValidateService checks the route selector: non-empty and well-formed.
func ValidateService(spec *mmv1.MaintenanceServiceSpec) []Problem {
	sel := spec.RouteSelector
	if len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0 {
		return []Problem{{Field: "sel0.key", Message: "a service needs at least one label; an empty selector would match every route"}}
	}
	if _, err := metav1.LabelSelectorAsSelector(&sel); err != nil {
		return []Problem{{Field: "sel0.key", Message: err.Error()}}
	}
	return nil
}

// ServicesOf lists every service a window names, in any phase, sorted.
func ServicesOf(spec *mmv1.MaintenanceWindowSpec) []string {
	set := map[string]bool{}
	for _, ph := range spec.Phases {
		for _, r := range ph.Routings {
			for _, s := range r.Services {
				set[s] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}
