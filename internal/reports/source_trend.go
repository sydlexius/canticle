package reports

import (
	"context"
	"fmt"
	"time"
)

// TrendMaxDays is the widest trend window; SourceTrend always reads this much
// so "no history" is judged over the same span whatever range is displayed.
const TrendMaxDays = 90

// TrendDay is one UTC day of a source's counters (#1302). HitRate is a percent
// in [0, 100] and nil on a day with no attempts (hits + misses == 0): a day
// nobody asked is a gap, never 0 percent. The type counts are landings, not
// distinct tracks.
type TrendDay struct {
	Day                                string
	HitRate                            *float64
	Hits, Misses                       int64
	Word, Line, Unsynced, Instrumental int64
}

// SourceTrend is a source's oldest-first daily series. HasHistory is false
// when the source has no counter row at all in the last TrendMaxDays days.
type SourceTrend struct {
	Days       []TrendDay
	HasHistory bool
}

// BuildSourceTrend folds rows for lane into the days UTC days ending at to
// (inclusive), one TrendDay per day, including days with no rows. history is
// computed over rows as given, so callers pass the full TrendMaxDays window.
func BuildSourceTrend(rows []SourceEventCount, lane string, to time.Time, days int) SourceTrend {
	if days < 1 {
		return SourceTrend{}
	}
	end := to.UTC()
	idx := make(map[string]int, days)
	out := SourceTrend{Days: make([]TrendDay, days)}
	for i := 0; i < days; i++ {
		d := end.AddDate(0, 0, i-(days-1)).Format("2006-01-02")
		out.Days[i].Day = d
		idx[d] = i
	}
	for _, r := range rows {
		if r.Lane != lane {
			continue
		}
		out.HasHistory = true
		i, ok := idx[r.Day]
		if !ok {
			continue
		}
		d := &out.Days[i]
		switch r.Event {
		case "hit":
			d.Hits += r.Count
		case "miss":
			d.Misses += r.Count
		case "word":
			d.Word += r.Count
		case "line":
			d.Line += r.Count
		case "unsynced":
			d.Unsynced += r.Count
		case "instrumental":
			d.Instrumental += r.Count
		}
	}
	for i := range out.Days {
		d := &out.Days[i]
		if n := d.Hits + d.Misses; n > 0 {
			p := float64(d.Hits) * 100 / float64(n)
			d.HitRate = &p
		}
	}
	return out
}

// SourceTrend returns lane's last days UTC days ending at now. HasHistory is
// judged over the full TrendMaxDays window, so a 7-day view of a source that
// was only active last month is a (gappy) chart, not "no history yet".
func (r *Repo) SourceTrend(ctx context.Context, lane string, now time.Time, days int) (SourceTrend, error) {
	end := now.UTC()
	rows, err := r.sourceEvents(ctx, end.AddDate(0, 0, -(TrendMaxDays-1)), end, lane)
	if err != nil {
		return SourceTrend{}, fmt.Errorf("reports: source trend: %w", err)
	}
	return BuildSourceTrend(rows, lane, end, days), nil
}
