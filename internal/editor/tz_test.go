package editor

import (
	"strings"
	"testing"
	"time"
)

func mustZone(t testing.TB, name string) *time.Location {
	t.Helper()
	loc, err := LoadZone(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestLoadZone(t *testing.T) {
	for _, name := range []string{"", "Local", "Mars/Olympus", "../etc/passwd"} {
		if _, err := LoadZone(name); err == nil {
			t.Errorf("LoadZone(%q) succeeded", name)
		}
	}
	for _, name := range []string{"UTC", "Europe/Berlin", "Australia/Lord_Howe"} {
		if _, err := LoadZone(name); err != nil {
			t.Errorf("LoadZone(%q): %v", name, err)
		}
	}
}

func TestParseLocal(t *testing.T) {
	tests := []struct {
		name, zone, in string
		want           string // RFC3339 UTC; "" = zero time
		errHas         string
	}{
		{name: "empty", zone: "UTC", in: "", want: ""},
		{name: "utc minutes", zone: "UTC", in: "2026-10-30T18:00", want: "2026-10-30T18:00:00Z"},
		{name: "utc seconds", zone: "UTC", in: "2026-10-30T18:00:07", want: "2026-10-30T18:00:07Z"},
		{name: "fraction dropped", zone: "UTC", in: "2026-10-30T18:00:07.987654321", want: "2026-10-30T18:00:07Z"},
		{name: "millis dropped", zone: "UTC", in: "2026-10-30T18:00:07.000", want: "2026-10-30T18:00:07Z"},
		{name: "berlin summer", zone: "Europe/Berlin", in: "2026-07-01T12:00", want: "2026-07-01T10:00:00Z"},
		{name: "berlin winter", zone: "Europe/Berlin", in: "2026-12-01T12:00", want: "2026-12-01T11:00:00Z"},
		{name: "berlin spring gap", zone: "Europe/Berlin", in: "2026-03-29T02:30", errHas: "Europe/Berlin"},
		{name: "berlin spring gap start", zone: "Europe/Berlin", in: "2026-03-29T02:00:00", errHas: "clocks jump forward"},
		{name: "berlin just after gap", zone: "Europe/Berlin", in: "2026-03-29T03:00", want: "2026-03-29T01:00:00Z"},
		{name: "berlin just before gap", zone: "Europe/Berlin", in: "2026-03-29T01:59:59", want: "2026-03-29T00:59:59Z"},
		{name: "berlin autumn overlap earlier", zone: "Europe/Berlin", in: "2026-10-25T02:30", want: "2026-10-25T00:30:00Z"},
		{name: "berlin overlap start", zone: "Europe/Berlin", in: "2026-10-25T02:00:00", want: "2026-10-25T00:00:00Z"},
		{name: "berlin overlap end", zone: "Europe/Berlin", in: "2026-10-25T02:59:59", want: "2026-10-25T00:59:59Z"},
		{name: "berlin after overlap", zone: "Europe/Berlin", in: "2026-10-25T03:00", want: "2026-10-25T02:00:00Z"},
		{name: "new york gap", zone: "America/New_York", in: "2026-03-08T02:30", errHas: "America/New_York"},
		{name: "new york overlap", zone: "America/New_York", in: "2026-11-01T01:30", want: "2026-11-01T05:30:00Z"},
		{name: "new york normal", zone: "America/New_York", in: "2026-11-01T03:00", want: "2026-11-01T08:00:00Z"},
		// Lord Howe shifts by 30 minutes: +11:00 -> +10:30 on 2026-04-05 at 02:00,
		// and +10:30 -> +11:00 on 2026-10-04 at 02:00.
		{name: "lord howe gap", zone: "Australia/Lord_Howe", in: "2026-10-04T02:15", errHas: "Lord_Howe"},
		{name: "lord howe after gap", zone: "Australia/Lord_Howe", in: "2026-10-04T02:30", want: "2026-10-03T15:30:00Z"},
		{name: "lord howe overlap", zone: "Australia/Lord_Howe", in: "2026-04-05T01:45", want: "2026-04-04T14:45:00Z"},
		{name: "garbage", zone: "UTC", in: "tomorrow", errHas: "not a date"},
		{name: "date only", zone: "UTC", in: "2026-10-30", errHas: "not a date"},
		{name: "with zone suffix", zone: "UTC", in: "2026-10-30T18:00Z", errHas: "not a date"},
		{name: "bad fraction", zone: "UTC", in: "2026-10-30T18:00:07.x", errHas: "not a date"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseLocal(tc.in, mustZone(t, tc.zone))
			if tc.errHas != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errHas) {
					t.Fatalf("err = %v, want one containing %q", err, tc.errHas)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if !got.IsZero() {
					t.Fatalf("got %v, want zero", got)
				}
				return
			}
			if want := utc(tc.want); !got.Equal(want) {
				t.Fatalf("got %s, want %s", got.UTC().Format(time.RFC3339), tc.want)
			}
		})
	}
}

func TestParseLocalGapMessage(t *testing.T) {
	_, err := ParseLocal("2026-03-29T02:30", mustZone(t, "Europe/Berlin"))
	if err == nil || !strings.Contains(err.Error(), "Europe/Berlin") || !strings.Contains(err.Error(), "clocks jump forward") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseLocalNilZoneIsUTC(t *testing.T) {
	got, err := ParseLocal("2026-10-30T18:00", nil)
	if err != nil || !got.Equal(utc("2026-10-30T18:00:00Z")) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestFormatLocalAndDisplay(t *testing.T) {
	berlin := mustZone(t, "Europe/Berlin")
	at := utc("2026-10-30T16:00:00Z")
	if got := FormatLocal(at, berlin); got != "2026-10-30T17:00:00" {
		t.Errorf("FormatLocal = %q", got)
	}
	if got := FormatLocal(time.Time{}, berlin); got != "" {
		t.Errorf("FormatLocal(zero) = %q", got)
	}
	if got := FormatDisplay(utc("2026-10-30T17:00:00Z"), berlin); got != "2026-10-30 18:00 Europe/Berlin (17:00 UTC)" {
		t.Errorf("FormatDisplay = %q", got)
	}
	if got := FormatDisplay(at, berlin); got != "2026-10-30 17:00 Europe/Berlin (16:00 UTC)" {
		t.Errorf("FormatDisplay = %q", got)
	}
	if got := FormatDisplay(at, time.UTC); got != "2026-10-30 16:00 UTC" {
		t.Errorf("FormatDisplay(UTC) = %q", got)
	}
	if got := FormatDisplay(time.Time{}, berlin); got != "" {
		t.Errorf("FormatDisplay(zero) = %q", got)
	}
}

// TestLocalRoundTrip formats many instants and parses them back. An instant in
// a DST overlap parses to the earlier of its two copies; every other instant
// comes back unchanged.
func TestLocalRoundTrip(t *testing.T) {
	zones := []string{"UTC", "Europe/Berlin", "America/New_York", "Australia/Lord_Howe", "Asia/Kolkata", "Pacific/Chatham"}
	start := utc("2025-01-01T00:00:00Z").Unix()
	end := utc("2028-01-01T00:00:00Z").Unix()
	for _, name := range zones {
		loc := mustZone(t, name)
		ambiguous := 0
		step := int64(1799) // odd step walks across every minute alignment
		for u := start; u < end; u += step {
			at := time.Unix(u, 0).UTC()
			s := FormatLocal(at, loc)
			got, err := ParseLocal(s, loc)
			if err != nil {
				t.Fatalf("%s: ParseLocal(%q): %v", name, s, err)
			}
			if got.Equal(at) {
				continue
			}
			// Not the same instant: it must be the earlier copy of an overlap.
			ambiguous++
			if !got.Before(at) || FormatLocal(got, loc) != s {
				t.Fatalf("%s: %s parsed to %s, not the earlier copy of the same wall clock", name, s, got.Format(time.RFC3339))
			}
			if _, off1 := got.In(loc).Zone(); true {
				if _, off2 := at.In(loc).Zone(); off1 <= off2 {
					t.Fatalf("%s: earlier copy %s has offset %d, later has %d", name, s, off1, off2)
				}
			}
		}
		if name != "UTC" && name != "Asia/Kolkata" && ambiguous == 0 {
			t.Errorf("%s: walked no overlap hour", name)
		}
	}
}
