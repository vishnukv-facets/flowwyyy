package server

import (
	"math"
	"testing"
)

func ratesClose(a, b float64) bool {
	return math.Abs(a-b) < 1e-12
}

func TestModelTokenRate(t *testing.T) {
	const m = 1_000_000.0
	cases := []struct {
		model   string
		wantIn  float64
		wantOut float64
	}{
		// Claude families, matched by family token regardless of suffix.
		{"claude-opus-4-8", 5.0 / m, 25.0 / m},
		{"claude-opus-4-8[1m]", 5.0 / m, 25.0 / m},
		{"claude-sonnet-4-6", 3.0 / m, 15.0 / m},
		{"claude-haiku-4-5-20251001", 1.0 / m, 5.0 / m},
		// Codex / OpenAI families. gpt-5.5 must win over the broader gpt-5.
		{"gpt-5.5", 5.0 / m, 30.0 / m},
		{"gpt-5", 1.75 / m, 14.0 / m},
		{"gpt-5-codex", 1.75 / m, 14.0 / m},
		{"gpt-5.1-codex-max", 1.75 / m, 14.0 / m},
		// Unknown / empty models contribute $0 rather than a fabricated rate.
		{"", 0, 0},
		{"some-future-model", 0, 0},
	}
	for _, c := range cases {
		got := modelTokenRate(c.model)
		if !ratesClose(got.inputPerToken, c.wantIn) || !ratesClose(got.outputPerToken, c.wantOut) {
			t.Errorf("modelTokenRate(%q) = {in:%g out:%g}, want {in:%g out:%g}",
				c.model, got.inputPerToken, got.outputPerToken, c.wantIn, c.wantOut)
		}
	}
}

// Claude turns carry message.model, so cost is priced per-turn from that model's
// rate. cache_read tokens are excluded from the work basis AND from cost (the
// estimate is the cost of fresh work, consistent with the token figures).
func TestAccumulateTranscriptUsageBucketsClaudeCostByDay(t *testing.T) {
	var stats transcriptUsageStats
	const day = "2026-06-01T12:00:00Z"
	// opus-4-8 = $5/MTok in, $25/MTok out. 1M fresh input + 1M output = $30.
	// cache_read of 9M is excluded from both tokens and cost.
	accumulateTranscriptUsage(&stats, usageLine(day, 1_000_000, 9_000_000, 1_000_000))

	d := localDay(day)
	if got := stats.CostByDay[d]; !ratesClose(got, 30.0) {
		t.Errorf("CostByDay[%s] = %g, want 30.00 (cache reads excluded)", d, got)
	}
}

// Codex token_count events carry no model; the model comes from a preceding
// turn_context record. Cost uses the input/output split of the running-total
// delta so the higher Codex output rate is applied correctly.
func TestAccumulateTranscriptUsageBucketsCodexCostByDay(t *testing.T) {
	var stats transcriptUsageStats
	const day = "2026-06-01T12:00:00Z"
	// turn_context sets the active model.
	accumulateTranscriptUsage(&stats, []byte(`{"type":"turn_context","timestamp":"`+day+`","payload":{"model":"gpt-5.5"}}`))
	// gpt-5.5 = $5/MTok in, $30/MTok out. First running total: fresh in = 1M
	// (2M input - 1M cached), fresh out = 1M → $5 + $30 = $35.
	accumulateTranscriptUsage(&stats, codexUsageLine(day, 2_000_000, 1_000_000, 1_000_000, 0))

	d := localDay(day)
	if got := stats.CostByDay[d]; !ratesClose(got, 35.0) {
		t.Errorf("CostByDay[%s] = %g, want 35.00", d, got)
	}
}

// An unknown model still counts tokens but must not fabricate a dollar cost.
func TestAccumulateTranscriptUsageUnknownModelHasNoCost(t *testing.T) {
	var stats transcriptUsageStats
	const day = "2026-06-01T12:00:00Z"
	accumulateTranscriptUsage(&stats, []byte(
		`{"type":"assistant","timestamp":"`+day+`","message":{"model":"mystery-model","usage":{"input_tokens":1000,"output_tokens":500}}}`))

	d := localDay(day)
	if got := stats.TokensByDay[d]; got != 1500 {
		t.Errorf("TokensByDay[%s] = %d, want 1500 (tokens still counted)", d, got)
	}
	if got, ok := stats.CostByDay[d]; ok && got != 0 {
		t.Errorf("CostByDay[%s] = %g, want no entry / 0 for unknown model", d, got)
	}
}
