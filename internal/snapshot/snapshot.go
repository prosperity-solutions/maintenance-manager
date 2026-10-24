// Package snapshot turns the custom resources and the HTTPRoutes into an
// immutable Snapshot: for every covered route, a timeline of decisions
// (docs/HANDOVER.md §3, §4.4, §6). Build is pure; Builder (builder.go) watches
// the cluster, debounces and swaps snapshots.
package snapshot

import (
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/locale"
	"github.com/prosperity-solutions/maintenance-manager/internal/render"
)

// RouteKey identifies an HTTPRoute. It is the key Check() looks up (§5.2).
type RouteKey struct {
	Namespace, Name string
}

// RouteInfo is what the snapshot needs from an HTTPRoute.
type RouteInfo struct {
	Key    RouteKey
	Labels map[string]string
}

// Page is a ready-to-render page: its template, locale matcher and texts.
type Page struct {
	Name     string // "" for the built-in default page
	Template *render.Page
	Locales  *locale.Matcher
	Texts    map[string]mmv1.PageText
}

// Text returns the texts for a resolved locale.
func (p *Page) Text(loc string) mmv1.PageText { return p.Texts[loc] }

// DefaultPage is used when a routing names no page, a missing page, or an
// invalid one (§4.2).
var DefaultPage = &Page{
	Template: render.Default,
	Locales:  locale.NewMatcher([]string{"en"}, "en"),
	Texts:    map[string]mmv1.PageText{"en": {Title: "We'll be right back", Message: "Scheduled maintenance."}},
}

// Decision is what happens to a route during [From, To). Zero From is
// "since the window was enabled"; zero To is "until it is disabled".
type Decision struct {
	From, To   time.Time
	Action     mmv1.Action
	Window     string
	Phase      string
	PhaseKey   string    // interstitial cookie value
	Cookie     string    // interstitial cookie name, per window
	Until      time.Time // end of the current phase, zero if unknown (.Until)
	RetryAfter time.Time // end of the contiguous block run, zero if unknown
	Page       *Page

	// untilCut: this decision ends where its phase ends, and the next phase
	// is pending, so the real end is unknown.
	untilCut bool
}

// Contains reports whether t falls in [From, To).
func (d *Decision) Contains(t time.Time) bool {
	return (d.From.IsZero() || !t.Before(d.From)) && (d.To.IsZero() || t.Before(d.To))
}

// Route is one covered route and its timeline, sorted by From.
type Route struct {
	Key      RouteKey
	Timeline []*Decision
}

// At returns the decision at t, or nil. Timelines hold a few entries, so a
// linear scan beats anything clever.
func (r *Route) At(t time.Time) *Decision {
	for _, d := range r.Timeline {
		if d.Contains(t) {
			return d
		}
	}
	return nil
}

// Snapshot is immutable once built: the hot path reads it without locks.
type Snapshot struct {
	// Routes holds every route covered by an enabled, valid, not-ended
	// window, armed or not (§6).
	Routes map[RouteKey]*Route
	// Bypass is the bypass token; empty disables the bypass (§7).
	Bypass string
	// Built is when the snapshot was built.
	Built time.Time
}

// Input is everything Build reads.
type Input struct {
	Windows  []mmv1.MaintenanceWindow
	Pages    []mmv1.MaintenancePage
	Services []mmv1.MaintenanceService
	Routes   []RouteInfo
	Bypass   string
	Now      time.Time
}

// Build computes a snapshot. It never fails: invalid windows are left out,
// invalid or missing pages become the default page, missing services are
// skipped (§4.4 rule 9).
func Build(in Input) *Snapshot {
	snap := &Snapshot{Routes: map[RouteKey]*Route{}, Bypass: in.Bypass, Built: in.Now}

	pages := map[string]*Page{}
	for i := range in.Pages {
		p := &in.Pages[i]
		if pg, ok := preparePage(p.Name, &p.Spec); ok {
			pages[p.Name] = pg
		}
	}
	selectors := map[string]labels.Selector{}
	for i := range in.Services {
		s := &in.Services[i]
		if len(ValidateService(&s.Spec)) > 0 {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&s.Spec.RouteSelector)
		if err != nil {
			continue
		}
		selectors[s.Name] = sel
	}

	cands := map[RouteKey][]*candidate{}
	for i := range in.Windows {
		w := &in.Windows[i]
		if !w.Spec.Enabled || Ended(&w.Spec, in.Now) || len(ValidateWindow(&w.Spec)) > 0 {
			continue
		}
		for _, seg := range Segments(&w.Spec) {
			// One candidate per (route, segment): the first routing that
			// reaches the route wins, in routing and service order.
			taken := map[RouteKey]bool{}
			for _, r := range w.Spec.Phases[seg.Phase].Routings {
				for _, svc := range r.Services {
					sel, ok := selectors[svc]
					if !ok {
						continue
					}
					for _, rt := range in.Routes {
						if taken[rt.Key] || !sel.Matches(labels.Set(rt.Labels)) {
							continue
						}
						taken[rt.Key] = true
						cands[rt.Key] = append(cands[rt.Key], &candidate{
							seg: seg, window: w.Name, uid: string(w.UID), spec: &w.Spec, page: r.Page, service: svc,
						})
					}
				}
			}
		}
	}

	// Identical decisions are interned, so routes of the same window, phase
	// and page share one *Decision and authz renders it once, however many
	// routes a service selects.
	interned := map[decisionKey]*Decision{}
	for key, cs := range cands {
		route := &Route{Key: key}
		for _, iv := range merge(cs) {
			c := iv.winner
			ph := c.spec.Phases[c.seg.Phase]
			page := pages[c.page]
			if page == nil {
				page = DefaultPage
			}
			u := until(c.spec, c.seg)
			d := &Decision{
				From: iv.from, To: iv.to,
				untilCut: u.IsZero() && iv.to.Equal(c.seg.To),
				Action:   ph.Action, Window: c.window, Phase: ph.Name,
				PhaseKey: PhaseKey(ph), Cookie: CookieName(c.uid),
				Until: u, Page: page,
			}
			route.Timeline = append(route.Timeline, d)
		}
		fillRetryAfter(route.Timeline)
		for i, d := range route.Timeline {
			k := decisionKey{d.Window, d.Phase, d.Page, nanos(d.From), nanos(d.To), nanos(d.Until), nanos(d.RetryAfter)}
			if shared, ok := interned[k]; ok {
				route.Timeline[i] = shared
			} else {
				interned[k] = d
			}
		}
		snap.Routes[key] = route
	}
	return snap
}

// decisionKey identifies a decision's content; equal keys mean equal
// responses. Times are instants (UnixNano), so a monotonic reading or a
// Location can never make equal instants unequal keys.
type decisionKey struct {
	window, phase               string
	page                        *Page
	from, to, until, retryAfter int64
}

// nanos is t as UnixNano, with the zero time (= open) as 0.
func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// until is the end of the window's current phase: the next phase's start or
// endsAt, zero when the next phase is pending (§4.2).
func until(spec *mmv1.MaintenanceWindowSpec, seg Segment) time.Time {
	if next := seg.Phase + 1; next < len(spec.Phases) && spec.Phases[next].StartsAt == nil {
		return time.Time{}
	}
	return seg.To
}

// fillRetryAfter sets, on every block decision, the end of the contiguous run
// of block decisions it belongs to; zero when that run is open-ended (§6).
func fillRetryAfter(tl []*Decision) {
	for i, d := range tl {
		if d.Action != mmv1.ActionBlock {
			continue
		}
		last := d
		for j := i + 1; j < len(tl) && !last.To.IsZero() && tl[j].Action == mmv1.ActionBlock && tl[j].From.Equal(last.To); j++ {
			last = tl[j]
		}
		// A run that ends where its phase ends but whose next phase is
		// pending ends whenever someone starts that phase: unknown, like
		// .Until (§6). A run cut short by another window ends at To.
		if last.Until.IsZero() && (last.To.IsZero() || last.untilCut) {
			d.RetryAfter = time.Time{}
			continue
		}
		d.RetryAfter = last.To
	}
}

// preparePage validates a page; an invalid page is left out, so routings
// naming it fall back to the default page.
func preparePage(name string, spec *mmv1.MaintenancePageSpec) (*Page, bool) {
	if len(ValidatePage(spec)) > 0 {
		return nil, false
	}
	tpl, err := render.Parse(spec.HTML)
	if err != nil {
		return nil, false
	}
	locs := make([]string, 0, len(spec.Locales))
	for l := range spec.Locales {
		locs = append(locs, l)
	}
	slices.Sort(locs)
	return &Page{
		Name:     name,
		Template: tpl,
		Locales:  locale.NewMatcher(locs, spec.FallbackLocale),
		Texts:    spec.Locales,
	}, true
}

// ValidatePage checks §4.2: the fallback locale exists, and the template
// parses, executes and stays within 64 KiB.
func ValidatePage(spec *mmv1.MaintenancePageSpec) []Problem {
	var ps []Problem
	if len(spec.Locales) == 0 {
		ps = append(ps, Problem{Field: "l0.tag", Message: "a page needs at least one locale"})
	} else if _, ok := spec.Locales[spec.FallbackLocale]; !ok {
		ps = append(ps, Problem{Field: "fallbackLocale", Message: "the fallback locale " + quote(spec.FallbackLocale) + " is not one of the page's locales"})
	}
	if _, err := render.Parse(spec.HTML); err != nil {
		ps = append(ps, Problem{Field: "html", Message: err.Error()})
	}
	return ps
}

func quote(s string) string { return `"` + s + `"` }
