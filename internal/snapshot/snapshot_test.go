package snapshot

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
)

var t0 = time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)

func at(h, m int) *metav1.Time {
	t := metav1.NewTime(time.Date(2026, 10, 30, h, m, 0, 0, time.UTC))
	return &t
}

func phase(name string, start *metav1.Time, action mmv1.Action, services ...string) mmv1.Phase {
	return mmv1.Phase{Name: name, StartsAt: start, Action: action, Routings: []mmv1.Routing{{Services: services}}}
}

func window(name string, enabled bool, ends *metav1.Time, phases ...mmv1.Phase) mmv1.MaintenanceWindow {
	return mmv1.MaintenanceWindow{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec:       mmv1.MaintenanceWindowSpec{Enabled: enabled, EndsAt: ends, Phases: phases},
	}
}

func fields(ps []Problem) string {
	var f []string
	for _, p := range ps {
		f = append(f, p.Field)
	}
	return strings.Join(f, ",")
}

func TestValidateWindow(t *testing.T) {
	cases := []struct {
		name string
		spec mmv1.MaintenanceWindowSpec
		want string // problem fields, "" = valid
	}{
		{"undated phase 0 then dated", mmv1.MaintenanceWindowSpec{Phases: []mmv1.Phase{phase("warn", nil, mmv1.ActionBanner, "a"), phase("down", at(20, 0), mmv1.ActionBlock, "a")}}, ""},
		{"pending then pending", mmv1.MaintenanceWindowSpec{Phases: []mmv1.Phase{phase("a", at(16, 0), mmv1.ActionBanner, "a"), phase("b", nil, mmv1.ActionBlock, "a"), phase("c", nil, mmv1.ActionBlock, "a")}}, ""},
		{"dated after pending", mmv1.MaintenanceWindowSpec{Phases: []mmv1.Phase{phase("a", nil, mmv1.ActionBanner, "a"), phase("b", nil, mmv1.ActionBlock, "a"), phase("c", at(20, 0), mmv1.ActionBlock, "a")}}, "p2.startsAt"},
		{"not increasing", mmv1.MaintenanceWindowSpec{Phases: []mmv1.Phase{phase("a", at(20, 0), mmv1.ActionBanner, "a"), phase("b", at(20, 0), mmv1.ActionBlock, "a")}}, "p1.startsAt"},
		{"endsAt before last start", mmv1.MaintenanceWindowSpec{EndsAt: at(19, 0), Phases: []mmv1.Phase{phase("a", at(16, 0), mmv1.ActionBanner, "a"), phase("b", at(20, 0), mmv1.ActionBlock, "a")}}, "endsAt"},
		{"no phases", mmv1.MaintenanceWindowSpec{}, "phases"},
		{"bad action", mmv1.MaintenanceWindowSpec{Phases: []mmv1.Phase{phase("a", nil, "nap", "a")}}, "p0.action"},
		{"duplicate phase name", mmv1.MaintenanceWindowSpec{Phases: []mmv1.Phase{phase("a", nil, mmv1.ActionBanner, "x"), phase("a", at(20, 0), mmv1.ActionBlock, "x")}}, "p1.name"},
		{"service twice in a phase", mmv1.MaintenanceWindowSpec{Phases: []mmv1.Phase{{Name: "a", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"x"}}, {Services: []string{"x"}}}}}}, "p0.r1.services"},
		{"empty routing", mmv1.MaintenanceWindowSpec{Phases: []mmv1.Phase{{Name: "a", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{}}}}}, "p0.r0.services"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fields(ValidateWindow(&tc.spec)); got != tc.want {
				t.Fatalf("problems %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSegments(t *testing.T) {
	spec := mmv1.MaintenanceWindowSpec{EndsAt: at(22, 30), Phases: []mmv1.Phase{
		phase("warn", nil, mmv1.ActionBanner, "a"),
		phase("down", at(20, 0), mmv1.ActionBlock, "a"),
	}}
	got := Segments(&spec)
	if len(got) != 2 || !got[0].From.IsZero() || !got[0].To.Equal(at(20, 0).Time) ||
		!got[1].From.Equal(at(20, 0).Time) || !got[1].To.Equal(at(22, 30).Time) {
		t.Fatalf("segments %+v", got)
	}

	// A pending phase never starts: the phase before runs to endsAt.
	spec.Phases[1].StartsAt = nil
	if got := Segments(&spec); len(got) != 1 || !got[0].To.Equal(at(22, 30).Time) {
		t.Fatalf("pending: %+v", got)
	}

	// A phase starting at or after endsAt never becomes active.
	spec.Phases[1].StartsAt = at(23, 0)
	if got := Segments(&spec); len(got) != 1 {
		t.Fatalf("after endsAt: %+v", got)
	}
}

func TestActivePhaseAndArmed(t *testing.T) {
	spec := mmv1.MaintenanceWindowSpec{Enabled: true, EndsAt: at(22, 30), Phases: []mmv1.Phase{
		phase("warn", at(16, 0), mmv1.ActionBanner, "a"),
		phase("down", at(20, 0), mmv1.ActionBlock, "a"),
	}}
	lead := 15 * time.Minute
	cases := []struct {
		now    *metav1.Time
		active int
		armed  bool
	}{
		{at(15, 0), -1, false},
		{at(15, 45), -1, true}, // within the lead time
		{at(16, 0), 0, true},
		{at(20, 0), 1, true},
		{at(22, 30), -1, false}, // ended
	}
	for _, tc := range cases {
		if got := ActivePhase(&spec, tc.now.Time); got != tc.active {
			t.Errorf("ActivePhase at %s = %d, want %d", tc.now.Format("15:04"), got, tc.active)
		}
		if got := Armed(&spec, tc.now.Time, lead); got != tc.armed {
			t.Errorf("Armed at %s = %v, want %v", tc.now.Format("15:04"), got, tc.armed)
		}
	}
	spec.Enabled = false
	if Armed(&spec, at(16, 0).Time, lead) || ActivePhase(&spec, at(16, 0).Time) != -1 {
		t.Fatal("a disabled window is never armed or active")
	}
	if got := NextBoundary(&mmv1.MaintenanceWindowSpec{Enabled: true, Phases: spec.Phases, EndsAt: spec.EndsAt}, at(16, 30).Time, lead); !got.Equal(at(19, 45).Time) {
		t.Fatalf("NextBoundary = %s, want 19:45", got.Format("15:04"))
	}
}

func TestHashes(t *testing.T) {
	if got := Hash8("abc"); got != "ba7816bf" {
		t.Fatalf("Hash8 = %s", got)
	}
	if got := CookieName("u"); !strings.HasPrefix(got, "mm_seen_") || len(got) != len("mm_seen_")+8 {
		t.Fatalf("CookieName = %s", got)
	}
	a := PhaseKey(phase("down", at(20, 0), mmv1.ActionBlock))
	b := PhaseKey(phase("down", at(20, 1), mmv1.ActionBlock))
	if a == b || len(a) != 8 {
		t.Fatalf("PhaseKey must change with startsAt: %s %s", a, b)
	}
}

func services(names ...string) []mmv1.MaintenanceService {
	var out []mmv1.MaintenanceService
	for _, n := range names {
		out = append(out, mmv1.MaintenanceService{
			ObjectMeta: metav1.ObjectMeta{Name: n},
			Spec:       mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": n}}},
		})
	}
	return out
}

var shopRoute = RouteInfo{Key: RouteKey{Namespace: "shop", Name: "web"}, Labels: map[string]string{"app": "shop"}}

func TestBuildTimelineUntilAndRetryAfter(t *testing.T) {
	w := window("db", true, at(22, 30),
		phase("warn", at(16, 0), mmv1.ActionBanner, "shop"),
		phase("down", at(20, 0), mmv1.ActionBlock, "shop"),
	)
	snap := Build(Input{Windows: []mmv1.MaintenanceWindow{w}, Services: services("shop"), Routes: []RouteInfo{shopRoute}, Now: t0})
	r := snap.Routes[shopRoute.Key]
	if r == nil || len(r.Timeline) != 2 {
		t.Fatalf("timeline: %+v", r)
	}
	warn, down := r.Timeline[0], r.Timeline[1]
	if warn.Action != mmv1.ActionBanner || !warn.Until.Equal(at(20, 0).Time) || !warn.RetryAfter.IsZero() {
		t.Fatalf("warn: %+v", warn)
	}
	if down.Action != mmv1.ActionBlock || !down.Until.Equal(at(22, 30).Time) || !down.RetryAfter.Equal(at(22, 30).Time) {
		t.Fatalf("down: %+v", down)
	}
	if down.Cookie != CookieName("uid-db") || down.Page != DefaultPage {
		t.Fatalf("down cookie/page: %+v", down)
	}
	if got := r.At(at(21, 0).Time); got != down {
		t.Fatal("At(21:00) is not the block decision")
	}
	if r.At(at(15, 0).Time) != nil || r.At(at(22, 30).Time) != nil {
		t.Fatal("no decision outside the window")
	}
}

func TestBuildUntilUnknownWhenNextPending(t *testing.T) {
	w := window("db", true, nil,
		phase("warn", nil, mmv1.ActionBanner, "shop"),
		phase("down", nil, mmv1.ActionBlock, "shop"),
	)
	snap := Build(Input{Windows: []mmv1.MaintenanceWindow{w}, Services: services("shop"), Routes: []RouteInfo{shopRoute}, Now: t0})
	d := snap.Routes[shopRoute.Key].Timeline
	if len(d) != 1 || !d[0].Until.IsZero() || !d[0].From.IsZero() || !d[0].To.IsZero() {
		t.Fatalf("warn with pending next: %+v", d)
	}
}

func TestBuildOverlap(t *testing.T) {
	// "a" blocks 20:00-21:00; "b" interstitial 19:00-22:00. Block wins where
	// both apply; the tie between equal actions goes to the smaller name.
	a := window("a", true, at(21, 0), phase("down", at(20, 0), mmv1.ActionBlock, "shop"))
	b := window("b", true, at(22, 0), phase("int", at(19, 0), mmv1.ActionInterstitial, "shop"))
	snap := Build(Input{Windows: []mmv1.MaintenanceWindow{b, a}, Services: services("shop"), Routes: []RouteInfo{shopRoute}, Now: t0})
	tl := snap.Routes[shopRoute.Key].Timeline
	var got []string
	for _, d := range tl {
		got = append(got, d.Window+":"+string(d.Action)+"@"+d.From.Format("15:04")+"-"+d.To.Format("15:04"))
	}
	want := "b:interstitial@19:00-20:00 a:block@20:00-21:00 b:interstitial@21:00-22:00"
	if strings.Join(got, " ") != want {
		t.Fatalf("timeline %v\nwant      %s", got, want)
	}
	// The block run ends at 21:00.
	if !tl[1].RetryAfter.Equal(at(21, 0).Time) {
		t.Fatalf("RetryAfter %s", tl[1].RetryAfter)
	}

	tie1 := window("zeta", true, nil, phase("x", at(20, 0), mmv1.ActionBlock, "shop"))
	tie2 := window("alpha", true, nil, phase("y", at(20, 0), mmv1.ActionBlock, "shop"))
	snap = Build(Input{Windows: []mmv1.MaintenanceWindow{tie1, tie2}, Services: services("shop"), Routes: []RouteInfo{shopRoute}, Now: t0})
	if w := snap.Routes[shopRoute.Key].Timeline[0].Window; w != "alpha" {
		t.Fatalf("tie went to %s, want alpha", w)
	}
}

func TestBuildContiguousBlockRun(t *testing.T) {
	// Two block phases in a row: Retry-After is the end of the run.
	w := window("db", true, at(23, 0),
		phase("down1", at(20, 0), mmv1.ActionBlock, "shop"),
		phase("down2", at(21, 0), mmv1.ActionBlock, "shop"),
	)
	snap := Build(Input{Windows: []mmv1.MaintenanceWindow{w}, Services: services("shop"), Routes: []RouteInfo{shopRoute}, Now: t0})
	tl := snap.Routes[shopRoute.Key].Timeline
	if len(tl) != 2 || !tl[0].RetryAfter.Equal(at(23, 0).Time) || !tl[0].Until.Equal(at(21, 0).Time) {
		t.Fatalf("run: %+v %+v", tl[0], tl[1])
	}
}

func TestBuildSkips(t *testing.T) {
	routes := []RouteInfo{shopRoute, {Key: RouteKey{Namespace: "other", Name: "x"}, Labels: map[string]string{"app": "other"}}}
	cases := []struct {
		name string
		w    mmv1.MaintenanceWindow
	}{
		{"disabled", window("w", false, nil, phase("p", nil, mmv1.ActionBlock, "shop"))},
		{"ended", window("w", true, at(15, 0), phase("p", at(14, 0), mmv1.ActionBlock, "shop"))},
		{"invalid", window("w", true, nil, phase("", nil, mmv1.ActionBlock, "shop"))},
		{"missing service", window("w", true, nil, phase("p", nil, mmv1.ActionBlock, "nope"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := Build(Input{Windows: []mmv1.MaintenanceWindow{tc.w}, Services: services("shop"), Routes: routes, Now: t0})
			if len(snap.Routes) != 0 {
				t.Fatalf("covered routes: %v", snap.Routes)
			}
		})
	}
	// A phase that names another service does nothing for this route, but
	// the window still covers the route through another phase.
	w := window("w", true, nil,
		phase("warn", nil, mmv1.ActionBanner, "other"),
		phase("down", at(20, 0), mmv1.ActionBlock, "shop"),
	)
	snap := Build(Input{Windows: []mmv1.MaintenanceWindow{w}, Services: services("shop", "other"), Routes: routes, Now: t0})
	tl := snap.Routes[shopRoute.Key].Timeline
	if len(tl) != 1 || tl[0].Action != mmv1.ActionBlock {
		t.Fatalf("shop timeline %+v", tl)
	}
}

func TestBuildPages(t *testing.T) {
	good := mmv1.MaintenancePage{ObjectMeta: metav1.ObjectMeta{Name: "nice"}, Spec: mmv1.MaintenancePageSpec{
		FallbackLocale: "en",
		Locales:        map[string]mmv1.PageText{"en": {Title: "Back soon"}, "de": {Title: "Gleich zurück"}},
	}}
	bad := mmv1.MaintenancePage{ObjectMeta: metav1.ObjectMeta{Name: "broken"}, Spec: mmv1.MaintenancePageSpec{
		FallbackLocale: "fr", Locales: map[string]mmv1.PageText{"en": {}},
	}}
	w := window("w", true, nil, mmv1.Phase{Name: "p", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"shop"}, Page: "nice"}}})
	snap := Build(Input{Windows: []mmv1.MaintenanceWindow{w}, Pages: []mmv1.MaintenancePage{good, bad}, Services: services("shop"), Routes: []RouteInfo{shopRoute}, Now: t0})
	p := snap.Routes[shopRoute.Key].Timeline[0].Page
	if p.Name != "nice" || p.Locales.Resolve("", "de-CH") != "de" {
		t.Fatalf("page %+v", p)
	}
	w.Spec.Phases[0].Routings[0].Page = "broken"
	snap = Build(Input{Windows: []mmv1.MaintenanceWindow{w}, Pages: []mmv1.MaintenancePage{good, bad}, Services: services("shop"), Routes: []RouteInfo{shopRoute}, Now: t0})
	if snap.Routes[shopRoute.Key].Timeline[0].Page != DefaultPage {
		t.Fatal("an invalid page must fall back to the default page")
	}
}

func TestValidateService(t *testing.T) {
	if ps := ValidateService(&mmv1.MaintenanceServiceSpec{}); len(ps) != 1 {
		t.Fatal("an empty selector must be rejected")
	}
	ok := mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}}
	if ps := ValidateService(&ok); len(ps) != 0 {
		t.Fatalf("valid selector rejected: %v", ps)
	}
}

func TestWarnings(t *testing.T) {
	spec := mmv1.MaintenanceWindowSpec{Phases: []mmv1.Phase{{Name: "p", Action: mmv1.ActionBlock, Routings: []mmv1.Routing{{Services: []string{"a", "gone"}, Page: "missing"}}}}}
	got := fields(Warnings(&spec, map[string]bool{"a": true}, map[string]bool{}))
	if got != "p0.r0.page,p0.r0.services" {
		t.Fatalf("warnings %q", got)
	}
}
