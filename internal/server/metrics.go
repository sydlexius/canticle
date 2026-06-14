package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/sydlexius/mxlrcgo-svc/internal/auth"
)

// MetricsReporter provides aggregate queue data for the GET /metrics endpoint.
type MetricsReporter interface {
	CountByStatus(ctx context.Context) (map[string]int64, error)
	CountFailuresByReason(ctx context.Context) (map[string]int64, error)
}

// handleMetrics writes a Prometheus text-exposition response. It applies the
// same admin-scope auth gate as handleStatus so operational data is not exposed
// to unauthenticated callers. Metrics are computed from read-only DB queries at
// request time (query-on-scrape); there is no in-process registry or caching.
func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		http.Error(w, "auth unavailable", http.StatusInternalServerError)
		return
	}
	if _, err := h.auth.ValidateKey(r.Context(), apiKey(r), auth.ScopeAdmin); err != nil {
		switch {
		case errors.Is(err, auth.ErrForbiddenScope):
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		case errors.Is(err, auth.ErrInvalidKey), errors.Is(err, auth.ErrRevokedKey):
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		default:
			slog.Error("metrics authentication failed", "error", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
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

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	writeMetrics(w, statusCounts, failureCounts)
}

// writeMetrics serializes all metric families in Prometheus text-exposition
// format (version 0.0.4). Each family includes the required # HELP and # TYPE
// comment lines followed by one sample line per label set. Label sets are
// sorted to ensure a deterministic response order. All metrics use gauge
// semantics because they are computed from DB snapshots, not monotonic counters.
func writeMetrics(w io.Writer, statusCounts, failureCounts map[string]int64) {
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
