package reports

// BucketPredicateForTest exposes a bucket's SQL predicate to the external test
// package so query-plan tests judge the real SQL, not a hand-copied one.
func BucketPredicateForTest(b Bucket) string { return bucketPredicates[b] }

// SourceLabelExprForTest exposes the generated Source-label CASE.
func SourceLabelExprForTest() string { return sourceLabelExpr }
