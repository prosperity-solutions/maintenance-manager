package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"time"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
)

// Hash8 is the first 8 lowercase hex characters of sha256(s).
func Hash8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// PhaseKey identifies a phase for the interstitial cookie:
// hash8(phase.name + startsAt), startsAt as RFC3339 UTC or "" when undated.
func PhaseKey(ph mmv1.Phase) string {
	start := ""
	if ph.StartsAt != nil {
		start = ph.StartsAt.UTC().Format(time.RFC3339)
	}
	return Hash8(ph.Name + start)
}

// CookieName is the interstitial cookie of a window: mm_seen_<hash8(UID)>.
func CookieName(uid string) string { return "mm_seen_" + Hash8(uid) }

// Segment is the time a phase is the window's active phase: [From, To).
// A zero From means "since the window was enabled" (an undated phase 0); a
// zero To means "until the window is disabled".
type Segment struct {
	From, To time.Time
	Phase    int
}

// Contains reports whether t falls in [From, To).
func (s Segment) Contains(t time.Time) bool {
	return (s.From.IsZero() || !t.Before(s.From)) && (s.To.IsZero() || t.Before(s.To))
}

// Segments turns a valid window into its active-phase intervals (§4.4):
// the active phase is the last phase that has started; a pending phase never
// starts by itself, so the phase before it runs until endsAt or forever.
// Phases that start at or after endsAt never become active.
func Segments(spec *mmv1.MaintenanceWindowSpec) []Segment {
	var segs []Segment
	var end time.Time
	if spec.EndsAt != nil {
		end = spec.EndsAt.UTC()
	}
	for i, ph := range spec.Phases {
		var from time.Time
		switch {
		case ph.StartsAt != nil:
			from = ph.StartsAt.UTC()
		case i == 0:
			// starts when the window is enabled
		default:
			return segs // pending: it and every later phase never start
		}
		if !end.IsZero() && !from.IsZero() && !from.Before(end) {
			return segs
		}
		if n := len(segs); n > 0 {
			segs[n-1].To = from
		}
		segs = append(segs, Segment{From: from, To: end, Phase: i})
	}
	return segs
}

// Ended reports whether the window's endsAt has passed.
func Ended(spec *mmv1.MaintenanceWindowSpec, now time.Time) bool {
	return spec.EndsAt != nil && !now.Before(spec.EndsAt.Time)
}

// ActivePhase returns the index of the phase active at now, or -1.
func ActivePhase(spec *mmv1.MaintenanceWindowSpec, now time.Time) int {
	if !spec.Enabled || Ended(spec, now) {
		return -1
	}
	for _, s := range Segments(spec) {
		if s.Contains(now) {
			return s.Phase
		}
	}
	return -1
}

// Armed reports whether Envoy must ask maintenance-manager about the window's
// routes (§4.4 rule 7): enabled, not ended, valid, and a phase is active or
// the next dated start is within lead. The caller checks validity.
func Armed(spec *mmv1.MaintenanceWindowSpec, now time.Time, lead time.Duration) bool {
	if !spec.Enabled || Ended(spec, now) {
		return false
	}
	for _, s := range Segments(spec) {
		if s.Contains(now) || (!s.From.IsZero() && !now.Before(s.From.Add(-lead)) && (s.To.IsZero() || now.Before(s.To))) {
			return true
		}
	}
	return false
}

// NextBoundary is the next instant after now at which Armed or ActivePhase
// can change: a phase start minus lead, a phase start, or endsAt. Zero if
// there is none. The controller wakes then (RequeueAfter).
func NextBoundary(spec *mmv1.MaintenanceWindowSpec, now time.Time, lead time.Duration) time.Time {
	var next time.Time
	consider := func(t time.Time) {
		if t.After(now) && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	if !spec.Enabled {
		return time.Time{}
	}
	for _, s := range Segments(spec) {
		if !s.From.IsZero() {
			consider(s.From.Add(-lead))
			consider(s.From)
		}
	}
	if spec.EndsAt != nil {
		consider(spec.EndsAt.UTC())
	}
	return next
}

// candidate is one window's phase applying to one route over a segment.
type candidate struct {
	seg     Segment
	window  string
	uid     string
	spec    *mmv1.MaintenanceWindowSpec
	page    string // routing's page name ("" = default)
	service string // the service that put the route in this routing
}

var severity = map[mmv1.Action]int{mmv1.ActionBanner: 1, mmv1.ActionInterstitial: 2, mmv1.ActionBlock: 3}

// beats reports whether a wins over b on overlap: the more severe action,
// then the lexicographically smallest window name (§4.4 rule 10).
func beats(a, b *candidate) bool {
	sa, sb := severity[a.spec.Phases[a.seg.Phase].Action], severity[b.spec.Phases[b.seg.Phase].Action]
	if sa != sb {
		return sa > sb
	}
	return a.window < b.window
}

// interval is a piece of a route's merged timeline before decisions are
// filled in.
type interval struct {
	from, to time.Time // zero from = -inf, zero to = +inf
	winner   *candidate
}

// merge resolves overlapping candidates into a timeline of non-overlapping
// intervals, each won by one candidate, adjacent equal winners joined.
func merge(cands []*candidate) []interval {
	if len(cands) == 0 {
		return nil
	}
	// Elementary boundaries; -inf and +inf are implicit.
	var cuts []time.Time
	for _, c := range cands {
		if !c.seg.From.IsZero() {
			cuts = append(cuts, c.seg.From)
		}
		if !c.seg.To.IsZero() {
			cuts = append(cuts, c.seg.To)
		}
	}
	slices.SortFunc(cuts, func(a, b time.Time) int { return a.Compare(b) })
	cuts = slices.CompactFunc(cuts, func(a, b time.Time) bool { return a.Equal(b) })

	// Pieces: (-inf, c0), [c0, c1), ..., [cn, +inf).
	type piece struct{ from, to time.Time }
	pieces := make([]piece, 0, len(cuts)+1)
	prev := time.Time{}
	for _, c := range cuts {
		pieces = append(pieces, piece{prev, c})
		prev = c
	}
	pieces = append(pieces, piece{prev, time.Time{}})

	var out []interval
	for _, p := range pieces {
		var win *candidate
		for _, c := range cands {
			if !covers(c.seg, p.from) {
				continue
			}
			if win == nil || beats(c, win) {
				win = c
			}
		}
		if win == nil {
			continue
		}
		if n := len(out); n > 0 && out[n-1].winner == win && out[n-1].to.Equal(p.from) {
			out[n-1].to = p.to
			continue
		}
		out = append(out, interval{from: p.from, to: p.to, winner: win})
	}
	return out
}

// covers reports whether segment s covers the whole piece [from, to).
// Pieces never straddle a segment boundary, so checking the start suffices;
// the piece before the first cut (zero from) is covered exactly by the
// segments open to the past.
func covers(s Segment, from time.Time) bool {
	if from.IsZero() {
		return s.From.IsZero()
	}
	return s.Contains(from)
}
