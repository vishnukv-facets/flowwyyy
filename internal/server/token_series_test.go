package server

import (
	"fmt"
	"testing"
	"time"
)

// Claude assistant transcript line with the given timestamp and token usage.
func usageLine(ts string, input, cacheRead, output int) []byte {
	return fmt.Appendf(nil,
		`{"type":"assistant","timestamp":%q,"message":{"model":"claude-opus-4-8",`+
			`"usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d}}}`,
		ts, input, cacheRead, output)
}

func TestAccumulateTranscriptUsageBucketsTokensByDay(t *testing.T) {
	var stats transcriptUsageStats
	// Two turns at the SAME instant (fresh 150 + 30 = 180) — identical timestamps
	// land on the same local day in any timezone — and one a full week later
	// (fresh 90), which is always a distinct local day. cache_read_input_tokens
	// is large but must be EXCLUDED from the per-day token total (processedTokens).
	const sameDay = "2026-06-01T12:00:00Z"
	const weekLater = "2026-06-08T12:00:00Z"
	accumulateTranscriptUsage(&stats, usageLine(sameDay, 100, 50000, 50))
	accumulateTranscriptUsage(&stats, usageLine(sameDay, 20, 90000, 10))
	accumulateTranscriptUsage(&stats, usageLine(weekLater, 40, 70000, 50))

	dayA := localDay(sameDay)
	dayB := localDay(weekLater)
	if dayA == "" || dayB == "" {
		t.Fatal("localDay returned empty for valid timestamps")
	}
	if dayA == dayB {
		t.Fatalf("timestamps a week apart must be different local days, both %s", dayA)
	}
	if len(stats.TokensByDay) != 2 {
		t.Errorf("TokensByDay should have 2 days, got %d: %v", len(stats.TokensByDay), stats.TokensByDay)
	}
	if got := stats.TokensByDay[dayA]; got != 180 {
		t.Errorf("dayA (%s): got %d fresh tokens, want 180 (cache reads excluded)", dayA, got)
	}
	if got := stats.TokensByDay[dayB]; got != 90 {
		t.Errorf("dayB (%s): got %d fresh tokens, want 90", dayB, got)
	}
	// Sanity: the per-day total reconciles with the cumulative session total.
	if stats.TokensSession != 270 {
		t.Errorf("TokensSession: got %d, want 270 (180+90)", stats.TokensSession)
	}
}

func codexUsageLine(ts string, input, cached, output, reasoning int) []byte {
	return fmt.Appendf(nil,
		`{"type":"event_msg","timestamp":%q,"payload":{"type":"token_count",`+
			`"info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,`+
			`"output_tokens":%d,"reasoning_output_tokens":%d}}}}`,
		ts, input, cached, output, reasoning)
}

func TestAccumulateTranscriptUsageBucketsCodexTokenDeltasByLocalDay(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.FixedZone("IST", 5*60*60+30*60)
	defer func() { time.Local = oldLocal }()

	var stats transcriptUsageStats
	// 18:00Z is 23:30 on Jun 5 IST; fresh total = (200-100)+20 = 120.
	accumulateTranscriptUsage(&stats, codexUsageLine("2026-06-05T18:00:00Z", 200, 100, 20, 0))
	// 18:35Z is 00:05 on Jun 6 IST; fresh total = (500-250)+100+10 = 360,
	// so only the +240 delta belongs to Jun 6.
	accumulateTranscriptUsage(&stats, codexUsageLine("2026-06-05T18:35:00Z", 500, 250, 100, 10))
	// Another Jun 6 event; fresh total = (900-500)+150+10 = 560, delta +200.
	accumulateTranscriptUsage(&stats, codexUsageLine("2026-06-05T19:00:00Z", 900, 500, 150, 10))

	if got := stats.TokensSession; got != 560 {
		t.Fatalf("TokensSession = %d, want latest Codex fresh total 560", got)
	}
	if got := stats.TokensByDay["2026-06-05"]; got != 120 {
		t.Errorf("Jun 5 local tokens = %d, want 120", got)
	}
	if got := stats.TokensByDay["2026-06-06"]; got != 440 {
		t.Errorf("Jun 6 local tokens = %d, want 440", got)
	}
}

func TestAccumulateTranscriptUsageSkipsZeroAndUnparseableTimestamps(t *testing.T) {
	var stats transcriptUsageStats
	// Zero fresh work (all input was cache reads, no output) → no day entry.
	accumulateTranscriptUsage(&stats, usageLine("2026-06-01T09:00:00Z", 0, 50000, 0))
	// Missing timestamp but real work → counted in session total, not bucketed.
	accumulateTranscriptUsage(&stats, []byte(`{"type":"assistant","message":{"usage":{"input_tokens":100,"output_tokens":50}}}`))

	if len(stats.TokensByDay) != 0 {
		t.Errorf("TokensByDay should be empty (zero-work + undated turns), got %v", stats.TokensByDay)
	}
	if stats.TokensSession != 150 {
		t.Errorf("TokensSession: got %d, want 150 (undated turn still counts to session)", stats.TokensSession)
	}
}

func TestLocalDayRejectsGarbage(t *testing.T) {
	if got := localDay(""); got != "" {
		t.Errorf("localDay(empty): got %q, want empty", got)
	}
	if got := localDay("not-a-timestamp"); got != "" {
		t.Errorf("localDay(garbage): got %q, want empty", got)
	}
}

func TestBuildUIStatsUsesTokenSeriesForStreaks(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local)
	tokenSeries := []uiTokenDay{
		{Date: "2026-06-12", Tokens: 100},
		{Date: "2026-06-13", Tokens: 100},
		{Date: "2026-06-14", Tokens: 100},
		{Date: "2026-06-15", Tokens: 0},
	}

	stats := buildUIStats(nil, nil, nil, tokenSeries, now)

	if stats.ActiveDays != 3 {
		t.Errorf("ActiveDays = %d, want 3 token-active days", stats.ActiveDays)
	}
	if stats.LongestStreak != 3 {
		t.Errorf("LongestStreak = %d, want 3 token-active days", stats.LongestStreak)
	}
	if stats.CurrentStreak != 3 {
		t.Errorf("CurrentStreak = %d, want 3 with untouched-today grace", stats.CurrentStreak)
	}
}

// GAP-12: origin="steerer" chats are attributed to the Steering slice (a subset of
// the totals + the correct provider bucket), while UI/Slack chats are not.
func TestBuildUIStatsSteeringSlice(t *testing.T) {
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.Local)
	chats := []uiAgent{
		{Slug: "chat-steer-c1", Provider: "claude", Origin: "steerer", TokensSession: 1000, CostSession: 0.50},
		{Slug: "chat-steer-c2", Provider: "codex", Origin: "steerer", TokensSession: 400, CostSession: 0.10},
		{Slug: "overview-ui1", Provider: "claude", Origin: "ui", TokensSession: 200, CostSession: 0.05},
	}
	stats := buildUIStats(nil, nil, chats, nil, now)

	if stats.TokensSteering != 1400 {
		t.Errorf("TokensSteering = %d, want 1400 (steerer chats only)", stats.TokensSteering)
	}
	if stats.SessionsSteering != 2 {
		t.Errorf("SessionsSteering = %d, want 2", stats.SessionsSteering)
	}
	// Steering is a subset of the totals (the UI chat counts too), not additive.
	if stats.TokensTotal != 1600 {
		t.Errorf("TokensTotal = %d, want 1600 (all three chats)", stats.TokensTotal)
	}
	// Provider split still holds across the steerer + UI chats.
	if stats.TokensCodex != 400 || stats.TokensClaude != 1200 {
		t.Errorf("provider split = claude %d / codex %d, want 1200 / 400", stats.TokensClaude, stats.TokensCodex)
	}
}
