// Package editor holds the pure parts of the editor (§10): time zone handling
// and the translation between HTML forms and the CR specs.
package editor

import (
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // embedded zone database (§10)
)

// localLayout is the value format of <input type=datetime-local step=1>.
const localLayout = "2006-01-02T15:04:05"

// LoadZone loads an IANA zone. An empty or unknown name is an error: the
// editor never falls back to UTC silently.
func LoadZone(name string) (*time.Location, error) {
	if name == "" {
		return nil, errors.New("no time zone given")
	}
	if name == "Local" {
		return nil, errors.New(`time zone "Local" is not an IANA zone name`)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q", name)
	}
	return loc, nil
}

// wallClock parses a datetime-local value into its wall-clock fields, held in a
// UTC time. Seconds are optional and a fraction is dropped (CRs are
// second-precision).
func wallClock(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"2006-01-02T15:04", localLayout} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.Truncate(time.Second), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a date and time like 2026-10-30T18:00", value)
}

// resolve maps a wall clock (held as UTC) to the instant it names in loc. A
// wall time that does not exist is an error; an ambiguous one takes the
// earlier instant.
func resolve(wall time.Time, loc *time.Location) (time.Time, error) {
	u := wall.Unix()
	offsetAt := func(unix int64) int {
		_, o := time.Unix(unix, 0).In(loc).Zone()
		return o
	}
	// A transition near the wall time shows as a different offset a day before
	// or after; those are the only offsets the wall time can have.
	var best time.Time
	found := false
	for _, probe := range []int64{u - 86400, u + 86400} {
		inst := u - int64(offsetAt(probe))
		if inst+int64(offsetAt(inst)) != u {
			continue // this offset does not reproduce the wall clock
		}
		if t := time.Unix(inst, 0).UTC(); !found || t.Before(best) {
			best, found = t, true
		}
	}
	if !found {
		return time.Time{}, fmt.Errorf("%s does not exist in %s: clocks jump forward at that time; pick another time", wall.Format("2006-01-02 15:04"), loc)
	}
	return best, nil
}

// ParseLocal parses a datetime-local value in loc. It accepts
// "2006-01-02T15:04", "2006-01-02T15:04:05" and a fraction after the seconds,
// which is dropped. An empty value gives the zero time. A wall time that falls
// into a DST gap is an error; an ambiguous one (DST overlap) takes the earlier
// instant.
func ParseLocal(value string, loc *time.Location) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	wall, err := wallClock(value)
	if err != nil {
		return time.Time{}, err
	}
	if loc == nil {
		loc = time.UTC
	}
	return resolve(wall, loc)
}

// FormatLocal formats t in loc for <input type=datetime-local step=1>. The
// zero time gives "".
func FormatLocal(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format(localLayout)
}

// FormatDisplay renders t like "2026-10-30 18:00 Europe/Berlin (16:00 UTC)",
// or "2026-10-30 16:00 UTC" when loc is UTC. The zero time gives "".
func FormatDisplay(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	if loc == nil || loc.String() == "UTC" {
		return t.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	return fmt.Sprintf("%s %s (%s UTC)", t.In(loc).Format("2006-01-02 15:04"), loc, t.UTC().Format("15:04"))
}
