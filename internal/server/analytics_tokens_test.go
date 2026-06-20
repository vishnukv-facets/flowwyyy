package server

import (
	"testing"
	"time"
)

func tokenUsagesFixture() []taskTokenUsage {
	return []taskTokenUsage{
		{
			Provider:    "claude",
			Model:       "sonnet",
			TokensByDay: map[string]int{"2026-06-14": 1000, "2026-06-16": 2000, "2026-06-01": 999}, // 6/01 out of window
			CostByDay:   map[string]float64{"2026-06-14": 0.5, "2026-06-16": 1.0},
		},
		{
			Provider:    "codex",
			Model:       "gpt-5.4",
			TokensByDay: map[string]int{"2026-06-14": 500},
			CostByDay:   map[string]float64{"2026-06-14": 0.25},
		},
	}
}

func TestAggregateTokens(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	from, to, unit, _ := rangeWindow(now, "7d") // daily 6/13..6/20
	g := bucketsFor(from, to, unit, now)

	agg := aggregateTokens(tokenUsagesFixture(), g)

	if agg.tokenTotal != 3500 {
		t.Errorf("tokenTotal=%v want 3500 (1000+2000+500; 999 is out of window)", agg.tokenTotal)
	}
	if agg.costTotal != 1.75 {
		t.Errorf("costTotal=%v want 1.75", agg.costTotal)
	}

	i := g.indexOf(parseDayLocal("2026-06-14"))
	if i < 0 {
		t.Fatal("6/14 should be in the 7d grid")
	}
	if got := agg.tokensByProvider["claude"][i]; got != 1000 {
		t.Errorf("claude tokens 6/14 = %v want 1000", got)
	}
	if got := agg.tokensByProvider["codex"][i]; got != 500 {
		t.Errorf("codex tokens 6/14 = %v want 500", got)
	}
	if agg.modelTokens["sonnet"] != 3000 {
		t.Errorf("sonnet model tokens=%v want 3000", agg.modelTokens["sonnet"])
	}
	if agg.modelTokens["gpt-5.4"] != 500 {
		t.Errorf("gpt-5.4 model tokens=%v want 500", agg.modelTokens["gpt-5.4"])
	}
}

func TestStackedSeriesAndModelBreakdown(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.Local)
	from, to, unit, _ := rangeWindow(now, "7d")
	g := bucketsFor(from, to, unit, now)
	agg := aggregateTokens(tokenUsagesFixture(), g)

	cost := stackedSeries("cost", "Token cost", "usd", agg.costByProvider, g)
	if cost.Key != "cost" || !cost.Stacked {
		t.Errorf("cost series key=%s stacked=%v want cost/true", cost.Key, cost.Stacked)
	}
	if lineByKey(cost, "claude").Key == "" || lineByKey(cost, "codex").Key == "" {
		t.Errorf("cost series missing provider lines: %+v", cost.Lines)
	}
	// Each provider line has one point per bucket.
	if n := len(lineByKey(cost, "claude").Points); n != g.Len() {
		t.Errorf("claude points=%d want %d", n, g.Len())
	}

	b := modelBreakdown(agg)
	if b.Key != "model_mix" {
		t.Errorf("breakdown key=%s want model_mix", b.Key)
	}
	var sum float64
	for _, seg := range b.Segments {
		sum += seg.Value
	}
	if sum != agg.tokenTotal {
		t.Errorf("model segments sum=%v want %v (total tokens)", sum, agg.tokenTotal)
	}
}
