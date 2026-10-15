package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/repair"
	"github.com/sydlexius/canticle/internal/web"
)

type wiringRepair struct{}

func (wiringRepair) Info() repair.Info {
	return repair.Info{Key: "wire-fix", Title: "Wire fix"}
}
func (wiringRepair) Pending(context.Context, repair.Options) (int64, error) { return 3, nil }
func (wiringRepair) Run(context.Context, repair.Options) (repair.Result, error) {
	return repair.NewResult(), nil
}

// TestWithRepairsWiring proves the option reaches the mounted web UI: attached
// with a registered repair the confirm route answers, while omitting it, or
// passing a nil registry or runner, leaves the route answering 404.
func TestWithRepairsWiring(t *testing.T) {
	reg := repair.NewRegistry()
	if err := reg.Register(wiringRepair{}); err != nil {
		t.Fatal(err)
	}
	runner := repair.NewRunner(reg, nil)
	cases := []struct {
		name string
		opts []Option
		want int
	}{
		{"wired", []Option{WithRepairs(web.RepairDeps{Registry: reg, Runner: runner})}, http.StatusOK},
		{"option omitted", nil, http.StatusNotFound},
		{"nil registry", []Option{WithRepairs(web.RepairDeps{Runner: runner})}, http.StatusNotFound},
		{"nil runner", []Option{WithRepairs(web.RepairDeps{Registry: reg})}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]Option{WithWebUI(config.Config{}, "vtest")}, tc.opts...)
			h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", opts...)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repairs/wire-fix", nil))
			if rec.Code != tc.want {
				t.Fatalf("GET /repairs/wire-fix = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
