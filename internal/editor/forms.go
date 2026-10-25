package editor

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mmv1 "github.com/prosperity-solutions/maintenance-manager/internal/api/v1alpha1"
	"github.com/prosperity-solutions/maintenance-manager/internal/snapshot"
)

// Limits of the CRD schema (§4), checked here in runes; maxSelectors is the
// editor's own (the CRD caps matchLabels only by being non-empty).
const (
	maxHTML        = 65536
	maxTitle       = 256
	maxMessage     = 4096
	maxContinue    = 64
	maxPhaseName   = 63
	maxPhases      = 16
	maxRoutings    = 32
	maxServices    = 64
	maxLocales     = 32
	maxSelectors   = 64
	blankRows      = 2 // the form renders two extra blank rows per list
	maxIndexDigits = 9 // row indices longer than this are not ours
)

// problems collects validation failures in the spelling of snapshot.Problem.
type problems []snapshot.Problem

func (p *problems) add(field, format string, a ...any) {
	*p = append(*p, snapshot.Problem{Field: field, Message: fmt.Sprintf(format, a...)})
}

// text checks a string for valid UTF-8 and, when limit > 0, its length in runes.
func (p *problems) text(field, s string, limit int) {
	if !utf8.ValidString(s) {
		p.add(field, "must be valid UTF-8 text")
		return
	}
	if limit > 0 && utf8.RuneCountInString(s) > limit {
		p.add(field, "is too long: %d characters, at most %d", utf8.RuneCountInString(s), limit)
	}
}

// cutIndex splits a field name like "p12.name" into its row index and the rest
// ("name") for the given prefix ("p"). Indices are plain decimal numbers.
func cutIndex(key, prefix string) (idx int, rest string, ok bool) {
	s, found := strings.CutPrefix(key, prefix)
	if !found {
		return 0, "", false
	}
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	if n == 0 || n > maxIndexDigits || n == len(s) || s[n] != '.' || n+1 == len(s) {
		return 0, "", false
	}
	idx, err := strconv.Atoi(s[:n])
	if err != nil {
		return 0, "", false
	}
	return idx, s[n+1:], true
}

// rowAt returns the row for index i, creating it. Memory depends on the number
// of rows, never on the index value.
func rowAt[T any](rows map[int]*T, i int) *T {
	r := rows[i]
	if r == nil {
		r = new(T)
		rows[i] = r
	}
	return r
}

// sortedRows returns the row indices in numeric order, capped at limit+2 (the
// two blank rows the form adds) with a problem on field when there are more.
func sortedRows[T any](p *problems, rows map[int]*T, field string, limit int) []int {
	idx := slices.Sorted(maps.Keys(rows))
	if len(idx) > limit+blankRows {
		p.add(field, "too many rows: at most %d", limit)
		idx = idx[:limit+blankRows]
	}
	return idx
}

// byKey returns the rows at idx, stably sorted by key.
func byKey[T any](idx []int, rows map[int]*T, key func(*T) string) []*T {
	out := make([]*T, 0, len(idx))
	for _, i := range idx {
		out = append(out, rows[i])
	}
	slices.SortStableFunc(out, func(a, b *T) int { return strings.Compare(key(a), key(b)) })
	return out
}

// first returns the first submitted value of a field, trimmed.
func first(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	return strings.TrimSpace(vals[0])
}

// firstRaw returns the first submitted value of a field as is.
func firstRaw(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// lf converts CRLF and lone CR line breaks of a textarea to LF.
func lf(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// ---- services ----

// EncodeService turns a service spec into form values: one row per label,
// sorted by key. matchExpressions are not editable and not encoded.
func EncodeService(spec *mmv1.MaintenanceServiceSpec) url.Values {
	v := url.Values{}
	if spec == nil {
		return v
	}
	for k, key := range slices.Sorted(maps.Keys(spec.RouteSelector.MatchLabels)) {
		v.Set("sel"+strconv.Itoa(k)+".key", key)
		v.Set("sel"+strconv.Itoa(k)+".value", spec.RouteSelector.MatchLabels[key])
	}
	return v
}

// DecodeService reads the selector rows into matchLabels. matchExpressions are
// not editable here and are copied from old, which may be nil.
func DecodeService(v url.Values, old *mmv1.MaintenanceServiceSpec) (mmv1.MaintenanceServiceSpec, []snapshot.Problem) {
	var p problems
	type row struct{ key, value string }
	rows := map[int]*row{}
	for name, vals := range v {
		i, rest, ok := cutIndex(name, "sel")
		if !ok {
			continue
		}
		switch rest {
		case "key":
			rowAt(rows, i).key = first(vals)
		case "value":
			rowAt(rows, i).value = first(vals)
		}
	}
	var spec mmv1.MaintenanceServiceSpec
	if old != nil {
		spec.RouteSelector.MatchExpressions = old.RouteSelector.DeepCopy().MatchExpressions
	}
	labels := map[string]string{}
	n := 0
	// Problems are numbered in the order the form shows the rows again: by
	// key, as EncodeService sorts them.
	for _, r := range byKey(sortedRows(&p, rows, "sel0.key", maxSelectors), rows, func(r *row) string { return r.key }) {
		if r.key == "" && r.value == "" {
			continue
		}
		// A repeated key folds into one row on the form, so its problem goes
		// to that row, not to a blank one after it.
		_, dup := labels[r.key]
		if dup {
			n--
		}
		f := "sel" + strconv.Itoa(n) + ".key"
		n++
		if r.key == "" {
			p.add(f, "row has a value but no key")
		}
		if dup && r.key != "" {
			p.add(f, "label key %q is used twice", r.key)
		}
		p.text(f, r.key, 0)
		p.text(f, r.value, 0)
		labels[r.key] = r.value
	}
	if n > maxSelectors {
		p.add("sel0.key", "too many rows: at most %d", maxSelectors)
	}
	if len(labels) > 0 {
		spec.RouteSelector.MatchLabels = labels
	}
	return spec, p
}

// ---- pages ----

// EncodePage turns a page spec into form values; locales are sorted by tag.
func EncodePage(spec *mmv1.MaintenancePageSpec) url.Values {
	v := url.Values{}
	if spec == nil {
		return v
	}
	v.Set("fallbackLocale", spec.FallbackLocale)
	v.Set("html", spec.HTML)
	for k, tag := range slices.Sorted(maps.Keys(spec.Locales)) {
		pre := "l" + strconv.Itoa(k) + "."
		v.Set(pre+"tag", tag)
		v.Set(pre+"title", spec.Locales[tag].Title)
		v.Set(pre+"message", spec.Locales[tag].Message)
		v.Set(pre+"continue", spec.Locales[tag].ContinueLabel)
	}
	return v
}

// DecodePage reads a page form. Blank locale rows are dropped.
func DecodePage(v url.Values) (mmv1.MaintenancePageSpec, []snapshot.Problem) {
	var p problems
	type row struct{ tag, title, message, cont string }
	rows := map[int]*row{}
	for name, vals := range v {
		i, rest, ok := cutIndex(name, "l")
		if !ok {
			continue
		}
		switch rest {
		case "tag":
			rowAt(rows, i).tag = first(vals)
		case "title":
			rowAt(rows, i).title = lf(firstRaw(vals))
		case "message":
			rowAt(rows, i).message = lf(firstRaw(vals))
		case "continue":
			rowAt(rows, i).cont = firstRaw(vals)
		}
	}
	spec := mmv1.MaintenancePageSpec{
		FallbackLocale: first(v["fallbackLocale"]),
		HTML:           lf(firstRaw(v["html"])),
		Locales:        map[string]mmv1.PageText{},
	}
	p.text("html", spec.HTML, maxHTML)
	p.text("fallbackLocale", spec.FallbackLocale, 0)
	n := 0
	// Problems are numbered in the order the form shows the rows again: by
	// tag, as EncodePage sorts them.
	for _, r := range byKey(sortedRows(&p, rows, "l0.tag", maxLocales), rows, func(r *row) string { return r.tag }) {
		if r.tag == "" && r.title == "" && r.message == "" && r.cont == "" {
			continue
		}
		// A repeated tag folds into one row on the form, so its problem goes
		// to that row, not to a blank one after it.
		_, dup := spec.Locales[r.tag]
		if dup {
			n--
		}
		pre := "l" + strconv.Itoa(n) + "."
		n++
		if r.tag == "" {
			p.add(pre+"tag", "row has a text but no tag")
		} else if dup {
			p.add(pre+"tag", "locale %q is used twice", r.tag)
		}
		p.text(pre+"tag", r.tag, 0)
		p.text(pre+"title", r.title, maxTitle)
		p.text(pre+"message", r.message, maxMessage)
		p.text(pre+"continue", r.cont, maxContinue)
		if strings.ContainsAny(r.cont, "\r\n") {
			p.add(pre+"continue", "must be one line")
		} else if r.cont != "" && strings.TrimSpace(r.cont) == "" {
			p.add(pre+"continue", "must not be only spaces: leave it empty for the built-in text")
		}
		spec.Locales[r.tag] = mmv1.PageText{Title: r.title, Message: r.message, ContinueLabel: r.cont}
	}
	if n > maxLocales {
		p.add("l0.tag", "too many locales: at most %d", maxLocales)
	}
	return spec, p
}

// ---- windows ----

// EncodeWindow turns a window spec into form values. Phases are numbered
// 1..n in `order`; times are shown in loc and carry the stored instant in a
// hidden "...Was" field, so an untouched save keeps it exactly.
func EncodeWindow(spec *mmv1.MaintenanceWindowSpec, loc *time.Location) url.Values {
	v := url.Values{}
	if spec == nil {
		return v
	}
	if spec.Enabled {
		v.Set("enabled", "on")
	}
	encodeTime(v, "endsAt", spec.EndsAt, loc)
	for i, ph := range spec.Phases {
		pre := "p" + strconv.Itoa(i) + "."
		v.Set(pre+"order", strconv.Itoa(i+1))
		v.Set(pre+"name", ph.Name)
		encodeTime(v, pre+"startsAt", ph.StartsAt, loc)
		v.Set(pre+"action", string(ph.Action))
		for j, r := range ph.Routings {
			rp := pre + "r" + strconv.Itoa(j) + "."
			if len(r.Services) > 0 {
				v[rp+"services"] = slices.Clone(r.Services)
			}
			v.Set(rp+"page", r.Page)
		}
	}
	return v
}

func encodeTime(v url.Values, field string, t *metav1.Time, loc *time.Location) {
	if t == nil || t.IsZero() {
		v.Set(field, "")
		v.Set(field+"Was", "")
		return
	}
	v.Set(field, FormatLocal(t.Time, loc))
	v.Set(field+"Was", t.UTC().Format(time.RFC3339))
}

// decodeTime reads a time field. A value equal to the stored instant shown in
// loc keeps that instant exactly; anything else is parsed in loc.
func (p *problems) decodeTime(field, value, was string, loc *time.Location) *metav1.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var t time.Time
	if stored, err := time.Parse(time.RFC3339, strings.TrimSpace(was)); err == nil && !stored.IsZero() {
		if wall, err := wallClock(value); err == nil && FormatLocal(stored, loc) == wall.Format(localLayout) {
			t = stored
		}
	}
	if t.IsZero() {
		var err error
		if t, err = ParseLocal(value, loc); err != nil {
			p.add(field, "%v", err)
			return nil
		}
	}
	t = t.UTC().Truncate(time.Second)
	if t.IsZero() || t.Year() < 1 || t.Year() > 9999 {
		p.add(field, "the time is out of range")
		return nil
	}
	return &metav1.Time{Time: t}
}

type routingRow struct {
	services []string
	page     string
}

type phaseRow struct {
	order, name, startsAt, startsAtWas, action string
	routings                                   map[int]*routingRow
	hasServices, hasPage                       bool
}

// DecodeWindow reads a window form, interpreting times in loc. Phases are
// sorted by `order`, blank rows are dropped, and problems are addressed by the
// resulting indices (what ValidateWindow reports on the decoded spec).
func DecodeWindow(v url.Values, loc *time.Location) (mmv1.MaintenanceWindowSpec, []snapshot.Problem) {
	spec, ps, _ := decodeWindow(v, loc)
	return spec, ps
}

// decodeWindow is DecodeWindow that also returns, per decoded phase, the
// submitted row index it came from, so a form shown again with errors can
// put back what was typed into a field that did not decode.
func decodeWindow(v url.Values, loc *time.Location) (mmv1.MaintenanceWindowSpec, []snapshot.Problem, []int) {
	var p problems
	if loc == nil {
		loc = time.UTC
	}
	spec := mmv1.MaintenanceWindowSpec{Phases: []mmv1.Phase{}}
	switch strings.ToLower(first(v["enabled"])) {
	case "on", "true", "1":
		spec.Enabled = true
	}
	spec.EndsAt = p.decodeTime("endsAt", firstRaw(v["endsAt"]), firstRaw(v["endsAtWas"]), loc)

	rows := map[int]*phaseRow{}
	for name, vals := range v {
		i, rest, ok := cutIndex(name, "p")
		if !ok {
			continue
		}
		switch rest {
		case "order":
			rowAt(rows, i).order = first(vals)
		case "name":
			rowAt(rows, i).name = first(vals)
		case "startsAt":
			rowAt(rows, i).startsAt = first(vals)
		case "startsAtWas":
			rowAt(rows, i).startsAtWas = first(vals)
		case "action":
			rowAt(rows, i).action = first(vals)
		default:
			j, field, ok := cutIndex(rest, "r")
			if !ok {
				continue
			}
			ph := rowAt(rows, i)
			if ph.routings == nil {
				ph.routings = map[int]*routingRow{}
			}
			switch field {
			case "services":
				var svcs []string
				for _, s := range vals {
					if s = strings.TrimSpace(s); s != "" {
						svcs = append(svcs, s)
					}
				}
				rowAt(ph.routings, j).services = svcs
				ph.hasServices = ph.hasServices || len(svcs) > 0
			case "page":
				rowAt(ph.routings, j).page = first(vals)
				ph.hasPage = ph.hasPage || first(vals) != ""
			}
		}
	}

	type keptPhase struct {
		row   *phaseRow
		idx   int
		order int
		valid bool
	}
	var kept []keptPhase
	for _, i := range sortedRows(&p, rows, "phases", maxPhases) {
		r := rows[i]
		if r.name == "" && !r.hasServices && r.startsAt == "" && !r.hasPage {
			continue // the order is prefilled on blank rows, so it does not count
		}
		k := keptPhase{row: r, idx: i}
		if n, ok := parseOrder(r.order); ok {
			k.order, k.valid = n, true
		}
		kept = append(kept, k)
	}
	sort.SliceStable(kept, func(a, b int) bool {
		if kept[a].valid != kept[b].valid {
			return kept[a].valid // phases without a usable order go last
		}
		return kept[a].order < kept[b].order
	})
	if len(kept) > maxPhases {
		p.add("phases", "too many phases: at most %d", maxPhases)
	}

	var from []int
	for i, k := range kept {
		r := k.row
		from = append(from, k.idx)
		f := "p" + strconv.Itoa(i)
		if !k.valid {
			p.add(f+".order", "the order must be a whole number")
		}
		if r.name == "" && !r.hasServices {
			p.add(f+".name", "row has a start time or page but no name")
		}
		p.text(f+".name", r.name, maxPhaseName)
		switch a := mmv1.Action(r.action); a {
		case mmv1.ActionBanner, mmv1.ActionInterstitial, mmv1.ActionBlock:
		default:
			p.add(f+".action", "action must be banner, interstitial or block")
		}
		ph := mmv1.Phase{
			Name:     r.name,
			StartsAt: p.decodeTime(f+".startsAt", r.startsAt, r.startsAtWas, loc),
			Action:   mmv1.Action(r.action),
			Routings: []mmv1.Routing{},
		}
		n := 0
		for _, j := range sortedRows(&p, r.routings, f+".r0.services", maxRoutings) {
			rr := r.routings[j]
			if len(rr.services) == 0 {
				if rr.page == "" {
					continue
				}
				// Kept (the save is refused anyway) so the form shows it again.
				p.add(f+".r"+strconv.Itoa(n)+".services", "a routing with a page needs at least one service")
			}
			rf := f + ".r" + strconv.Itoa(n) + "."
			n++
			if len(rr.services) > maxServices {
				p.add(rf+"services", "too many services: at most %d", maxServices)
			}
			for _, s := range rr.services {
				p.text(rf+"services", s, 0)
			}
			p.text(rf+"page", rr.page, 0)
			ph.Routings = append(ph.Routings, mmv1.Routing{Services: rr.services, Page: rr.page})
		}
		if n > maxRoutings {
			p.add(f+".r0.services", "too many routings: at most %d", maxRoutings)
		}
		spec.Phases = append(spec.Phases, ph)
	}
	return spec, p, from
}

// parseOrder reads a phase order: a plain non-negative whole number.
func parseOrder(s string) (int, bool) {
	if s == "" || len(s) > maxIndexDigits {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}
