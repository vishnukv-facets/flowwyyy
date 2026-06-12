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
		{"claude-fable-5", 10.0 / m, 50.0 / m},
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
// rate. Unlike the work-token metric, cost is the FULL bill: cache reads are
// included at 0.1x the input rate.
func TestAccumulateTranscriptUsageBucketsClaudeCostByDay(t *testing.T) {
	var stats transcriptUsageStats
	const day = "2026-06-01T12:00:00Z"
	// opus-4-8 = $5/MTok in, $25/MTok out, cache read = $0.50/MTok (0.1x in).
	// 1M fresh input ($5) + 1M output ($25) + 9M cache_read ($4.50) = $34.50.
	accumulateTranscriptUsage(&stats, usageLine(day, 1_000_000, 9_000_000, 1_000_000))

	d := localDay(day)
	if got := stats.CostByDay[d]; !ratesClose(got, 34.5) {
		t.Errorf("CostByDay[%s] = %g, want 34.50 (cache reads billed at 0.1x)", d, got)
	}
}

// Cache-creation tokens bill at a premium on the input rate: 1.25x for the
// 5-minute TTL, 2x for the 1-hour TTL. The breakdown must be priced separately.
func TestAccumulateTranscriptUsageClaudeCacheCreationCost(t *testing.T) {
	var stats transcriptUsageStats
	const day = "2026-06-01T12:00:00Z"
	// opus-4-8 in=$5/MTok. 1M output ($25) + 2M 5m-creation (2M×$6.25/MTok=$12.50)
	// + 1M 1h-creation (1M×$10/MTok=$10) = $47.50. No fresh input, no cache reads.
	line := []byte(`{"type":"assistant","timestamp":"` + day + `","requestId":"req_1",` +
		`"message":{"id":"msg_1","model":"claude-opus-4-8","usage":{` +
		`"output_tokens":1000000,"cache_creation_input_tokens":3000000,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":2000000,"ephemeral_1h_input_tokens":1000000}}}}`)
	accumulateTranscriptUsage(&stats, line)

	d := localDay(day)
	if got := stats.CostByDay[d]; !ratesClose(got, 47.5) {
		t.Errorf("CostByDay[%s] = %g, want 47.50 (5m@1.25x + 1h@2x)", d, got)
	}
}

// Claude Code writes the SAME request's usage to the jsonl multiple times
// (intermediate + final snapshots, identical counts). Each request must be
// counted once for both work tokens and cost — keyed by (message.id, requestId).
func TestAccumulateTranscriptUsageDedupsClaudeSnapshots(t *testing.T) {
	var stats transcriptUsageStats
	const day = "2026-06-01T12:00:00Z"
	line := []byte(`{"type":"assistant","timestamp":"` + day + `","requestId":"req_A",` +
		`"message":{"id":"msg_A","model":"claude-opus-4-8","usage":{` +
		`"input_tokens":1000000,"output_tokens":1000000}}}`)
	// Same (message.id, requestId) three times — a final snapshot plus two repeats.
	accumulateTranscriptUsage(&stats, line)
	accumulateTranscriptUsage(&stats, line)
	accumulateTranscriptUsage(&stats, line)
	// A genuinely different request must still count.
	other := []byte(`{"type":"assistant","timestamp":"` + day + `","requestId":"req_B",` +
		`"message":{"id":"msg_B","model":"claude-opus-4-8","usage":{` +
		`"input_tokens":500000,"output_tokens":0}}}`)
	accumulateTranscriptUsage(&stats, other)

	// Work tokens: 2M (req_A, once) + 0.5M (req_B) = 2.5M, NOT 6.5M.
	if got := stats.TokensSession; got != 2_500_000 {
		t.Errorf("TokensSession = %d, want 2,500,000 (req_A counted once)", got)
	}
	// Cost: req_A $30 (1M in + 1M out, once) + req_B $2.50 (0.5M in) = $32.50.
	d := localDay(day)
	if got := stats.CostByDay[d]; !ratesClose(got, 32.5) {
		t.Errorf("CostByDay[%s] = %g, want 32.50 (duplicate snapshots not double-counted)", d, got)
	}
}

// Codex token_count events carry no model; the model comes from a preceding
// turn_context record. Cost uses the input/output split of the running-total
// delta so the higher Codex output rate is applied correctly, plus the
// cached-input portion billed at the cache-read rate.
func TestAccumulateTranscriptUsageBucketsCodexCostByDay(t *testing.T) {
	var stats transcriptUsageStats
	const day = "2026-06-01T12:00:00Z"
	// turn_context sets the active model.
	accumulateTranscriptUsage(&stats, []byte(`{"type":"turn_context","timestamp":"`+day+`","payload":{"model":"gpt-5.5"}}`))
	// gpt-5.5 = $5/MTok in, $30/MTok out, cached = $0.50/MTok (0.1x in). First
	// running total: fresh in = 1M (2M input - 1M cached) = $5, fresh out = 1M
	// = $30, cached 1M = $0.50 → $35.50.
	accumulateTranscriptUsage(&stats, codexUsageLine(day, 2_000_000, 1_000_000, 1_000_000, 0))

	d := localDay(day)
	if got := stats.CostByDay[d]; !ratesClose(got, 35.5) {
		t.Errorf("CostByDay[%s] = %g, want 35.50 (cached input billed at 0.1x)", d, got)
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
