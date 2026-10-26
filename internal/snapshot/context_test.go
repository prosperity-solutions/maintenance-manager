package snapshot

import (
	"testing"
	"time"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/render"
)

func hhmm(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return t.Format("15:04")
}

func phaseText(p render.Phase) string {
	if p.Name == "" {
		return "-"
	}
	return p.Window + "/" + p.Name + ":" + p.Action + "@" + hhmm(p.Start) + "-" + hhmm(p.End)
}

func downtimeText(c render.Context) string {
	if !c.HasDowntime {
		return "-"
	}
	return hhmm(c.Downtime.Start) + "-" + hhmm(c.Downtime.End)
}

func timelineOf(t *testing.T, ws ...mmv1.MaintenanceWindow) []*Decision {
	t.Helper()
	snap := Build(Input{Windows: ws, Services: services("shop", "api"), Routes: []RouteInfo{shopRoute}, Now: t0})
	r := snap.Routes[shopRoute.Key]
	if r == nil {
		t.Fatal("no timeline")
	}
	return r.Timeline
}

func TestContextBannerThenBlock(t *testing.T) {
	tl := timelineOf(t, window("db", true, at(22, 30),
		phase("warn", at(16, 0), mmv1.ActionBanner, "shop"),
		phase("down", at(20, 0), mmv1.ActionBlock, "shop"),
	))
	warn, down := tl[0].Context, tl[1].Context
	if got := phaseText(warn.Next); got != "db/down:block@20:00-22:30" {
		t.Errorf("warn.Next %s", got)
	}
	if got := downtimeText(warn); got != "20:00-22:30" {
		t.Errorf("warn downtime %s", got)
	}
	if warn.Window.Name != "db" || hhmm(warn.Window.Start) != "16:00" || hhmm(warn.Window.End) != "22:30" || warn.Window.Phases != 2 {
		t.Errorf("window %+v", warn.Window)
	}
	if warn.Phase.Number != 1 || down.Phase.Number != 2 || warn.Previous.Name != "" {
		t.Errorf("numbers %d %d, previous %+v", warn.Phase.Number, down.Phase.Number, warn.Previous)
	}
	if got := phaseText(down.Previous); got != "db/warn:banner@16:00-20:00" {
		t.Errorf("down.Previous %s", got)
	}
	if down.Next.Name != "" || downtimeText(down) != "20:00-22:30" {
		t.Errorf("down: next %+v downtime %s", down.Next, downtimeText(down))
	}
	if warn.ContinueAfter != 0 || down.ContinueAfter != 0 {
		t.Errorf("continueAfter on banner/block: %d %d", warn.ContinueAfter, down.ContinueAfter)
	}
}

func TestContextPendingNext(t *testing.T) {
	// The next phase has no start yet: it is named, its start unknown, and so
	// is the downtime's.
	tl := timelineOf(t, window("db", true, nil,
		phase("warn", nil, mmv1.ActionBanner, "shop"),
		phase("down", nil, mmv1.ActionBlock, "shop"),
	))
	c := tl[0].Context
	if got := phaseText(c.Next); got != "db/down:block@?-?" {
		t.Errorf("next %s", got)
	}
	if c.HasDowntime {
		t.Errorf("a pending block is not on the timeline yet: %s", downtimeText(c))
	}
	if !c.Phase.Start.IsZero() || !c.Window.Start.IsZero() {
		t.Errorf("an undated first phase has no start: %+v", c.Phase)
	}
}

func TestContextNextFromTheRoute(t *testing.T) {
	// "db" blocks only api at 20:00; the shop route goes from warn straight
	// to the end, so its next phase and downtime are empty.
	w := window("db", true, at(22, 0),
		phase("warn", at(16, 0), mmv1.ActionBanner, "shop", "api"),
		phase("down", at(20, 0), mmv1.ActionBlock, "api"),
	)
	tl := timelineOf(t, w)
	if len(tl) != 1 {
		t.Fatalf("timeline %d", len(tl))
	}
	if c := tl[0].Context; c.Next.Name != "" || c.HasDowntime {
		t.Errorf("shop does not get api's block: next %+v downtime %s", c.Next, downtimeText(c))
	}
}

func TestContextOverlappingWindows(t *testing.T) {
	// "b" warns 16:00-22:00; "a" blocks 20:00-21:00. The banner's next phase
	// and downtime are a's block, the block's next phase is b's banner again.
	a := window("a", true, at(21, 0), phase("down", at(20, 0), mmv1.ActionBlock, "shop"))
	b := window("b", true, at(22, 0), phase("warn", at(16, 0), mmv1.ActionBanner, "shop"))
	tl := timelineOf(t, a, b)
	if len(tl) != 3 {
		t.Fatalf("timeline %d", len(tl))
	}
	if got := phaseText(tl[0].Context.Next); got != "a/down:block@20:00-21:00" {
		t.Errorf("banner.Next %s", got)
	}
	if got := downtimeText(tl[0].Context); got != "20:00-21:00" {
		t.Errorf("banner downtime %s", got)
	}
	if got := phaseText(tl[1].Context.Next); got != "b/warn:banner@21:00-22:00" {
		t.Errorf("block.Next %s", got)
	}
	if tl[2].Context.HasDowntime {
		t.Error("no block after 21:00")
	}
}

func TestContextBlockRunAcrossPhases(t *testing.T) {
	tl := timelineOf(t, window("db", true, at(23, 0),
		phase("warn", at(16, 0), mmv1.ActionBanner, "shop"),
		phase("down1", at(20, 0), mmv1.ActionBlock, "shop"),
		phase("down2", at(21, 0), mmv1.ActionBlock, "shop"),
	))
	for i, d := range tl {
		if got := downtimeText(d.Context); got != "20:00-23:00" {
			t.Errorf("phase %d downtime %s", i, got)
		}
	}
}

func TestContextAutoContinueAndSharing(t *testing.T) {
	secs := func(n int32) *mmv1.InterstitialOptions { return &mmv1.InterstitialOptions{AutoContinueSeconds: &n} }
	inter := func(name string, start int, o *mmv1.InterstitialOptions) mmv1.Phase {
		p := phase(name, at(start, 0), mmv1.ActionInterstitial, "shop")
		p.Interstitial = o
		return p
	}
	tl := timelineOf(t, window("db", true, at(23, 0),
		inter("a", 16, nil), inter("b", 18, secs(0)), inter("c", 20, secs(30)),
	))
	for i, want := range []int{8, 0, 30} {
		if got := tl[i].Context.ContinueAfter; got != want {
			t.Errorf("phase %d: continueAfter %d, want %d", i, got, want)
		}
	}

	// Two routes with the same banner but different blocks after it must not
	// share one decision: the page shows the downtime.
	api := RouteInfo{Key: RouteKey{Namespace: "api", Name: "web"}, Labels: map[string]string{"app": "api"}}
	w := window("db", true, at(22, 0),
		phase("warn", at(16, 0), mmv1.ActionBanner, "shop", "api"),
		phase("down", at(20, 0), mmv1.ActionBlock, "api"),
	)
	snap := Build(Input{Windows: []mmv1.MaintenanceWindow{w}, Services: services("shop", "api"), Routes: []RouteInfo{shopRoute, api}, Now: t0})
	if snap.Routes[shopRoute.Key].Timeline[0] == snap.Routes[api.Key].Timeline[0] {
		t.Fatal("the banners share a decision although only api's leads to a block")
	}
}

func TestContextEdges(t *testing.T) {
	// (a) A gap after the banner: no next phase, but the later block is the
	// downtime.
	tl := timelineOf(t,
		window("w1", true, at(18, 0), phase("warn", at(16, 0), mmv1.ActionBanner, "shop")),
		window("w2", true, at(21, 0), phase("down", at(20, 0), mmv1.ActionBlock, "shop")),
	)
	if c := tl[0].Context; c.Next.Name != "" || downtimeText(c) != "20:00-21:00" {
		t.Errorf("gap: next %+v downtime %s", c.Next, downtimeText(c))
	}
	if c := tl[1].Context; c.Previous.Name != "" {
		t.Errorf("gap: the block's previous phase is %+v; nothing ran right before it", c.Previous)
	}

	// (b) The window's next phase is pending, but another window starts right
	// where this one ends: the route's real next phase wins.
	tl = timelineOf(t,
		window("w1", true, at(20, 0), phase("warn", at(16, 0), mmv1.ActionBanner, "shop"), phase("later", nil, mmv1.ActionBlock, "shop")),
		window("w2", true, at(21, 0), phase("down", at(20, 0), mmv1.ActionBlock, "shop")),
	)
	if got := phaseText(tl[0].Context.Next); got != "w2/down:block@20:00-21:00" {
		t.Errorf("adjacent beats pending: next %s", got)
	}

	// (c) A block run across two windows: the second block's downtime starts
	// at the first; its previous phase is the first block.
	tl = timelineOf(t,
		window("w1", true, at(20, 0), phase("down1", at(19, 0), mmv1.ActionBlock, "shop")),
		window("w2", true, at(21, 0), phase("down2", at(20, 0), mmv1.ActionBlock, "shop")),
	)
	if got := downtimeText(tl[1].Context); got != "19:00-21:00" {
		t.Errorf("run across windows: downtime %s", got)
	}
	if got := phaseText(tl[1].Context.Previous); got != "w1/down1:block@19:00-20:00" {
		t.Errorf("run across windows: previous %s", got)
	}
}

func TestContextPreviousFromTheRoute(t *testing.T) {
	// Phase 1 blocks only api; for shop, the phase after warn follows
	// straight on warn, not on api's block.
	tl := timelineOf(t, window("db", true, at(22, 0),
		phase("warn", at(16, 0), mmv1.ActionBanner, "shop", "api"),
		phase("down", at(18, 0), mmv1.ActionBlock, "api"),
		phase("again", at(20, 0), mmv1.ActionBanner, "shop", "api"),
	))
	if len(tl) != 2 {
		t.Fatalf("timeline %d", len(tl))
	}
	if c := tl[1].Context; c.Previous.Name != "" || c.Phase.Number != 3 {
		t.Errorf("shop had a gap from 18:00 to 20:00, so no previous phase: %+v (number %d)", c.Previous, c.Phase.Number)
	}
}

func TestContextIdenticalRoutesShare(t *testing.T) {
	// Three routes of the same service, a banner then a block: every
	// decision is shared, so memory does not grow with the routes.
	w := window("db", true, at(22, 0),
		phase("warn", at(16, 0), mmv1.ActionBanner, "shop"),
		phase("down", at(20, 0), mmv1.ActionBlock, "shop"),
	)
	var routes []RouteInfo
	for _, n := range []string{"a", "b", "c"} {
		routes = append(routes, RouteInfo{Key: RouteKey{Namespace: "shop", Name: n}, Labels: map[string]string{"app": "shop"}})
	}
	snap := Build(Input{Windows: []mmv1.MaintenanceWindow{w}, Services: services("shop"), Routes: routes, Now: t0})
	first := snap.Routes[routes[0].Key].Timeline
	for _, r := range routes[1:] {
		for i, d := range snap.Routes[r.Key].Timeline {
			if d != first[i] {
				t.Fatalf("%s decision %d is not shared", r.Key.Name, i)
			}
		}
	}
}
