package reports

import (
	"context"
	"time"
)

// BucketPredicateForTest exposes a bucket's SQL predicate to the external test
// package so query-plan tests judge the real SQL, not a hand-copied one.
func BucketPredicateForTest(b Bucket) string { return bucketPredicates[b] }

// SourceLabelExprForTest exposes the generated Source-label CASE.
func SourceLabelExprForTest() string { return sourceLabelExpr }

// SourceEventsForTest exposes the unscoped counter read (lane "") so its range
// and ordering contract stays pinned.
func SourceEventsForTest(r *Repo, ctx context.Context, from, to time.Time) ([]SourceEventCount, error) {
	return r.sourceEvents(ctx, from, to, "")
}
