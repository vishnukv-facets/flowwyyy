package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"flow/internal/flowdb"
)

func kpiByKey(p AnalyticsPayload, key string) Kpi {
	for _, k := range p.KPIs {
		if k.Key == key {
			return k
		}
	}
	return Kpi{}
}

func hasSeries(p AnalyticsPayload, key string) bool {
	for _, s := range p.Series {
		if s.Key == key {
			return true
		}
	}
	return false
}

func TestParseAnalyticsRange(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)

	q, err := parseAnalyticsRange(url.Values{"range": {"7d"}}, now)
	if err != nil {
		t.Fatalf("range=7d: %v", err)
	}
	if q.Range != "7d" || q.Unit != bucketDay || !q.To.Equal(now) {
		t.Errorf("range=7d parsed wrong: %+v", q)
	}

	// A 1-day custom span picks hourly buckets.
	q, err = parseAnalyticsRange(url.Values{"from": {"2026-06-18T00:00:00Z"}, "to": {"2026-06-19T00:00:00Z"}}, now)
	if err != nil {
		t.Fatalf("custom: %v", err)
	}
	if q.Unit != bucketHour {
		t.Errorf("24h custom span unit=%s want hour", q.Unit)
	}

	if _, err := parseAnalyticsRange(url.Values{"range": {"bogus"}}, now); err == nil {
		t.Errorf("bogus range should error")
	}
	if _, err := parseAnalyticsRange(url.Values{"from": {"2026-06-19T00:00:00Z"}, "to": {"2026-06-18T00:00:00Z"}}, now); err == nil {
		t.Errorf("from after to should error")
	}

	q, _ = parseAnalyticsRange(url.Values{}, now)
	if q.Range != "7d" {
		t.Errorf("default range=%s want 7d", q.Range)
	}
}

func TestBuildAnalyticsPayloadActivity(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	q, _ := parseAnalyticsRange(url.Values{"range": {"7d"}}, now)

	tasks := []*flowdb.Task{
		mkTask("a", "done", atDay(2026, 6, 15), atDay(2026, 6, 16)), // done in window
		mkTask("b", "done", atDay(2026, 6, 17), atDay(2026, 6, 18)), // done in window
		mkTask("c", "done", atDay(2026, 6, 8), atDay(2026, 6, 9)),   // done in the previous 7d window
		mkTask("d", "in-progress", atDay(2026, 6, 15), ""),
	}

	p := buildAnalyticsPayload(analyticsInputs{tasks: tasks, usages: tokenUsagesFixture()}, q, now)
	if p.Bucket != bucketDay {
		t.Errorf("bucket=%s want day", p.Bucket)
	}
	if !p.Partial {
		t.Errorf("expected partial (window ends at now)")
	}
	if len(p.Series) == 0 || p.Series[0].Key != "throughput" {
		t.Fatalf("missing throughput series: %+v", p.Series)
	}
	if p.From == "" || p.To == "" || p.GeneratedAt == "" {
		t.Errorf("missing time fields: from=%q to=%q gen=%q", p.From, p.To, p.GeneratedAt)
	}

	done := kpiByKey(p, "tasks_done")
	if done.Value != 2 {
		t.Errorf("tasks_done=%v want 2 (a,b)", done.Value)
	}
	if done.DeltaPct == nil || *done.DeltaPct != 100 {
		t.Errorf("tasks_done delta=%v want +100 (2 vs 1 prior)", done.DeltaPct)
	}
	if kpiByKey(p, "cycle_time_median").Key == "" {
		t.Errorf("expected cycle_time_median KPI (a,b completed in window)")
	}

	// Token usages (fixture) flow through into KPIs, series, and the model mix.
	if tok := kpiByKey(p, "tokens"); tok.Value != 3500 {
		t.Errorf("tokens KPI=%v want 3500", tok.Value)
	}
	if c := kpiByKey(p, "cost_usd"); c.Value != 1.75 {
		t.Errorf("cost_usd KPI=%v want 1.75", c.Value)
	}
	if !hasSeries(p, "tokens") || !hasSeries(p, "cost") {
		t.Errorf("expected tokens + cost series on a daily grid")
	}
	if len(p.Breakdowns) == 0 || p.Breakdowns[0].Key != "model_mix" {
		t.Errorf("expected model_mix breakdown, got %+v", p.Breakdowns)
	}
}

func TestBuildAnalyticsPayloadDomains(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	q, _ := parseAnalyticsRange(url.Values{"range": {"7d"}}, now)

	in := analyticsInputs{
		tasks:  []*flowdb.Task{mkTask("a", "done", atDay(2026, 6, 15), atDay(2026, 6, 16))},
		usages: tokenUsagesFixture(),
		runs: []*flowdb.BrainRun{
			mkRun("r1", "completed", "2026-06-15T10:00:00Z", "2026-06-15T10:10:00Z"),
			mkRun("r2", "dead", "2026-06-17T09:00:00Z", "2026-06-17T09:50:00Z"),
		},
		traces: steeringFixture(),
	}
	p := buildAnalyticsPayload(in, q, now)

	// Autonomy domain present.
	if kpiByKey(p, "runs").Value != 2 {
		t.Errorf("runs KPI = %v want 2 (both started in window)", kpiByKey(p, "runs").Value)
	}
	if kpiByKey(p, "run_success").Key == "" {
		t.Errorf("expected run_success KPI")
	}
	if !hasSeries(p, "runs") {
		t.Errorf("expected runs series")
	}

	// Steering domain present.
	if !hasSeries(p, "steering") || !hasSeries(p, "steering_latency") {
		t.Errorf("expected steering + steering_latency series")
	}
	if p.Funnel == nil || p.Funnel.Observed != 4 {
		t.Errorf("expected funnel Observed=4, got %+v", p.Funnel)
	}
	if p.Funnel.Surfaced != 2 {
		t.Errorf("funnel Surfaced = %d want 2", p.Funnel.Surfaced)
	}
}

// The live endpoint must serve an empty window as a valid zero-filled payload,
// never an error, with delta omitted (nil) against an empty prior period.
func TestAnalyticsEndpointEmptyWindow(t *testing.T) {
	root, db := testRootDB(t)
	srv := authedTestHandler(New(Config{DB: db, FlowRoot: root, Version: "test"}))

	req := httptest.NewRequest(http.MethodGet, "/api/analytics?range=7d", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var p AnalyticsPayload
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if p.Bucket != bucketDay {
		t.Errorf("bucket=%s want day", p.Bucket)
	}
	if len(p.Series) == 0 || p.Series[0].Key != "throughput" {
		t.Fatalf("missing throughput series")
	}
	for _, pt := range p.Series[0].Lines[0].Points {
		if pt.V != 0 {
			t.Errorf("empty-window point should be 0, got %v at %s", pt.V, pt.T)
		}
	}
	done := kpiByKey(p, "tasks_done")
	if done.Value != 0 {
		t.Errorf("tasks_done=%v want 0", done.Value)
	}
	if done.DeltaPct != nil {
		t.Errorf("delta should be nil when prior window empty, got %v", *done.DeltaPct)
	}
}
