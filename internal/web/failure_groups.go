package web

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/sydlexius/canticle/internal/failsig"
	"github.com/sydlexius/canticle/web/templates"
)

// failureGroupPageSize is how many rows one expanded failure group lists. One
// extra row is fetched to learn whether the group holds more.
const failureGroupPageSize = 100

// registerFailureGroupRoutes registers the failure-group expansion fragment
// through reg, so it is guarded exactly like every other page route.
func (u *UI) registerFailureGroupRoutes(reg routeReg) {
	reg("GET /reports/failure-group", u.handleFailureGroup)
}

// groupClass is the badge class of a Failure Analysis group. Only failed groups
// carry one: a deferred group is a provider miss the worker already retries on a
// schedule, so failsig (which reads failed-row errors) would mislabel it. This
// mirrors reports.FailureGroupItems, which leaves a deferred item's Class empty.
func groupClass(status, reason string) failsig.Class {
	if status != "failed" {
		return ""
	}
	return failsig.Classify(reason)
}

// handleFailureGroup renders one group's rows as an htmx fragment. Rows load
// only when a group is expanded: each call scans the whole status. The
// signature is a query value because it is arbitrary text.
func (u *UI) handleFailureGroup(w http.ResponseWriter, r *http.Request) {
	// Rows carry artist/title; never let a browser or proxy cache them.
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	status, signature := q.Get("status"), q.Get("signature")
	if status != "failed" && status != "deferred" {
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	}
	if u.reports == nil {
		slog.Error("reports repo not wired; cannot serve failure group")
		http.Error(w, "reports data source unavailable", http.StatusServiceUnavailable)
		return
	}
	items, err := u.reports.FailureGroupItems(r.Context(), status, signature, failureGroupPageSize+1)
	if err != nil {
		// The error text can embed the signature, which is library-derived; log
		// the status only.
		slog.Error("failure group query failed", "status", status)
		http.Error(w, "failure group query failed", http.StatusInternalServerError)
		return
	}
	view := templates.FailureGroupView{Truncated: len(items) > failureGroupPageSize}
	if view.Truncated {
		items = items[:failureGroupPageSize]
	}
	for _, it := range items {
		view.Rows = append(view.Rows, templates.FailureItemRow{
			Artist:        it.Artist,
			Title:         it.Title,
			Album:         it.Album,
			NextAttemptAt: formatQueueTime(it.NextAttemptAt),
			MissCount:     strconv.FormatInt(it.MissCount, 10),
			Attempts:      strconv.FormatInt(it.Attempts, 10),
			UpdatedAt:     formatQueueTime(it.UpdatedAt),
		})
	}
	render(w, r, templates.FailureGroupRows(view))
}
