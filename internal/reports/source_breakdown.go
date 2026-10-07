package reports

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/sydlexius/canticle/internal/providers"
)

// TypeCounts is a count of done rows by delivered type. The fields are the
// ResultsBreakdown buckets, so Total is the number of done rows counted.
type TypeCounts struct {
	WordSynced   int64
	LineSynced   int64
	Unsynced     int64
	Instrumental int64
	// TierUnknown is ResultsBreakdown.SyncedTierUnknown.
	TierUnknown int64
	// Other is ResultsBreakdown.Other (retired, rejected, legacy NULL outcome),
	// kept so the counts sum to the group's total by construction.
	Other int64
}

// Total is the sum of every type.
func (c TypeCounts) Total() int64 {
	return c.WordSynced + c.LineSynced + c.Unsynced + c.Instrumental + c.TierUnknown + c.Other
}

func (c *TypeCounts) add(bucket string, n int64) {
	switch bucket {
	case "word":
		c.WordSynced += n
	case "line":
		c.LineSynced += n
	case "tier_unknown":
		c.TierUnknown += n
	case "unsynced":
		c.Unsynced += n
	case "instrumental":
		c.Instrumental += n
	default:
		c.Other += n
	}
}

// UpstreamBreakdown is one licensor's slice of a multiplexing source.
type UpstreamBreakdown struct {
	// Upstream is the recorded licensor; empty when NotRecorded.
	Upstream string
	// NotRecorded marks rows whose work_queue.upstream is NULL (settled before
	// #1297, or a result that reported none). Distinct from any named licensor.
	NotRecorded bool
	Counts      TypeCounts
}

// SourceBreakdown is one source's done rows by delivered type.
type SourceBreakdown struct {
	// Lane is the persisted provider_lane; empty when Unattributed.
	Lane string
	// Unattributed marks rows with a NULL provider_lane (a cache hit, a
	// pre-attribution row). It is its own group, never folded into a source.
	Unattributed bool
	// Counts covers every row of the source, whatever its upstream.
	Counts TypeCounts
	// Upstreams is the per-licensor split, populated only for a lane in
	// providers.UpstreamLanes; its counts sum to Counts. Nil otherwise.
	Upstreams []UpstreamBreakdown
}

// SourceBreakdown returns, per lyrics source, the done rows by delivered type
// (#1299). Sources are ordered by total descending (then lane), with the
// unattributed group last; upstreams likewise, "not recorded" last.
//
// Classification is resultBucketCaseSQL, the very fragment ResultsBreakdown
// selects, over the same status='done' population, so the per-type sums across
// sources equal the ResultsBreakdown tiles and the totals sum to
// QueueSummary.Done. top-rung does not touch this split: it only changes what
// QueueSummary.Finished counts, never a row's word/line/unknown bucket.
//
// One scan over done rows (no index covers it); intended for an on-demand page.
func (r *Repo) SourceBreakdown(ctx context.Context) ([]SourceBreakdown, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT provider_lane, upstream, `+resultBucketCaseSQL+` AS bucket, COUNT(*)
         FROM work_queue
         WHERE status = 'done'
         GROUP BY provider_lane, upstream, bucket`)
	if err != nil {
		return nil, fmt.Errorf("reports: source breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()

	multiplexing := providers.UpstreamLanes()
	bySource := map[string]*SourceBreakdown{}
	byUpstream := map[string]map[string]*UpstreamBreakdown{}
	var order []string
	for rows.Next() {
		var lane, upstream *string
		var bucket string
		var n int64
		if err := rows.Scan(&lane, &upstream, &bucket, &n); err != nil {
			return nil, fmt.Errorf("reports: scan source breakdown: %w", err)
		}
		key, name := "", ""
		if lane != nil {
			key, name = "L:"+*lane, *lane
		}
		sb, ok := bySource[key]
		if !ok {
			sb = &SourceBreakdown{Lane: name, Unattributed: lane == nil}
			bySource[key] = sb
			order = append(order, key)
		}
		sb.Counts.add(bucket, n)
		if lane == nil || !slices.Contains(multiplexing, name) {
			continue
		}
		uk, un := "", ""
		if upstream != nil {
			uk, un = "U:"+*upstream, *upstream
		}
		if byUpstream[key] == nil {
			byUpstream[key] = map[string]*UpstreamBreakdown{}
		}
		ub, ok := byUpstream[key][uk]
		if !ok {
			ub = &UpstreamBreakdown{Upstream: un, NotRecorded: upstream == nil}
			byUpstream[key][uk] = ub
		}
		ub.Counts.add(bucket, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: source breakdown rows: %w", err)
	}

	out := make([]SourceBreakdown, 0, len(order))
	for _, key := range order {
		sb := *bySource[key]
		for _, ub := range byUpstream[key] {
			sb.Upstreams = append(sb.Upstreams, *ub)
		}
		sort.Slice(sb.Upstreams, func(i, j int) bool {
			a, b := sb.Upstreams[i], sb.Upstreams[j]
			if a.NotRecorded != b.NotRecorded {
				return b.NotRecorded
			}
			if a.Counts.Total() != b.Counts.Total() {
				return a.Counts.Total() > b.Counts.Total()
			}
			return a.Upstream < b.Upstream
		})
		out = append(out, sb)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Unattributed != b.Unattributed {
			return b.Unattributed
		}
		if a.Counts.Total() != b.Counts.Total() {
			return a.Counts.Total() > b.Counts.Total()
		}
		return a.Lane < b.Lane
	})
	return out, nil
}
