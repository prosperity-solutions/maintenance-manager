package editor

import (
	"fmt"
	"math/rand/v2"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

func mt(s string) *metav1.Time { return &metav1.Time{Time: utc(s)} }

func fields(ps []snapshot.Problem) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Field)
	}
	return out
}

func hasField(ps []snapshot.Problem, f string) bool {
	for _, p := range ps {
		if p.Field == f {
			return true
		}
	}
	return false
}

func noProblems(t *testing.T, ps []snapshot.Problem) {
	t.Helper()
	if len(ps) != 0 {
		t.Fatalf("unexpected problems: %v", ps)
	}
}

// ---- generators ----

type gen struct{ r *rand.Rand }

func newGen(seed uint64) *gen {
	return &gen{rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))} //nolint:gosec // deterministic test data
}

func (g *gen) n(lo, hi int) int { return lo + g.r.IntN(hi-lo+1) }

var words = []string{"alpha", "beta", "shop", "blog", "api", "tonight", "classic-503", "größe", "日本語", "emoji-😀", "a b c", "x\ny"}

func (g *gen) word() string { return words[g.r.IntN(len(words))] }

// name is a DNS-label-like token (decode trims whitespace, so tokens have none).
func (g *gen) name(prefix string) string { return fmt.Sprintf("%s-%d", prefix, g.r.IntN(1000)) }

func (g *gen) instant() time.Time {
	base := utc("2026-01-01T00:00:00Z").Unix()
	return time.Unix(base+g.r.Int64N(365*86400), 0).UTC()
}

func (g *gen) service() mmv1.MaintenanceServiceSpec {
	var spec mmv1.MaintenanceServiceSpec
	spec.RouteSelector.MatchLabels = map[string]string{}
	for range g.n(1, 5) {
		spec.RouteSelector.MatchLabels[g.name("k")+".io/x"] = g.name("v")
	}
	if g.r.IntN(2) == 0 {
		spec.RouteSelector.MatchExpressions = []metav1.LabelSelectorRequirement{
			{Key: g.name("e"), Operator: metav1.LabelSelectorOpIn, Values: []string{"a", "b"}},
		}
	}
	return spec
}

func (g *gen) page() mmv1.MaintenancePageSpec {
	spec := mmv1.MaintenancePageSpec{Locales: map[string]mmv1.PageText{}}
	for range g.n(0, 6) {
		spec.Locales[g.name("l")] = mmv1.PageText{Title: g.word(), Message: g.word() + "\n" + g.word()}
	}
	for tag := range spec.Locales {
		spec.FallbackLocale = tag
		break
	}
	if g.r.IntN(2) == 0 {
		spec.HTML = "<!doctype html>\n<h1>{{.Title}}</h1>\n<p>" + g.word() + "</p>\n"
	}
	return spec
}

func (g *gen) window(loc *time.Location) mmv1.MaintenanceWindowSpec {
	spec := mmv1.MaintenanceWindowSpec{Enabled: g.r.IntN(2) == 0, Phases: []mmv1.Phase{}}
	at := g.instant()
	for i := range g.n(1, 5) {
		ph := mmv1.Phase{
			Name:     g.name("ph"),
			Action:   []mmv1.Action{mmv1.ActionBanner, mmv1.ActionInterstitial, mmv1.ActionBlock}[g.r.IntN(3)],
			Routings: []mmv1.Routing{},
		}
		switch {
		case i == 0 && g.r.IntN(2) == 0: // undated phase 0
		case i > 0 && g.r.IntN(3) == 0: // pending (and the rest after it)
		default:
			at = at.Add(time.Duration(g.n(1, 40000)) * time.Second)
			ph.StartsAt = &metav1.Time{Time: at}
		}
		for range g.n(1, 4) {
			r := mmv1.Routing{Services: []string{}}
			for range g.n(1, 6) {
				r.Services = append(r.Services, g.name("svc")) // submitted order, not sorted
			}
			if g.r.IntN(3) > 0 {
				r.Page = g.name("page")
			}
			ph.Routings = append(ph.Routings, r)
		}
		spec.Phases = append(spec.Phases, ph)
	}
	// A pending phase makes every later one pending.
	pending := false
	for i := range spec.Phases {
		if i > 0 && spec.Phases[i].StartsAt == nil {
			pending = true
		}
		if pending {
			spec.Phases[i].StartsAt = nil
		}
	}
	if g.r.IntN(2) == 0 {
		spec.EndsAt = &metav1.Time{Time: at.Add(time.Hour)}
	}
	return spec
}

// ---- round trips ----

func TestServiceRoundTrip(t *testing.T) {
	specs := []mmv1.MaintenanceServiceSpec{
		{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}},
		{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"platform.example/environment-id": "0000", "z": "", "a": "ü"}}},
		{RouteSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "k", Operator: metav1.LabelSelectorOpExists}}}},
	}
	g := newGen(1)
	for range 200 {
		specs = append(specs, g.service())
	}
	for i, spec := range specs {
		got, ps := DecodeService(EncodeService(&spec), &spec)
		noProblems(t, ps)
		if !equality.Semantic.DeepEqual(spec, got) {
			t.Fatalf("spec %d: got %+v, want %+v", i, got, spec)
		}
	}
}

func TestPageRoundTrip(t *testing.T) {
	specs := []mmv1.MaintenancePageSpec{
		{Locales: map[string]mmv1.PageText{}},
		{FallbackLocale: "en", Locales: map[string]mmv1.PageText{"en": {Title: "We'll be right back"}}},
		{FallbackLocale: "de", HTML: "<h1>{{.Title}}</h1>\n\n<p>ö</p>", Locales: map[string]mmv1.PageText{
			"en": {Title: "t", Message: "line1\nline2"}, "de-CH": {Message: "日本語 😀"}, "de": {},
		}},
	}
	g := newGen(2)
	for range 200 {
		specs = append(specs, g.page())
	}
	for i, spec := range specs {
		got, ps := DecodePage(EncodePage(&spec))
		noProblems(t, ps)
		if !equality.Semantic.DeepEqual(spec, got) {
			t.Fatalf("spec %d: got %+v, want %+v", i, got, spec)
		}
	}
}

func TestWindowRoundTrip(t *testing.T) {
	berlin := mustZone(t, "Europe/Berlin")
	ny := mustZone(t, "America/New_York")
	specs := []mmv1.MaintenanceWindowSpec{
		{Phases: []mmv1.Phase{}},
		{Enabled: true, Phases: []mmv1.Phase{{
			Name: "down", Action: mmv1.ActionBlock, StartsAt: mt("2026-10-30T20:00:00Z"),
			Routings: []mmv1.Routing{{Services: []string{"z", "a", "m"}, Page: "classic-503"}},
		}}},
		{ // undated phase 0, pending later phase, the second copy of the autumn overlap hour
			EndsAt: mt("2026-10-25T01:45:00Z"),
			Phases: []mmv1.Phase{
				{Name: "warn", Action: mmv1.ActionBanner, Routings: []mmv1.Routing{{Services: []string{"b", "a"}}}},
				{Name: "down", Action: mmv1.ActionBlock, StartsAt: mt("2026-10-25T01:30:00Z"), Routings: []mmv1.Routing{{Services: []string{"a"}, Page: "p"}, {Services: []string{"b"}}}},
				{Name: "up", Action: mmv1.ActionInterstitial, Routings: []mmv1.Routing{{Services: []string{"ü"}}}},
			},
		},
	}
	g := newGen(3)
	for range 300 {
		specs = append(specs, g.window(nil))
	}
	for i, spec := range specs {
		for _, loc := range []*time.Location{time.UTC, berlin, ny} {
			got, ps := DecodeWindow(EncodeWindow(&spec, loc), loc)
			noProblems(t, ps)
			if !equality.Semantic.DeepEqual(spec, got) {
				t.Fatalf("spec %d in %s: got %+v, want %+v", i, loc, got, spec)
			}
		}
	}
}

// ---- windows ----

func TestWindowUntouchedTimeKeepsStoredInstant(t *testing.T) {
	berlin := mustZone(t, "Europe/Berlin")
	second := mt("2026-10-25T01:30:00Z") // 02:30 CET, the second 02:30 of the day
	spec := mmv1.MaintenanceWindowSpec{EndsAt: second, Phases: []mmv1.Phase{{
		Name: "a", Action: mmv1.ActionBanner, StartsAt: second,
		Routings: []mmv1.Routing{{Services: []string{"s"}}},
	}}}
	v := EncodeWindow(&spec, berlin)
	if v.Get("endsAt") != "2026-10-25T02:30:00" || v.Get("endsAtWas") != "2026-10-25T01:30:00Z" {
		t.Fatalf("encoded %v", v)
	}
	got, ps := DecodeWindow(v, berlin)
	noProblems(t, ps)
	if !got.EndsAt.Equal(second) || !got.Phases[0].StartsAt.Equal(second) {
		t.Fatalf("stored instant lost: %v / %v", got.EndsAt, got.Phases[0].StartsAt)
	}
	// The browser may drop zero seconds; still untouched.
	v.Set("endsAt", "2026-10-25T02:30")
	got, _ = DecodeWindow(v, berlin)
	if !got.EndsAt.Equal(second) {
		t.Fatalf("minute-precision value lost the stored instant: %v", got.EndsAt)
	}
	// Without the hidden field the same text means the earlier instant.
	v.Del("endsAtWas")
	got, _ = DecodeWindow(v, berlin)
	if want := utc("2026-10-25T00:30:00Z"); !got.EndsAt.Time.Equal(want) {
		t.Fatalf("got %v, want %v", got.EndsAt, want)
	}
}

func TestWindowChangedTimeParsedInZone(t *testing.T) {
	berlin := mustZone(t, "Europe/Berlin")
	v := url.Values{
		"p0.name": {"a"}, "p0.order": {"1"}, "p0.action": {"block"},
		"p0.startsAt": {"2026-10-30T18:00"}, "p0.startsAtWas": {"2026-10-30T10:00:00Z"},
		"p0.r0.services": {"s"},
		"endsAt":         {"2026-10-31T00:00:00.999"}, "endsAtWas": {""},
	}
	got, ps := DecodeWindow(v, berlin)
	noProblems(t, ps)
	if want := utc("2026-10-30T17:00:00Z"); !got.Phases[0].StartsAt.Time.Equal(want) {
		t.Fatalf("startsAt = %v", got.Phases[0].StartsAt)
	}
	if want := utc("2026-10-30T23:00:00Z"); !got.EndsAt.Time.Equal(want) {
		t.Fatalf("endsAt = %v", got.EndsAt)
	}
	// Clearing the field clears the time even though a stored one exists.
	v.Set("p0.startsAt", "")
	got, ps = DecodeWindow(v, berlin)
	noProblems(t, ps)
	if got.Phases[0].StartsAt != nil {
		t.Fatalf("startsAt = %v, want nil", got.Phases[0].StartsAt)
	}
}

func TestWindowTimeProblems(t *testing.T) {
	berlin := mustZone(t, "Europe/Berlin")
	for name, tc := range map[string]struct{ value, field string }{
		"gap":     {"2026-03-29T02:30", "p0.startsAt"},
		"garbage": {"soon", "p0.startsAt"},
		"range":   {"0000-01-01T00:00", "p0.startsAt"},
	} {
		t.Run(name, func(t *testing.T) {
			v := url.Values{"p0.name": {"a"}, "p0.action": {"banner"}, "p0.order": {"1"}, "p0.startsAt": {tc.value}, "p0.r0.services": {"s"}}
			_, ps := DecodeWindow(v, berlin)
			if !hasField(ps, tc.field) {
				t.Fatalf("problems = %v", ps)
			}
		})
	}
	_, ps := DecodeWindow(url.Values{"endsAt": {"2026-03-29T02:30"}}, berlin)
	if !hasField(ps, "endsAt") || !strings.Contains(ps[0].Message, "clocks jump forward") {
		t.Fatalf("problems = %v", ps)
	}
}

func TestWindowRowOrderingAndBlanks(t *testing.T) {
	v := url.Values{
		"enabled": {"on"},
		// Numeric, not lexical: p2 < p10, and r9 < r10.
		"p10.order": {"1"}, "p10.name": {"first"}, "p10.action": {"banner"},
		"p10.r10.services": {"late"}, "p10.r9.services": {"early1", "early2"}, "p10.r9.page": {"pg"},
		"p2.order": {"2"}, "p2.name": {"second"}, "p2.action": {"block"}, "p2.r0.services": {"x"},
		// Blank rows: nothing but defaults.
		"p3.order": {"3"}, "p3.action": {"banner"}, "p3.r0.page": {""},
		"p4.order": {"4"}, "p4.name": {""}, "p4.r0.services": {""},
		// Unknown fields and malformed indices are ignored.
		"p+1.name": {"x"}, "p-1.name": {"x"}, "pX.name": {"x"}, "p.name": {"x"}, "p1name": {"x"}, "q1.name": {"x"},
		"p5.bogus": {"x"}, "p1234567890.name": {"x"}, "resourceVersion": {"7"}, "tz": {"UTC"},
	}
	got, ps := DecodeWindow(v, time.UTC)
	noProblems(t, ps)
	if !got.Enabled || len(got.Phases) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got.Phases[0].Name != "first" || got.Phases[1].Name != "second" {
		t.Fatalf("order: %v, %v", got.Phases[0].Name, got.Phases[1].Name)
	}
	rs := got.Phases[0].Routings
	if len(rs) != 2 || rs[0].Services[0] != "early1" || rs[0].Page != "pg" || rs[1].Services[0] != "late" {
		t.Fatalf("routings = %+v", rs)
	}
	if len(got.Phases[1].Routings) != 1 {
		t.Fatalf("routings = %+v", got.Phases[1].Routings)
	}
}

func TestWindowOrderTiesAndMissing(t *testing.T) {
	v := url.Values{
		"p0.name": {"a"}, "p0.order": {"5"}, "p0.action": {"banner"}, "p0.r0.services": {"s"},
		"p1.name": {"b"}, "p1.order": {"5"}, "p1.action": {"banner"}, "p1.r0.services": {"s"},
		"p2.name": {"c"}, "p2.order": {"1"}, "p2.action": {"banner"}, "p2.r0.services": {"s"},
	}
	got, ps := DecodeWindow(v, time.UTC)
	noProblems(t, ps)
	if n := []string{got.Phases[0].Name, got.Phases[1].Name, got.Phases[2].Name}; strings.Join(n, "") != "cab" {
		t.Fatalf("order = %v", n)
	}
	for _, bad := range []string{"", "x", "-1", "+1", "1.5", " "} {
		v.Set("p2.order", bad)
		got, ps = DecodeWindow(v, time.UTC)
		if !hasField(ps, "p2.order") || got.Phases[2].Name != "c" {
			t.Fatalf("order %q: problems %v, phases %+v", bad, ps, got.Phases)
		}
	}
}

func TestWindowRowProblems(t *testing.T) {
	// A start time without a name keeps the row and says so.
	got, ps := DecodeWindow(url.Values{"p0.order": {"1"}, "p0.startsAt": {"2026-10-30T10:00"}, "p0.action": {"banner"}}, time.UTC)
	if !hasField(ps, "p0.name") || len(got.Phases) != 1 {
		t.Fatalf("problems = %v, phases = %d", ps, len(got.Phases))
	}
	// Action.
	for _, a := range []string{"", "BANNER", "pause"} {
		_, ps = DecodeWindow(url.Values{"p0.name": {"a"}, "p0.order": {"1"}, "p0.action": {a}, "p0.r0.services": {"s"}}, time.UTC)
		if !hasField(ps, "p0.action") {
			t.Fatalf("action %q: problems = %v", a, ps)
		}
	}
	// Indices in problems follow the decoded rows: the blank p0 is gone.
	_, ps = DecodeWindow(url.Values{
		"p0.order": {"1"}, "p0.action": {"banner"},
		"p7.order": {"2"}, "p7.name": {"a"}, "p7.action": {"nope"}, "p7.r3.services": {"s"},
	}, time.UTC)
	if got := fields(ps); len(got) != 1 || got[0] != "p0.action" {
		t.Fatalf("problems = %v", ps)
	}
	// Window problems and decode problems line up on the decoded spec.
	spec, ps := DecodeWindow(url.Values{
		"p4.order": {"1"}, "p4.name": {"a"}, "p4.action": {"banner"}, "p4.r2.services": {"s", "s"},
	}, time.UTC)
	noProblems(t, ps)
	if vs := snapshot.ValidateWindow(&spec); len(vs) != 1 || vs[0].Field != "p0.r0.services" {
		t.Fatalf("ValidateWindow = %v", vs)
	}
}

func TestWindowCaps(t *testing.T) {
	phase := func(v url.Values, i int) {
		p := fmt.Sprintf("p%d.", i)
		v.Set(p+"order", fmt.Sprint(i))
		v.Set(p+"name", fmt.Sprintf("n%d", i))
		v.Set(p+"action", "banner")
		v.Set(p+"r0.services", "s")
	}
	// 16 phases are fine, 17 are not, and more rows than max+2 are cut.
	v := url.Values{}
	for i := range 16 {
		phase(v, i)
	}
	if _, ps := DecodeWindow(v, time.UTC); len(ps) != 0 {
		t.Fatalf("16 phases: %v", ps)
	}
	phase(v, 16)
	if _, ps := DecodeWindow(v, time.UTC); !hasField(ps, "phases") {
		t.Fatalf("17 phases: %v", ps)
	}
	v = url.Values{}
	for i := range 30 {
		phase(v, i)
	}
	got, ps := DecodeWindow(v, time.UTC)
	if !hasField(ps, "phases") || len(got.Phases) > 18 {
		t.Fatalf("30 phases: %v, kept %d", ps, len(got.Phases))
	}

	v = url.Values{}
	phase(v, 0)
	for j := range 33 {
		v.Set(fmt.Sprintf("p0.r%d.services", j), "s")
	}
	if _, ps := DecodeWindow(v, time.UTC); !hasField(ps, "p0.r0.services") {
		t.Fatalf("33 routings: %v", ps)
	}
	v = url.Values{}
	phase(v, 0)
	for j := range 32 {
		v.Set(fmt.Sprintf("p0.r%d.services", j), "s")
	}
	if _, ps := DecodeWindow(v, time.UTC); len(ps) != 0 {
		t.Fatalf("32 routings: %v", ps)
	}
	v = url.Values{}
	phase(v, 0)
	var svcs []string
	for range 65 {
		svcs = append(svcs, "s")
	}
	v["p0.r0.services"] = svcs
	if _, ps := DecodeWindow(v, time.UTC); !hasField(ps, "p0.r0.services") {
		t.Fatalf("65 services: %v", ps)
	}
	v["p0.r0.services"] = svcs[:64]
	if _, ps := DecodeWindow(v, time.UTC); len(ps) != 0 {
		t.Fatalf("64 services: %v", ps)
	}
	// Phase name, counted in runes.
	v["p0.name"] = []string{strings.Repeat("é", 63)}
	if _, ps := DecodeWindow(v, time.UTC); len(ps) != 0 {
		t.Fatalf("63-rune name: %v", ps)
	}
	v["p0.name"] = []string{strings.Repeat("é", 64)}
	if _, ps := DecodeWindow(v, time.UTC); !hasField(ps, "p0.name") {
		t.Fatalf("64-rune name: %v", ps)
	}
}

func heapDelta(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

func TestHugeIndicesDoNotAllocate(t *testing.T) {
	v := url.Values{
		"p999999999.name": {"x"}, "p999999999.r999999999.services": {"s"}, "p999999999.action": {"banner"}, "p999999999.order": {"1"},
		"l999999999.tag": {"en"}, "sel999999999.key": {"k"}, "sel999999999.value": {"v"},
	}
	if n := heapDelta(func() {
		got, _ := DecodeWindow(v, time.UTC)
		if len(got.Phases) != 1 || len(got.Phases[0].Routings) != 1 {
			t.Errorf("got %+v", got)
		}
		page, _ := DecodePage(v)
		if len(page.Locales) != 1 {
			t.Errorf("got %+v", page)
		}
		svc, _ := DecodeService(v, nil)
		if len(svc.RouteSelector.MatchLabels) != 1 {
			t.Errorf("got %+v", svc)
		}
	}); n > 1<<20 {
		t.Fatalf("allocated %d bytes for a handful of rows", n)
	}
}

func TestWindowEnabled(t *testing.T) {
	for in, want := range map[string]bool{"on": true, "": false, "off": false} {
		got, _ := DecodeWindow(url.Values{"enabled": {in}}, time.UTC)
		if got.Enabled != want {
			t.Errorf("enabled=%q: %v", in, got.Enabled)
		}
	}
	got, _ := DecodeWindow(url.Values{}, time.UTC)
	if got.Enabled {
		t.Error("absent enabled must be false")
	}
	if EncodeWindow(&mmv1.MaintenanceWindowSpec{}, nil).Has("enabled") {
		t.Error("disabled window must not encode enabled")
	}
}

// ---- pages ----

func TestPageRowsAndNormalisation(t *testing.T) {
	v := url.Values{
		"fallbackLocale": {" en "},
		"html":           {"<p>a</p>\r\n<p>b</p>\r<p>c</p>\n"},
		"l10.tag":        {"de"}, "l10.title": {"T"}, "l10.message": {"x\r\ny\rz"},
		"l9.tag": {"en"}, "l9.title": {"U"},
		"l0.tag": {""}, "l0.title": {""}, "l0.message": {""}, // blank
		"resourceVersion": {"1"},
	}
	got, ps := DecodePage(v)
	noProblems(t, ps)
	if got.FallbackLocale != "en" {
		t.Errorf("fallback = %q", got.FallbackLocale)
	}
	if got.HTML != "<p>a</p>\n<p>b</p>\n<p>c</p>\n" {
		t.Errorf("html = %q", got.HTML)
	}
	if len(got.Locales) != 2 || got.Locales["de"].Message != "x\ny\nz" || got.Locales["en"].Title != "U" {
		t.Errorf("locales = %+v", got.Locales)
	}
	if got.Locales == nil {
		t.Error("locales must not be nil")
	}
	// Numeric order decides which duplicate is reported at which row.
	v = url.Values{"l9.tag": {"en"}, "l10.tag": {"en"}}
	_, ps = DecodePage(v)
	if len(ps) != 1 || ps[0].Field != "l1.tag" {
		t.Errorf("duplicate tag: %v", ps)
	}
	// Text without a tag is a problem, not silent loss.
	got, ps = DecodePage(url.Values{"l0.title": {"orphan"}})
	if !hasField(ps, "l0.tag") || len(got.Locales) != 1 {
		t.Errorf("orphan text: %v %+v", ps, got.Locales)
	}
}

func TestPageSizesCountRunes(t *testing.T) {
	check := func(name string, v url.Values, field string, wantProblem bool) {
		t.Helper()
		_, ps := DecodePage(v)
		if got := hasField(ps, field); got != wantProblem {
			t.Errorf("%s: problems = %v, want problem on %s: %v", name, ps, field, wantProblem)
		}
	}
	for _, c := range []struct {
		field string
		limit int
		set   func(string) url.Values
	}{
		{"html", 65536, func(s string) url.Values { return url.Values{"html": {s}} }},
		{"l0.title", 256, func(s string) url.Values { return url.Values{"l0.tag": {"en"}, "l0.title": {s}} }},
		{"l0.message", 4096, func(s string) url.Values { return url.Values{"l0.tag": {"en"}, "l0.message": {s}} }},
	} {
		check(c.field+" at limit", c.set(strings.Repeat("日", c.limit)), c.field, false) // more bytes than runes
		check(c.field+" over limit", c.set(strings.Repeat("日", c.limit+1)), c.field, true)
	}
	// CRLF counts as one rune after normalisation: 32768 CRLFs are 32768 runes.
	check("crlf", url.Values{"html": {strings.Repeat("\r\n", 40000)}}, "html", false)
	check("invalid utf-8", url.Values{"html": {"a\xffb"}}, "html", true)
}

func TestPageCaps(t *testing.T) {
	v := url.Values{}
	for i := range 32 {
		v.Set(fmt.Sprintf("l%d.tag", i), fmt.Sprintf("t%d", i))
	}
	if _, ps := DecodePage(v); len(ps) != 0 {
		t.Fatalf("32 locales: %v", ps)
	}
	v.Set("l32.tag", "t32")
	if _, ps := DecodePage(v); !hasField(ps, "l0.tag") {
		t.Fatalf("33 locales: %v", ps)
	}
	for i := 33; i < 60; i++ {
		v.Set(fmt.Sprintf("l%d.tag", i), fmt.Sprintf("t%d", i))
	}
	got, ps := DecodePage(v)
	if len(ps) == 0 || len(got.Locales) > 34 {
		t.Fatalf("60 locales: %v, kept %d", ps, len(got.Locales))
	}
}

// ---- services ----

func TestServiceRows(t *testing.T) {
	old := &mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{
		MatchLabels:      map[string]string{"gone": "x"},
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "e", Operator: metav1.LabelSelectorOpExists}},
	}}
	got, ps := DecodeService(url.Values{
		"sel10.key": {" b "}, "sel10.value": {"2"},
		"sel9.key": {"a"}, "sel9.value": {"1"},
		"sel0.key": {""}, "sel0.value": {""}, // blank
	}, old)
	noProblems(t, ps)
	if len(got.RouteSelector.MatchLabels) != 2 || got.RouteSelector.MatchLabels["a"] != "1" || got.RouteSelector.MatchLabels["b"] != "2" {
		t.Fatalf("labels = %v", got.RouteSelector.MatchLabels)
	}
	if !equality.Semantic.DeepEqual(got.RouteSelector.MatchExpressions, old.RouteSelector.MatchExpressions) {
		t.Fatalf("expressions = %v", got.RouteSelector.MatchExpressions)
	}
	got.RouteSelector.MatchExpressions[0].Key = "changed"
	if old.RouteSelector.MatchExpressions[0].Key != "e" {
		t.Fatal("expressions must be copied, not shared")
	}

	// Duplicate key, value without a key, create without old.
	_, ps = DecodeService(url.Values{"sel0.key": {"a"}, "sel1.key": {"a"}}, nil)
	if len(ps) != 1 || ps[0].Field != "sel1.key" {
		t.Fatalf("duplicate: %v", ps)
	}
	_, ps = DecodeService(url.Values{"sel0.value": {"v"}}, nil)
	if !hasField(ps, "sel0.key") {
		t.Fatalf("orphan value: %v", ps)
	}
	got, ps = DecodeService(url.Values{}, nil)
	noProblems(t, ps)
	if len(got.RouteSelector.MatchLabels) != 0 {
		t.Fatalf("labels = %v", got.RouteSelector.MatchLabels)
	}
}

func TestServiceCaps(t *testing.T) {
	v := url.Values{}
	for i := range 64 {
		v.Set(fmt.Sprintf("sel%d.key", i), fmt.Sprintf("k%d", i))
	}
	if _, ps := DecodeService(v, nil); len(ps) != 0 {
		t.Fatalf("64 rows: %v", ps)
	}
	v.Set("sel64.key", "k64")
	if _, ps := DecodeService(v, nil); !hasField(ps, "sel0.key") {
		t.Fatalf("65 rows: %v", ps)
	}
	for i := 65; i < 100; i++ {
		v.Set(fmt.Sprintf("sel%d.key", i), fmt.Sprintf("k%d", i))
	}
	got, ps := DecodeService(v, nil)
	if len(ps) == 0 || len(got.RouteSelector.MatchLabels) > 66 {
		t.Fatalf("100 rows: %v, kept %d", ps, len(got.RouteSelector.MatchLabels))
	}
}

// ---- fuzzing ----

func parseFuzz(t *testing.T, raw string) url.Values {
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Skip()
	}
	return v
}

func FuzzDecodeService(f *testing.F) {
	f.Add(EncodeService(&mmv1.MaintenanceServiceSpec{RouteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"a": "b", "c": "d"}}}).Encode())
	f.Add("sel0.key=a&sel0.value=b&sel1.key=a")
	f.Add("sel999999999.key=x&sel10.key=y&sel9.value=z")
	f.Fuzz(func(t *testing.T, raw string) {
		v := parseFuzz(t, raw)
		got, ps := DecodeService(v, nil)
		if len(ps) != 0 {
			return
		}
		again, ps := DecodeService(EncodeService(&got), nil)
		if len(ps) != 0 || !equality.Semantic.DeepEqual(got, again) {
			t.Fatalf("round trip: %+v -> %+v (%v)", got, again, ps)
		}
	})
}

func FuzzDecodePage(f *testing.F) {
	g := newGen(7)
	for range 3 {
		page := g.page()
		f.Add(EncodePage(&page).Encode())
	}
	f.Add("html=%0D%0A%0D&l0.tag=en&l0.message=a%0Db&l1.tag=en")
	f.Add("l999999999.tag=x&l5.title=orphan&fallbackLocale=%20en")
	f.Fuzz(func(t *testing.T, raw string) {
		v := parseFuzz(t, raw)
		got, ps := DecodePage(v)
		if len(ps) != 0 {
			return
		}
		again, ps := DecodePage(EncodePage(&got))
		if len(ps) != 0 || !equality.Semantic.DeepEqual(got, again) {
			t.Fatalf("round trip: %+v -> %+v (%v)", got, again, ps)
		}
	})
}

func FuzzDecodeWindow(f *testing.F) {
	berlin, err := LoadZone("Europe/Berlin")
	if err != nil {
		f.Fatal(err)
	}
	g := newGen(11)
	for range 3 {
		w := g.window(nil)
		f.Add(EncodeWindow(&w, berlin).Encode())
	}
	overlap := mmv1.MaintenanceWindowSpec{EndsAt: mt("2026-10-25T01:30:00Z"), Phases: []mmv1.Phase{{
		Name: "a", Action: mmv1.ActionBlock, StartsAt: mt("2026-10-25T00:30:00Z"), Routings: []mmv1.Routing{{Services: []string{"s"}}},
	}}}
	f.Add(EncodeWindow(&overlap, berlin).Encode())
	f.Add("p0.startsAt=2026-03-29T02:30&p0.name=a&p0.action=block&p0.order=1&p0.r0.services=s")
	f.Add("p999999999.name=x&p3.order=zz&p2.r9.services=a&p2.r10.services=b&endsAt=9999-12-31T23:59")
	f.Fuzz(func(t *testing.T, raw string) {
		v := parseFuzz(t, raw)
		got, ps := DecodeWindow(v, berlin)
		if len(ps) != 0 {
			return
		}
		again, ps := DecodeWindow(EncodeWindow(&got, berlin), berlin)
		if len(ps) != 0 || !equality.Semantic.DeepEqual(got, again) {
			t.Fatalf("round trip: %+v -> %+v (%v)", got, again, ps)
		}
	})
}
