package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/reports"
)

// TestFinishedWordChip pins the #1405 shape under both rungs: Finished lists the
// hand-marked row, the Word-synced chip is shown active on the tile's link and
// removes it, clearing the chip shows it again, and Finished + Settled = Done.
func TestFinishedWordChip(t *testing.T) {
	for _, top := range []reports.TopRung{reports.TopRungWord, reports.TopRungLine} {
		t.Run(fmt.Sprint(top), func(t *testing.T) {
			sqlDB := openReportsTestDB(t)
			seedLinkPopulation(t, sqlDB)
			repo := reports.New(sqlDB, reports.WithLineTopRung(top == reports.TopRungLine))
			mux := http.NewServeMux()
			NewUI(config.Config{}, "v-test", WithReports(repo)).Register(mux)

			all := getQueue(t, mux, "/queue/finished", false).Body.String()
			if !strings.Contains(all, "m1") {
				t.Fatal("unfiltered Finished does not list the hand-marked row")
			}
			rec := getQueue(t, mux, "/queue/finished?word=1", false)
			body := rec.Body.String()
			if strings.Contains(body, "m1") {
				t.Error("word=1 still lists the hand-marked row")
			}
			chip, ok := chipsOf(body)["Word-synced"]
			if !ok || chip[1] == "" || chip[3] == "" {
				t.Fatalf("Word-synced chip missing or not active: %v", chip)
			}
			if q := chipQuery(t, chip); q.Get("word") != "" {
				t.Errorf("active chip link %q does not clear the filter", chip[2])
			}
			// Mutual exclusion with the line tier: the line chip's link drops word.
			if top == reports.TopRungLine {
				if q := chipQuery(t, chipsOf(body)["Line-synced (editable)"]); q.Get("word") != "" || q.Get("tier") != "line" {
					t.Errorf("Line-synced link keeps word: %v", q)
				}
			}
			q, err := repo.QueueSummary(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var done int64
			if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM work_queue WHERE status = 'done'`).Scan(&done); err != nil {
				t.Fatal(err)
			}
			if q.Finished+q.SettledUpgradable != done || q.Done != done {
				t.Errorf("Finished %d + Settled %d != Done %d (%d)", q.Finished, q.SettledUpgradable, q.Done, done)
			}
			if n := listFromHref(t, repo, top, "/queue/finished"); strconv.FormatInt(q.Finished, 10) != strconv.Itoa(n) {
				t.Errorf("Finished lists %d rows, summary %d", n, q.Finished)
			}
		})
	}
}
