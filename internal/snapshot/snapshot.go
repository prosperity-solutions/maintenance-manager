// Package snapshot turns the custom resources and the HTTPRoutes into an
// immutable Snapshot: for every covered route, a timeline of decisions
// (docs/HANDOVER.md §3, §4.4, §6). Build is pure; Builder (builder.go) watches
// the cluster, debounces and swaps snapshots.
package snapshot

import (
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

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
	Key     RouteKey
	Labels  map[string]string
	Parents []gwapiv1.ParentReference // for the controller's conflict check (§5.3)
}

// Selectors compiles the route selectors of the valid services, by name.
// Invalid services are left out, so windows naming them skip them (§4.4).
func Selectors(services []mmv1.MaintenanceService) map[string]labels.Selector {
	out := map[string]labels.Selector{}
	for i := range services {
		s := &services[i]
		if len(ValidateService(&s.Spec)) > 0 {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&s.Spec.RouteSelector)
		if err != nil {
			continue
		}
		out[s.Name] = sel
	}
	return out
}

// WindowRoutes lists the routes any phase of a window reaches, through the
// services it names (§4.4 rule 7: arming covers every service named in any
// phase). The snapshot and the controller share this rule.
func WindowRoutes(spec *mmv1.MaintenanceWindowSpec, selectors map[string]labels.Selector, routes []RouteInfo) []RouteInfo {
	var out []RouteInfo
	seen := map[RouteKey]bool{}
	for _, svc := range ServicesOf(spec) {
		sel, ok := selectors[svc]
		if !ok {
			continue
		}
		for _, rt := range routes {
			if !seen[rt.Key] && sel.Matches(labels.Set(rt.Labels)) {
				seen[rt.Key] = true
				out = append(out, rt)
			}
		}
	}
	return out
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
	// Context is what a page template sees about where it sits: the window
	// and its current phase (from the window), and the previous and next
	// phase and the downtime (from this route's own timeline, so overlapping
	// windows and per-phase routings show what really happens to the route).
	Context render.Context

	// pendingNext: the window's next phase, when it is pending (no start).
	pendingNext render.Phase

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
	selectors := Selectors(in.Services)

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
			d.Context, d.pendingNext = windowContext(c, u)
			route.Timeline = append(route.Timeline, d)
		}
		fillRetryAfter(route.Timeline)
		fillRouteContext(route.Timeline)
		for i, d := range route.Timeline {
			k := decisionKey{d.Window, d.Phase, d.Page, nanos(d.From), nanos(d.To), nanos(d.Until), nanos(d.RetryAfter), contextKey(d.Context)}
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
	context                     string // contextKey: everything else a body shows
}

// contextKey identifies a Context by content, with instants as UnixNano.
func contextKey(c render.Context) string {
	ph := func(p render.Phase) string {
		return fmt.Sprintf("%q/%q/%q/%d/%d/%d", p.Window, p.Name, p.Action, nanos(p.Start), nanos(p.End), p.Number)
	}
	return fmt.Sprintf("%q/%d/%d/%d|%s|%s|%s|%t/%d/%d|%d", c.Window.Name, nanos(c.Window.Start), nanos(c.Window.End), c.Window.Phases,
		ph(c.Phase), ph(c.Previous), ph(c.Next), c.HasDowntime, nanos(c.Downtime.Start), nanos(c.Downtime.End), c.ContinueAfter)
}

// windowContext is the part of a decision's Context its own window decides:
// the window, the current phase, the auto-continue wait, and the next phase
// when it is pending.
func windowContext(c *candidate, until time.Time) (render.Context, render.Phase) {
	spec := c.spec
	segs := Segments(spec)
	i := c.seg.Phase
	ph := spec.Phases[i]
	ctx := render.Context{
		Window: render.Window{Name: c.window, Phases: len(spec.Phases)},
		Phase: render.Phase{Window: c.window, Name: ph.Name, Action: string(ph.Action),
			Start: c.seg.From, End: until, Number: i + 1},
		ContinueAfter: ph.AutoContinue(),
	}
	if len(segs) > 0 {
		ctx.Window.Start = segs[0].From
	}
	if spec.EndsAt != nil {
		ctx.Window.End = spec.EndsAt.UTC()
	}
	var pending render.Phase
	if n := i + 1; n < len(spec.Phases) && spec.Phases[n].StartsAt == nil {
		next := spec.Phases[n]
		pending = render.Phase{Window: c.window, Name: next.Name, Action: string(next.Action), Number: n + 1}
	}
	return ctx, pending
}

// fillRouteContext sets each decision's previous and next phase and its
// downtime from the route's timeline, so they show what happens to this
// route: the previous phase is the decision that ends where this one starts,
// the next one the decision that starts where it ends (or the window's
// pending next phase); the downtime is the first block decision at or after
// this one, from the start of its run of blocks to the run's end.
func fillRouteContext(tl []*Decision) {
	for i, d := range tl {
		if i > 0 && !d.From.IsZero() && tl[i-1].To.Equal(d.From) {
			p := tl[i-1]
			d.Context.Previous = render.Phase{Window: p.Window, Name: p.Phase, Action: string(p.Action),
				Start: p.From, End: p.To, Number: p.Context.Phase.Number}
		}
		switch {
		case i+1 < len(tl) && !d.To.IsZero() && tl[i+1].From.Equal(d.To):
			n := tl[i+1]
			d.Context.Next = render.Phase{Window: n.Window, Name: n.Phase, Action: string(n.Action),
				Start: n.From, End: n.Until, Number: n.Context.Phase.Number}
		case d.untilCut:
			d.Context.Next = d.pendingNext
		}
		for j := i; j < len(tl); j++ {
			if tl[j].Action != mmv1.ActionBlock {
				continue
			}
			// The run starts where its first block starts, also when this
			// decision is the run's second block.
			k := j
			for k > 0 && tl[k-1].Action == mmv1.ActionBlock && !tl[k-1].To.IsZero() && tl[k-1].To.Equal(tl[k].From) {
				k--
			}
			d.Context.Downtime = render.Span{Start: tl[k].From, End: tl[j].RetryAfter}
			d.Context.HasDowntime = true
			break
		}
	}
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
