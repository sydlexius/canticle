package schedule_test

import (
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/schedule"
)

func TestParseTimeOfDay(t *testing.T) {
	cases := []struct {
		in      string
		want    schedule.TimeOfDay
		wantErr bool
	}{
		{in: "04:00", want: schedule.TimeOfDay{Hour: 4}},
		{in: "00:00", want: schedule.TimeOfDay{}},
		{in: "23:59", want: schedule.TimeOfDay{Hour: 23, Minute: 59}},
		{in: " 04:30 ", want: schedule.TimeOfDay{Hour: 4, Minute: 30}},
		{in: "24:00", wantErr: true},
		{in: "04:60", wantErr: true},
		{in: "4:00", wantErr: true},
		{in: "0400", wantErr: true},
		{in: "", wantErr: true},
		{in: "aa:bb", wantErr: true},
	}
	for _, tc := range cases {
		got, err := schedule.ParseTimeOfDay(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseTimeOfDay(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTimeOfDay(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseTimeOfDay(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseWeekday(t *testing.T) {
	if got, err := schedule.ParseWeekday("Sunday"); err != nil || got != time.Sunday {
		t.Errorf("ParseWeekday(Sunday) = %v, %v; want Sunday, nil", got, err)
	}
	if _, err := schedule.ParseWeekday("sundae"); err == nil {
		t.Error("ParseWeekday(sundae) = nil error; want error")
	}
}

// TestWeekdayNamesRoundTrip is the drift guard between the vocabulary the
// config enum offers and the vocabulary ParseWeekday accepts: an offered value
// that does not parse would be selectable in the UI and fatal at boot.
func TestWeekdayNamesRoundTrip(t *testing.T) {
	names := schedule.WeekdayNames()
	if len(names) != 7 {
		t.Fatalf("WeekdayNames len = %d; want 7", len(names))
	}
	for i, n := range names {
		d, err := schedule.ParseWeekday(n)
		if err != nil {
			t.Errorf("offered weekday %q does not parse: %v", n, err)
			continue
		}
		if int(d) != i {
			t.Errorf("weekday %q = %v; want index %d (calendar order)", n, d, i)
		}
	}
}

// TestFrequencyNamesAllSchedulable asserts every offered frequency actually
// produces a decision from NextFire (a next instant, or an explicit "never"),
// so a dropdown value can never be a mode the scheduler does not implement.
func TestFrequencyNamesAllSchedulable(t *testing.T) {
	now := time.Date(2026, time.March, 3, 12, 0, 0, 0, time.UTC)
	for _, name := range schedule.FrequencyNames() {
		s := schedule.Schedule{
			Frequency: schedule.Frequency(name),
			At:        schedule.TimeOfDay{Hour: 4},
			Day:       time.Sunday,
		}
		next, ok := s.NextFire(now)
		if name == string(schedule.FrequencyOff) {
			if ok {
				t.Errorf("frequency %q returned a fire time %v; want none", name, next)
			}
			continue
		}
		if !ok {
			t.Errorf("frequency %q returned no fire time", name)
			continue
		}
		if !next.After(now) {
			t.Errorf("frequency %q next = %v; want strictly after %v", name, next, now)
		}
	}
}

func TestNextFireHourly(t *testing.T) {
	s := schedule.Schedule{Frequency: schedule.FrequencyHourly}
	now := time.Date(2026, time.March, 3, 12, 37, 42, 0, time.UTC)
	next, ok := s.NextFire(now)
	if !ok {
		t.Fatal("NextFire returned no time")
	}
	want := time.Date(2026, time.March, 3, 13, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("hourly next = %v; want %v", next, want)
	}
	// Exactly on the hour must advance, not return now: a fire time equal to
	// now would make the loop scan back-to-back.
	next, _ = s.NextFire(want)
	if !next.After(want) {
		t.Errorf("hourly next from the top of the hour = %v; want strictly after %v", next, want)
	}
}

func TestNextFireDaily(t *testing.T) {
	s := schedule.Schedule{Frequency: schedule.FrequencyDaily, At: schedule.TimeOfDay{Hour: 4}}

	// Before today's anchor: fires today.
	now := time.Date(2026, time.March, 3, 1, 0, 0, 0, time.UTC)
	next, _ := s.NextFire(now)
	if want := time.Date(2026, time.March, 3, 4, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("daily before anchor = %v; want %v", next, want)
	}

	// After today's anchor: rolls to tomorrow rather than running late. This is
	// the missed-window policy: a process started at 09:00 waits for tomorrow.
	now = time.Date(2026, time.March, 3, 9, 0, 0, 0, time.UTC)
	next, _ = s.NextFire(now)
	if want := time.Date(2026, time.March, 4, 4, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("daily after anchor = %v; want %v", next, want)
	}

	// Month boundary.
	now = time.Date(2026, time.January, 31, 23, 0, 0, 0, time.UTC)
	next, _ = s.NextFire(now)
	if want := time.Date(2026, time.February, 1, 4, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("daily across a month boundary = %v; want %v", next, want)
	}

	// Leap-year February boundary: 2028-02-29 exists, so the next fire is the
	// 29th, not March 1st.
	now = time.Date(2028, time.February, 28, 23, 0, 0, 0, time.UTC)
	next, _ = s.NextFire(now)
	if want := time.Date(2028, time.February, 29, 4, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("daily across a leap-day boundary = %v; want %v", next, want)
	}
}

func TestNextFireWeekly(t *testing.T) {
	s := schedule.Schedule{Frequency: schedule.FrequencyWeekly, Day: time.Sunday, At: schedule.TimeOfDay{Hour: 4}}

	// Tuesday -> the coming Sunday.
	now := time.Date(2026, time.March, 3, 12, 0, 0, 0, time.UTC) // a Tuesday
	if now.Weekday() != time.Tuesday {
		t.Fatalf("fixture date is a %v, not a Tuesday", now.Weekday())
	}
	next, _ := s.NextFire(now)
	if want := time.Date(2026, time.March, 8, 4, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("weekly from Tuesday = %v; want %v", next, want)
	}

	// The anchor day itself, before the time: fires today.
	now = time.Date(2026, time.March, 8, 1, 0, 0, 0, time.UTC)
	next, _ = s.NextFire(now)
	if want := time.Date(2026, time.March, 8, 4, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("weekly on the anchor day before the time = %v; want %v", next, want)
	}

	// The anchor day, after the time: a FULL WEEK forward, not the next day.
	// This is the weekday-wrap case a naive "+1 day until the weekday matches"
	// implementation gets wrong.
	now = time.Date(2026, time.March, 8, 9, 0, 0, 0, time.UTC)
	next, _ = s.NextFire(now)
	if want := time.Date(2026, time.March, 15, 4, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("weekly on the anchor day after the time = %v; want %v", next, want)
	}
}

// TestNextFireDailyAcrossDST is the reason the computation goes through
// time.Date in the caller's location rather than adding 24 hours: on a
// transition day the correct stride is 23 or 25 hours, and the WALL-CLOCK time
// is what must be preserved. An operator who asked for 04:00 wants 04:00 on
// both sides of the change, not 03:00 or 05:00.
func TestNextFireDailyAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	s := schedule.Schedule{Frequency: schedule.FrequencyDaily, At: schedule.TimeOfDay{Hour: 4}}

	// Spring forward: 2026-03-08 02:00 EST -> 03:00 EDT. From 04:00 on the 7th
	// the next fire is 04:00 on the 8th, 23 hours later.
	now := time.Date(2026, time.March, 7, 4, 0, 0, 0, ny)
	next, _ := s.NextFire(now)
	if h, m := next.Hour(), next.Minute(); h != 4 || m != 0 {
		t.Errorf("spring-forward next = %v; want wall-clock 04:00", next)
	}
	if got := next.Sub(now); got != 23*time.Hour {
		t.Errorf("spring-forward stride = %v; want 23h", got)
	}

	// Fall back: 2026-11-01 02:00 EDT -> 01:00 EST. From 04:00 on Oct 31 the
	// next fire is 04:00 on Nov 1, 25 hours later.
	now = time.Date(2026, time.October, 31, 4, 0, 0, 0, ny)
	next, _ = s.NextFire(now)
	if h, m := next.Hour(), next.Minute(); h != 4 || m != 0 {
		t.Errorf("fall-back next = %v; want wall-clock 04:00", next)
	}
	if got := next.Sub(now); got != 25*time.Hour {
		t.Errorf("fall-back stride = %v; want 25h", got)
	}
}

// TestNextFireSkippedLocalTimeStillFires covers the nastiest DST case: an
// anchor inside the hour that does not exist on the spring-forward day. The run
// must still happen (normalized into the adjacent real instant) rather than
// being silently skipped, and it must still be strictly in the future.
func TestNextFireSkippedLocalTimeStillFires(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	s := schedule.Schedule{Frequency: schedule.FrequencyDaily, At: schedule.TimeOfDay{Hour: 2, Minute: 30}}
	now := time.Date(2026, time.March, 7, 12, 0, 0, 0, ny)
	next, ok := s.NextFire(now)
	if !ok {
		t.Fatal("NextFire returned no time on the spring-forward day")
	}
	if !next.After(now) {
		t.Fatalf("next = %v; want strictly after %v", next, now)
	}
	if d := next.Sub(now); d > 24*time.Hour {
		t.Errorf("next = %v (%v away); a nonexistent local anchor must not skip the day", next, d)
	}
}

// TestNextFireIsIndependentOfProcessStart is the regression guard for #726
// itself: two schedulers whose "process start" differs by hours must agree on
// the next fire, because the anchor is the calendar and not the boot instant.
func TestNextFireIsIndependentOfProcessStart(t *testing.T) {
	s := schedule.Schedule{Frequency: schedule.FrequencyDaily, At: schedule.TimeOfDay{Hour: 4}}
	want := time.Date(2026, time.March, 4, 4, 0, 0, 0, time.UTC)
	for _, now := range []time.Time{
		time.Date(2026, time.March, 3, 4, 0, 1, 0, time.UTC),
		time.Date(2026, time.March, 3, 13, 12, 0, 0, time.UTC),
		time.Date(2026, time.March, 4, 3, 59, 59, 0, time.UTC),
	} {
		next, _ := s.NextFire(now)
		if !next.Equal(want) {
			t.Errorf("NextFire(%v) = %v; want %v regardless of when the process started", now, next, want)
		}
	}
}

func TestNextFireOffAndInterval(t *testing.T) {
	now := time.Date(2026, time.March, 3, 12, 0, 0, 0, time.UTC)

	if _, ok := (schedule.Schedule{Frequency: schedule.FrequencyOff}).NextFire(now); ok {
		t.Error("off returned a fire time; want none")
	}
	// The zero value is legacy interval mode with a zero interval, which is the
	// historical "scan once and stop".
	if _, ok := (schedule.Schedule{}).NextFire(now); ok {
		t.Error("zero schedule returned a fire time; want none (scan once)")
	}
	s := schedule.Schedule{Frequency: schedule.FrequencyInterval, Interval: 90 * time.Minute}
	next, ok := s.NextFire(now)
	if !ok {
		t.Fatal("interval mode returned no fire time")
	}
	if want := now.Add(90 * time.Minute); !next.Equal(want) {
		t.Errorf("interval next = %v; want %v", next, want)
	}
}

func TestScheduleString(t *testing.T) {
	cases := []struct {
		s    schedule.Schedule
		want string
	}{
		{schedule.Schedule{Frequency: schedule.FrequencyOff}, "off"},
		{schedule.Schedule{Frequency: schedule.FrequencyHourly}, "hourly"},
		{schedule.Schedule{Frequency: schedule.FrequencyDaily, At: schedule.TimeOfDay{Hour: 4, Minute: 5}}, "daily at 04:05"},
		{schedule.Schedule{Frequency: schedule.FrequencyWeekly, Day: time.Sunday, At: schedule.TimeOfDay{Hour: 4}}, "weekly on sunday at 04:00"},
		{schedule.Schedule{}, "once"},
		{schedule.Schedule{Frequency: schedule.FrequencyInterval, Interval: time.Hour}, "every 1h0m0s (deprecated interval mode)"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("String() = %q; want %q", got, tc.want)
		}
	}
}
