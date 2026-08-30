package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Frequency names how often the periodic library scan runs.
//
// WHY THIS TYPE EXISTS AT ALL. The scheduler used to hold a bare
// time.Duration and start a time.NewTicker from process start, which is a
// duration-from-boot rather than a schedule: any supervisor that restarts the
// process more often than the interval makes the ticker unreachable, and the
// knob meant to REDUCE scanning silently guarantees the maximum rate the
// restart cadence allows (issue #726). A wall-clock frequency plus an anchor
// is the smallest surface that cannot be inverted that way, because the next
// fire is computed from the calendar rather than from an arbitrary epoch.
type Frequency string

const (
	// FrequencyOff disables periodic scanning entirely. The optional startup
	// walk and the filesystem watcher still run.
	FrequencyOff Frequency = "off"
	// FrequencyHourly fires at the top of every hour.
	FrequencyHourly Frequency = "hourly"
	// FrequencyDaily fires at the next occurrence of At, local time.
	FrequencyDaily Frequency = "daily"
	// FrequencyWeekly fires at the next occurrence of Day at At, local time.
	FrequencyWeekly Frequency = "weekly"
	// FrequencyInterval is the DEPRECATED duration-from-now mode that backs the
	// legacy server.scan_interval_seconds key. It is kept so an existing config
	// keeps working for a release rather than silently doing nothing; it is the
	// only mode whose fire times are not anchored to the wall clock, and it is
	// the mode #726 exists to retire.
	FrequencyInterval Frequency = "interval"
)

// TimeOfDay is a wall-clock hour and minute, with no date and no zone. It is
// the anchor for the daily and weekly frequencies.
type TimeOfDay struct {
	Hour   int
	Minute int
}

// ParseTimeOfDay parses a "HH:MM" 24-hour string. It is deliberately strict
// (exactly two fields, both numeric, in range) rather than delegating to
// time.Parse, because time.Parse's "15:04" layout also accepts inputs whose
// meaning an operator would not predict, and a schedule that fires at a time
// nobody intended is the failure mode this whole change exists to remove.
func ParseTimeOfDay(s string) (TimeOfDay, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return TimeOfDay{}, fmt.Errorf("must be HH:MM (24-hour), got %q", s)
	}
	if len(h) != 2 || len(m) != 2 {
		return TimeOfDay{}, fmt.Errorf("must be HH:MM (24-hour, zero-padded), got %q", s)
	}
	hour, err := strconv.Atoi(h)
	if err != nil || hour < 0 || hour > 23 {
		return TimeOfDay{}, fmt.Errorf("hour must be 00-23, got %q", s)
	}
	minute, err := strconv.Atoi(m)
	if err != nil || minute < 0 || minute > 59 {
		return TimeOfDay{}, fmt.Errorf("minute must be 00-59, got %q", s)
	}
	return TimeOfDay{Hour: hour, Minute: minute}, nil
}

// String renders the time of day back as "HH:MM".
func (t TimeOfDay) String() string { return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute) }

// weekdayNames maps the config vocabulary to time.Weekday. Lowercase only: the
// config loader normalizes before this is consulted, so the map has one entry
// per day rather than a case-insensitive scan that could drift from the
// validator's accepted set.
var weekdayNames = map[string]time.Weekday{
	"sunday":    time.Sunday,
	"monday":    time.Monday,
	"tuesday":   time.Tuesday,
	"wednesday": time.Wednesday,
	"thursday":  time.Thursday,
	"friday":    time.Friday,
	"saturday":  time.Saturday,
}

// ParseWeekday resolves a lowercase English weekday name.
func ParseWeekday(s string) (time.Weekday, error) {
	d, ok := weekdayNames[strings.ToLower(strings.TrimSpace(s))]
	if !ok {
		return time.Sunday, fmt.Errorf("must be a weekday name (sunday..saturday), got %q", s)
	}
	return d, nil
}

// WeekdayNames returns the accepted weekday vocabulary in calendar order. It is
// the single source the config enum and the settings dropdown read, so the
// values a user can pick cannot drift from the values ParseWeekday accepts.
func WeekdayNames() []string {
	return []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
}

// FrequencyNames returns the accepted frequency vocabulary, in the order the
// settings dropdown should offer it. FrequencyInterval is deliberately absent:
// it is the deprecated compatibility mode reached only by leaving
// [server.scan_schedule] unconfigured, never something to newly select.
func FrequencyNames() []string {
	return []string{string(FrequencyOff), string(FrequencyHourly), string(FrequencyDaily), string(FrequencyWeekly)}
}

// Schedule describes when the periodic library scan runs. The zero value is
// legacy interval mode with a zero interval, which means "scan once and stop" --
// the same thing a zero Scheduler.Interval has always meant.
type Schedule struct {
	// Frequency selects the mode. An empty value is treated as
	// FrequencyInterval so a Scheduler built the old way (Interval set, nothing
	// else) keeps its previous behavior.
	Frequency Frequency
	// At is the daily/weekly wall-clock anchor, local time. Ignored by hourly
	// (which fires at the top of the hour) and by interval mode.
	At TimeOfDay
	// Day is the weekly anchor. Ignored by every other frequency.
	Day time.Weekday
	// Interval is the legacy duration between scans, used only by
	// FrequencyInterval. A non-positive value means "scan once and stop".
	Interval time.Duration
	// ScanOnStart requests a full library walk at process start, before the
	// first scheduled fire.
	ScanOnStart bool
}

// NextFire returns the next instant this schedule should run after now, and
// whether there is one at all. It is pure: the result depends only on the
// arguments, which is what makes DST, month boundaries, and weekday wrap
// directly testable rather than something you can only observe by waiting.
//
// MISSED WINDOWS ARE SKIPPED, NOT RUN LATE. Every call computes the next
// occurrence strictly after now, so a machine that was asleep or a container
// that was down through 04:00 does not walk the library the moment it comes
// back -- it waits for tomorrow's 04:00. That is the deliberate choice: this
// scan is a backstop behind the filesystem watcher, and a catch-up run on
// every boot is precisely the "restart schedule wins" behavior #726 is about.
//
// All arithmetic goes through time.Date in now's location, so "daily at 04:00"
// means 04:00 local on each calendar day rather than a fixed 24-hour stride.
// Across a DST transition the stride is 23 or 25 hours and the wall-clock time
// is preserved, which is what an operator scheduling by clock time expects. On
// a spring-forward day a nonexistent local time (e.g. 02:30 where 02:00 jumps
// to 03:00) is normalized by time.Date into the adjacent real instant, so the
// run happens once rather than being skipped for the year.
func (s Schedule) NextFire(now time.Time) (time.Time, bool) {
	loc := now.Location()
	switch s.Frequency {
	case FrequencyOff:
		return time.Time{}, false
	case FrequencyInterval, "":
		if s.Interval <= 0 {
			return time.Time{}, false
		}
		return now.Add(s.Interval), true
	case FrequencyHourly:
		y, m, d := now.Date()
		return time.Date(y, m, d, now.Hour()+1, 0, 0, 0, loc), true
	case FrequencyDaily:
		return advanceUntilAfter(now, func(offset int) time.Time {
			y, m, d := now.Date()
			return time.Date(y, m, d+offset, s.At.Hour, s.At.Minute, 0, 0, loc)
		}, 1), true
	case FrequencyWeekly:
		// Days until the target weekday, taking today when the anchor has not
		// passed yet; advanceUntilAfter rolls a whole week forward otherwise.
		ahead := (int(s.Day) - int(now.Weekday()) + 7) % 7
		return advanceUntilAfter(now, func(offset int) time.Time {
			y, m, d := now.Date()
			return time.Date(y, m, d+ahead+offset, s.At.Hour, s.At.Minute, 0, 0, loc)
		}, 7), true
	default:
		// An unrecognized frequency never reaches here from the config loader
		// (which rejects it at boot), but returning "no next fire" rather than
		// guessing keeps a programming error quiet on disk instead of scanning
		// on a cadence nobody asked for.
		return time.Time{}, false
	}
}

// advanceUntilAfter calls build with increasing multiples of step until the
// constructed instant is strictly after now. The bound exists because time.Date
// normalization around a DST transition can, in principle, hand back an instant
// that is not the naive wall-clock arithmetic; two periods is always enough,
// and the loop is bounded so a future zoneinfo oddity can never spin here.
func advanceUntilAfter(now time.Time, build func(offset int) time.Time, step int) time.Time {
	candidate := build(0)
	for i := 1; i <= 2 && !candidate.After(now); i++ {
		candidate = build(i * step)
	}
	return candidate
}

// String renders the schedule for a log line. It never carries a library path
// or any other private value, so it is safe on any surface the logs reach.
func (s Schedule) String() string {
	switch s.Frequency {
	case FrequencyOff:
		return "off"
	case FrequencyHourly:
		return "hourly"
	case FrequencyDaily:
		return "daily at " + s.At.String()
	case FrequencyWeekly:
		return "weekly on " + strings.ToLower(s.Day.String()) + " at " + s.At.String()
	case FrequencyInterval, "":
		if s.Interval <= 0 {
			return "once"
		}
		return "every " + s.Interval.String() + " (deprecated interval mode)"
	default:
		return string(s.Frequency)
	}
}
