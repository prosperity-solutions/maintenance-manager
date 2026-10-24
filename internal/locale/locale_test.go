package locale

import (
	"reflect"
	"strings"
	"testing"
)

func TestNewMatcher(t *testing.T) {
	m := NewMatcher([]string{"fr_CA", "en", "de-CH", "de-AT"}, "xx")
	if want := []string{"de-AT", "de-CH", "en", "fr_CA"}; !reflect.DeepEqual(m.Locales(), want) {
		t.Fatalf("Locales = %v, want %v", m.Locales(), want)
	}
	if m.Fallback() != "de-AT" {
		t.Fatalf("Fallback = %q, want first sorted", m.Fallback())
	}
	if got := NewMatcher([]string{"en", "de"}, "en").Fallback(); got != "en" {
		t.Fatalf("Fallback = %q", got)
	}
	e := NewMatcher(nil, "en")
	if e.Resolve("de", "fr") != "" || len(e.Locales()) != 0 {
		t.Fatal("empty matcher misbehaves")
	}
}

func TestResolve(t *testing.T) {
	full := NewMatcher([]string{"en", "de", "de-CH", "fr_CA"}, "en")
	noBase := NewMatcher([]string{"en", "de-CH", "de-AT"}, "en")
	tests := []struct {
		name  string
		m     *Matcher
		query string
		al    string
		want  string
	}{
		{"exact", full, "", "de-ch", "de-CH"},
		{"exact case", full, "", "DE-cH", "de-CH"},
		{"base", full, "", "de-DE", "de"},
		{"same base", noBase, "", "de", "de-AT"},
		{"same base via region", noBase, "", "de-LI", "de-AT"},
		{"underscore request", full, "", "fr-ca", "fr_CA"},
		{"underscore both", full, "", "fr_CA", "fr_CA"},
		{"original case kept", full, "", "FR", "fr_CA"},
		{"q ordering", full, "", "en;q=0.5, de;q=0.9", "de"},
		{"q ties stable", full, "", "de, en", "de"},
		{"q ties stable 2", full, "", "en, de", "en"},
		{"q=0 ignored", full, "", "de;q=0, fr;q=0.1", "fr_CA"},
		{"q=0.000 ignored", full, "", "de;q=0.000", "en"},
		{"star skipped", full, "", "*, de;q=0.1", "de"},
		{"star only", noBase, "", "*", "en"},
		{"malformed q is 1", full, "", "en;q=0.5, de;q=abc", "de"},
		{"q above 1 malformed", full, "", "en;q=0.5, de;q=7", "de"},
		{"spaces", full, "", "  fr-CA ; q=0.3 ,  de ; q=0.8 ", "de"},
		{"query wins", full, "de-CH", "en", "de-CH"},
		{"query miss falls to header", full, "zz", "fr-CA", "fr_CA"},
		{"query underscore", full, "de_ch", "", "de-CH"},
		{"empty both", full, "", "", "en"},
		{"empty entries", noBase, "", ",,;,", "en"},
		{"no match", full, "", "ja, ko", "en"},
		{"leading dash", full, "-de", "", "en"},
		{"too long tag", full, strings.Repeat("a", 100), "", "en"},
		{"garbage", full, "", ";;;q=,,=", "en"},
	}
	for _, tc := range tests {
		if got := tc.m.Resolve(tc.query, tc.al); got != tc.want {
			t.Errorf("%s: Resolve(%q, %q) = %q, want %q", tc.name, tc.query, tc.al, got, tc.want)
		}
	}
}

func TestResolveCandidateCap(t *testing.T) {
	m := NewMatcher([]string{"en", "de"}, "en")
	if got := m.Resolve("", strings.Repeat("xx, ", 8)+"de"); got != "en" {
		t.Fatalf("9th candidate considered: %q", got)
	}
	al := strings.Repeat("xx, ", 7) + "de"
	if got := m.Resolve("", al); got != "de" {
		t.Fatalf("8th candidate ignored: %q", got)
	}
	// The query takes one of the 8 slots.
	if got := m.Resolve("xx", al); got != "en" {
		t.Fatalf("query slot not counted: %q", got)
	}
	// Skipped entries (*, q=0) do not use a slot.
	if got := m.Resolve("", strings.Repeat("*, ", 20)+"de"); got != "de" {
		t.Fatalf("wildcards used slots: %q", got)
	}
}

func TestResolveZeroAllocs(t *testing.T) {
	m := NewMatcher([]string{"en", "de", "de-CH", "fr_CA"}, "en")
	long := strings.Repeat("xx-YY;q=0.2, ", 80)[:1024]
	for _, al := range []string{"", "de-CH,de;q=0.9,en;q=0.8", "ja, ko;q=0.5, fr-ca;q=0.4", long} {
		for _, q := range []string{"", "de_CH", "zz"} {
			if n := testing.AllocsPerRun(100, func() { _ = m.Resolve(q, al) }); n != 0 {
				t.Errorf("Resolve(%q, %.20q) allocs = %v", q, al, n)
			}
		}
	}
}

func BenchmarkResolve(b *testing.B) {
	m := NewMatcher([]string{"en", "de", "de-CH", "fr_CA"}, "en")
	const al = "fr-CH, fr;q=0.9, en;q=0.8, de;q=0.7, *;q=0.5"
	b.ReportAllocs()
	for b.Loop() {
		_ = m.Resolve("", al)
	}
}

func FuzzResolve(f *testing.F) {
	for _, s := range []string{
		"", "de", "de-CH", "de_ch;q=0.5", "*", "en;q=", "en;q=x;q=1", ";", ",", "-", "--;q=0",
		"en;q=0.5,de;q=0.9,fr;q=1,it,es,pt,nl,sv,da", "\xff\xfe", "de;q=1e9", "a;q=....",
		strings.Repeat("a", 200), strings.Repeat("a,", 600),
	} {
		f.Add(s, s)
		f.Add("de", s)
	}
	m := NewMatcher([]string{"en", "de", "de-CH", "fr_CA"}, "en")
	f.Fuzz(func(t *testing.T, q, al string) {
		got := m.Resolve(q, al)
		for _, l := range m.Locales() {
			if l == got {
				return
			}
		}
		t.Fatalf("Resolve(%q, %q) = %q, not a page locale", q, al, got)
	})
}
