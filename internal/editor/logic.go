package editor

import (
	"errors"
	"fmt"
	"html/template"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/render"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// Window states on the dashboard (§10).
const (
	StateIdle    = "idle"
	StateArmed   = "armed"
	StateActive  = "active"
	StateEnded   = "ended"
	StateInvalid = "invalid"
)

// WindowState is one of idle, armed, active, ended or invalid.
func WindowState(spec *mmv1.MaintenanceWindowSpec, now time.Time, lead time.Duration) string {
	switch {
	case len(snapshot.ValidateWindow(spec)) > 0:
		return StateInvalid
	case snapshot.Ended(spec, now):
		return StateEnded
	case snapshot.ActivePhase(spec, now) >= 0:
		return StateActive
	case snapshot.Armed(spec, now, lead):
		return StateArmed
	}
	return StateIdle
}

// NextPhase is the index of the phase "Start next phase now" applies to:
// the one right after the active phase (§4.4 rule 5), or -1.
func NextPhase(spec *mmv1.MaintenanceWindowSpec, now time.Time) int {
	a := snapshot.ActivePhase(spec, now)
	if a < 0 || a+1 >= len(spec.Phases) {
		return -1
	}
	return a + 1
}

// StartNextPhaseNow applies §4.4 rule 5 to a copy of spec: the phase after
// the active one starts at max(now, previous start + 1 s), truncated to
// seconds. want is the phase index the button was shown for; if the active
// phase moved since, it refuses rather than start another phase.
func StartNextPhaseNow(spec *mmv1.MaintenanceWindowSpec, now time.Time, want int) (*mmv1.MaintenanceWindowSpec, error) {
	i := NextPhase(spec, now)
	if i < 0 {
		return nil, errors.New("no phase is active, or the active phase is the last one")
	}
	if i != want {
		return nil, fmt.Errorf("the window moved on since the page was loaded (next phase is now %d, not %d); reload", i+1, want+1)
	}
	t := now.UTC().Truncate(time.Second)
	if prev := spec.Phases[i-1].StartsAt; prev != nil {
		if min := prev.UTC().Truncate(time.Second).Add(time.Second); t.Before(min) {
			t = min
		}
	}
	out := spec.DeepCopy()
	out.Phases[i].StartsAt = &metav1.Time{Time: t}
	if ps := snapshot.ValidateWindow(out); len(ps) > 0 {
		return nil, fmt.Errorf("starting phase %d now would make the window invalid: %s", i+1, ps[0].Error())
	}
	return out, nil
}

// EndsAtProblem applies the editor-only part of §4.4 rule 3: a changed
// endsAt must not lie in the past. An unchanged one may (the window has
// ended and is being edited or disabled).
func EndsAtProblem(old, updated *mmv1.MaintenanceWindowSpec, now time.Time) []snapshot.Problem {
	if updated.EndsAt == nil {
		return nil
	}
	if old != nil && old.EndsAt != nil && old.EndsAt.Equal(updated.EndsAt) {
		return nil
	}
	if updated.EndsAt.Time.Before(now) {
		return []snapshot.Problem{{Field: "endsAt", Message: "a new end time must not be in the past"}}
	}
	return nil
}

// PageProblems is snapshot.ValidatePage plus a render of every locale in
// both modes (plain and interstitial) against the 64 KiB cap, so a page that
// grows too large only for one locale is caught on save.
func PageProblems(spec *mmv1.MaintenancePageSpec) []snapshot.Problem {
	ps := snapshot.ValidatePage(spec)
	if len(ps) > 0 {
		return ps
	}
	p, err := render.Parse(spec.HTML)
	if err != nil {
		return []snapshot.Problem{{Field: "html", Message: err.Error()}}
	}
	block := render.SampleFull
	block.Interstitial = false
	for _, tag := range slices.Sorted(maps.Keys(spec.Locales)) {
		txt := spec.Locales[tag]
		// Every locale, as an interstitial and a block with everything known,
		// and with nothing known, so no branch fails only for visitors.
		for _, in := range []render.Input{render.SampleFull, block, render.SampleMinimal} {
			in.Locale, in.Title, in.Message, in.Continue = tag, txt.Title, txt.Message, txt.ContinueLabel
			if _, err := p.Render(in); err != nil {
				ps = append(ps, snapshot.Problem{Field: "html", Message: fmt.Sprintf("locale %s: %v", tag, err)})
				break
			}
		}
	}
	return ps
}

// errorsByField groups problems for the forms.
func errorsByField(ps ...[]snapshot.Problem) map[string][]string {
	out := map[string][]string{}
	for _, list := range ps {
		for _, p := range list {
			out[p.Field] = append(out[p.Field], p.Message)
		}
	}
	return out
}

func itoa(i int) string { return strconv.Itoa(i) }

var funcs = template.FuncMap{
	"add":  func(a, b int) int { return a + b },
	"itoa": itoa,
	"field": func(prefix string, i int, rest string) string {
		return prefix + strconv.Itoa(i) + "." + rest
	},
	"esc":    pathEscape,
	"fields": strings.Fields,
}
