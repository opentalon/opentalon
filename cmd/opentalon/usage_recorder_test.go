package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/metrics"
	"github.com/opentalon/opentalon/internal/state/store"
)

func TestUsageRecorderAdapterRecordsTheGivenCost(t *testing.T) {
	db, err := store.Open(config.DBConfig{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	c := metrics.New()
	a := &usageRecorderAdapter{store: store.NewUsageStore(db), collector: c}

	a.RecordUsage(context.Background(), "ent1", "g1", "web", "s1", "m", "chat", "", 2100, 50, 1, 0.75, 1.125)

	var in, out float64
	row := db.SQLDB().QueryRow(`SELECT input_cost, output_cost FROM profile_usage WHERE session_id = 's1'`)
	if err := row.Scan(&in, &out); err != nil || in != 0.75 || out != 1.125 {
		t.Errorf("usage row cost = %v/%v (%v), want 0.75/1.125", in, out, err)
	}
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`opentalon_llm_input_cost_usd_total{channel="web",entity_id="ent1",group="g1",model="m"} 0.75`,
		`opentalon_llm_output_cost_usd_total{channel="web",entity_id="ent1",group="g1",model="m"} 1.125`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
}
