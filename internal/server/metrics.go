package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
)

// MetricsReporter provides aggregate queue data for the GET /metrics endpoint.
type MetricsReporter interface {
	CountByStatus(ctx context.Context) (map[string]int64, error)
	CountFailuresByReason(ctx context.Context) (map[string]int64, error)
	ProviderHits(ctx context.Context) (map[string]int64, error)
	ProviderMisses(ctx context.Context) (map[string]int64, error)
	CountInstrumental(ctx context.Context) (int64, error)
}

// handleMetrics writes a Prometheus text-exposition response. Access is gated
// by the trusted-network allowlist (issue #204, S3), not by an API key or
// session: only a request whose resolved client IP is loopback or within a
// configured CIDR may scrape, and a spoofed X-Forwarded-For cannot forge a
// trusted source (see trustnet.ClientIP). Metrics are computed from read-only
// DB queries at request time (query-on-scrape); there is no in-process registry
// or caching.
func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !h.trusted.Trusted(r) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}

	if h.metrics == nil {
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}

	statusCounts, err := h.metrics.CountByStatus(r.Context())
	if err != nil {
		slog.Error("metrics: count by status failed", "error", err)
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}

	failureCounts, err := h.metrics.CountFailuresByReason(r.Context())
	if err != nil {
		slog.Error("metrics: count failures by reason failed", "error", err)
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}

	providerHits, err := h.metrics.ProviderHits(r.Context())
	if err != nil {
		slog.Error("metrics: provider hits failed", "error", err)
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}

	providerMisses, err := h.metrics.ProviderMisses(r.Context())
	if err != nil {
		slog.Error("metrics: provider misses failed", "error", err)
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}

	instrumentalCount, err := h.metrics.CountInstrumental(r.Context())
	if err != nil {
		slog.Error("metrics: count instrumental failed", "error", err)
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // prevent proxies from caching stale metrics
	w.WriteHeader(http.StatusOK)
	writeMetrics(w, statusCounts, failureCounts, providerHits, providerMisses, instrumentalCount)
}

// writeMetrics serializes all metric families in Prometheus text-exposition
// format (version 0.0.4). Each family includes the required # HELP and # TYPE
// comment lines followed by one sample line per label set. Label sets are
// sorted to ensure a deterministic response order. Queue-snapshot metrics use
// gauge semantics; provider outcome counters use counter semantics (_total suffix).
func writeMetrics(w io.Writer, statusCounts, failureCounts, providerHits, providerMisses map[string]int64, instrumentalCount int64) {
	_, _ = fmt.Fprintln(w, "# HELP mxlrcgo_queue_items Current number of items in the work queue by status.")
	_, _ = fmt.Fprintln(w, "# TYPE mxlrcgo_queue_items gauge")
	for _, status := range sortedKeys(statusCounts) {
		_, _ = fmt.Fprintf(w, "mxlrcgo_queue_items{status=\"%s\"} %d\n", promEscape(status), statusCounts[status])
	}

	_, _ = fmt.Fprintln(w, "# HELP mxlrcgo_queue_failures Current number of failed work queue items by error reason.")
	_, _ = fmt.Fprintln(w, "# TYPE mxlrcgo_queue_failures gauge")
	for _, reason := range sortedKeys(failureCounts) {
		_, _ = fmt.Fprintf(w, "mxlrcgo_queue_failures{reason=\"%s\"} %d\n", promEscape(reason), failureCounts[reason])
	}

	_, _ = fmt.Fprintln(w, "# HELP mxlrcgo_provider_hits_total Total number of successful lyrics fetches by provider lane.")
	_, _ = fmt.Fprintln(w, "# TYPE mxlrcgo_provider_hits_total counter")
	for _, lane := range sortedKeys(providerHits) {
		_, _ = fmt.Fprintf(w, "mxlrcgo_provider_hits_total{lane=\"%s\"} %d\n", promEscape(lane), providerHits[lane])
	}

	_, _ = fmt.Fprintln(w, "# HELP mxlrcgo_provider_misses_total Total number of benign no-result misses by provider lane.")
	_, _ = fmt.Fprintln(w, "# TYPE mxlrcgo_provider_misses_total counter")
	for _, lane := range sortedKeys(providerMisses) {
		_, _ = fmt.Fprintf(w, "mxlrcgo_provider_misses_total{lane=\"%s\"} %d\n", promEscape(lane), providerMisses[lane])
	}

	_, _ = fmt.Fprintln(w, "# HELP mxlrcgo_instrumental_tracks Number of work queue items confirmed instrumental by audio detection.")
	_, _ = fmt.Fprintln(w, "# TYPE mxlrcgo_instrumental_tracks gauge")
	_, _ = fmt.Fprintf(w, "mxlrcgo_instrumental_tracks %d\n", instrumentalCount)
}

// sortedKeys returns the keys of m in ascending lexicographic order.
func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// promEscape escapes a label value per the Prometheus text-exposition format
// spec: backslashes, double-quotes, and newlines must be escaped.
func promEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
