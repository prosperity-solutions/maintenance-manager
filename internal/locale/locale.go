// Package locale negotiates a page locale from ?locale= and Accept-Language
// (docs/HANDOVER.md §8). Resolve is on the hot path (§6): no allocation, no
// locks, no I/O. A Matcher is immutable after NewMatcher.
package locale

import "sort"

const (
	// maxCandidates caps the parsed candidates (query included).
	maxCandidates = 8
	// maxTagLen is the longest tag we normalize; longer tags cannot match.
	maxTagLen = 64
)

// Matcher resolves requested language tags to one of a page's locales.
type Matcher struct {
	tags     map[string]string // normalized tag -> page locale
	locales  []string          // sorted, unique
	fallback string
}

// normalizeByte lowercases ASCII letters and maps '_' to '-'.
func normalizeByte(c byte) byte {
	switch {
	case c >= 'A' && c <= 'Z':
		return c + 'a' - 'A'
	case c == '_':
		return '-'
	}
	return c
}

func normalize(s string) string {
	b := []byte(s)
	for i := range b {
		b[i] = normalizeByte(b[i])
	}
	return string(b)
}

func baseOf(tag string) string {
	for i := 0; i < len(tag); i++ {
		if tag[i] == '-' {
			return tag[:i]
		}
	}
	return tag
}

// NewMatcher builds a matcher for a page's locale keys and its fallback. A
// fallback that is not one of locales is replaced by the first locale in
// sorted order.
func NewMatcher(locales []string, fallback string) *Matcher {
	sorted := append([]string(nil), locales...)
	sort.Strings(sorted)
	uniq := sorted[:0]
	for i, l := range sorted {
		if i == 0 || l != sorted[i-1] {
			uniq = append(uniq, l)
		}
	}
	sorted = uniq

	m := &Matcher{tags: make(map[string]string, 2*len(sorted)), locales: sorted}

	// Rule 1: exact. The first in sorted order wins a normalized collision.
	for _, l := range sorted {
		n := normalize(l)
		if _, ok := m.tags[n]; !ok {
			m.tags[n] = l
		}
	}
	// Rule 3: a bare base language maps to the first locale with that base.
	// Rule 2 (region tag to its base) is a second lookup in Resolve.
	for _, l := range sorted {
		b := baseOf(normalize(l))
		if _, ok := m.tags[b]; !ok {
			m.tags[b] = l
		}
	}

	for _, l := range sorted {
		if l == fallback {
			m.fallback = l
			break
		}
	}
	if m.fallback == "" && len(sorted) > 0 {
		m.fallback = sorted[0]
	}
	return m
}

// Locales returns the page locales in sorted order. The slice is shared and
// must not be modified.
func (m *Matcher) Locales() []string { return m.locales }

// Fallback returns the resolved fallback locale.
func (m *Matcher) Fallback() string { return m.fallback }

type candidate struct {
	tag string
	q   int // thousandths, 1..1000
}

// Resolve returns the page locale for the request: the query value first, then
// Accept-Language entries by descending q. It does not allocate.
func (m *Matcher) Resolve(query, acceptLanguage string) string {
	var cands [maxCandidates]candidate
	n := 0
	if query != "" {
		// The query is candidate 0 and counts toward the cap.
		if l, ok := m.lookup(query); ok {
			return l
		}
		n = 1
	}
	start := n
	s := acceptLanguage
	for s != "" && n < maxCandidates {
		entry := s
		if i := indexByte(s, ','); i >= 0 {
			entry, s = s[:i], s[i+1:]
		} else {
			s = ""
		}
		tag, q, ok := parseEntry(entry)
		if !ok {
			continue
		}
		// Stable insertion by q descending.
		j := n
		for j > start && cands[j-1].q < q {
			cands[j] = cands[j-1]
			j--
		}
		cands[j] = candidate{tag, q}
		n++
	}
	for i := start; i < n; i++ {
		if l, ok := m.lookup(cands[i].tag); ok {
			return l
		}
	}
	return m.fallback
}

// lookup tries the tag itself (exact and same-base are both in the map), then
// its base language.
func (m *Matcher) lookup(tag string) (string, bool) {
	if tag == "" || len(tag) > maxTagLen {
		return "", false
	}
	var buf [maxTagLen]byte
	for i := 0; i < len(tag); i++ {
		buf[i] = normalizeByte(tag[i])
	}
	if l, ok := m.tags[string(buf[:len(tag)])]; ok {
		return l, true
	}
	for i := 1; i < len(tag); i++ {
		if buf[i] == '-' {
			l, ok := m.tags[string(buf[:i])]
			return l, ok
		}
	}
	return "", false
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	for s != "" && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for s != "" && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// parseEntry parses "tag[;q=0.x]". ok is false for empty entries, "*" and
// q=0. A malformed q counts as 1.
func parseEntry(e string) (tag string, q int, ok bool) {
	q = 1000
	params := ""
	if i := indexByte(e, ';'); i >= 0 {
		e, params = e[:i], e[i+1:]
	}
	tag = trimSpace(e)
	if tag == "" || tag == "*" {
		return "", 0, false
	}
	for params != "" {
		p := params
		if i := indexByte(params, ';'); i >= 0 {
			p, params = params[:i], params[i+1:]
		} else {
			params = ""
		}
		p = trimSpace(p)
		if p == "" || (p[0] != 'q' && p[0] != 'Q') {
			continue
		}
		rest := trimSpace(p[1:])
		if rest == "" || rest[0] != '=' {
			continue
		}
		if v, good := parseQ(trimSpace(rest[1:])); good {
			q = v
		}
	}
	if q <= 0 {
		return "", 0, false
	}
	return tag, q, true
}

// parseQ parses "0", "1", "0.5", "1.000" into thousandths. Anything else is
// malformed.
func parseQ(s string) (int, bool) {
	if s == "" || s[0] < '0' || s[0] > '1' {
		return 0, false
	}
	v := int(s[0]-'0') * 1000
	if len(s) == 1 {
		return v, true
	}
	if s[1] != '.' {
		return 0, false
	}
	scale := 100
	for i := 2; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		v += int(s[i]-'0') * scale
		scale /= 10 // digits past the thousandths add 0
	}
	if v > 1000 {
		return 0, false
	}
	return v, true
}
