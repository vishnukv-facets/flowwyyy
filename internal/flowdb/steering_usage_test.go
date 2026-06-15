package flowdb

import "testing"

func TestRecordSteeringUsageAggregatesByKindAndDay(t *testing.T) {
	db := openTempDB(t)

	if err := RecordSteeringUsage(db, SteeringUsageDelta{
		Kind:                     "classifier",
		Day:                      "2026-06-15",
		InputTokens:              100,
		CacheCreationInputTokens: 20,
		CacheReadInputTokens:     1000,
		OutputTokens:             30,
		CostUSD:                  0.12,
	}); err != nil {
		t.Fatalf("RecordSteeringUsage first: %v", err)
	}
	if err := RecordSteeringUsage(db, SteeringUsageDelta{
		Kind:         "classifier",
		Day:          "2026-06-15",
		InputTokens:  50,
		OutputTokens: 10,
		CostUSD:      0.03,
	}); err != nil {
		t.Fatalf("RecordSteeringUsage second: %v", err)
	}
	if err := RecordSteeringUsage(db, SteeringUsageDelta{
		Kind:         "dream",
		Day:          "2026-06-16",
		InputTokens:  7,
		OutputTokens: 3,
		CostUSD:      0.01,
	}); err != nil {
		t.Fatalf("RecordSteeringUsage other day: %v", err)
	}

	var rowCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM steering_usage`).Scan(&rowCount); err != nil {
		t.Fatalf("count steering_usage: %v", err)
	}
	if rowCount != 2 {
		t.Fatalf("row count = %d, want 2", rowCount)
	}
	var got SteeringUsage
	err := db.QueryRow(`
		SELECT kind, day, runs, tokens, input_tokens, cache_creation_input_tokens,
			cache_read_input_tokens, output_tokens, cost_usd
		FROM steering_usage
		WHERE kind = 'classifier' AND day = '2026-06-15'
	`).Scan(&got.Kind, &got.Day, &got.Runs, &got.Tokens, &got.InputTokens, &got.CacheCreationInputTokens, &got.CacheReadInputTokens, &got.OutputTokens, &got.CostUSD)
	if err != nil {
		t.Fatalf("load classifier usage: %v", err)
	}
	if got.Kind != "classifier" || got.Day != "2026-06-15" {
		t.Fatalf("row = %+v, want classifier on 2026-06-15", got)
	}
	if got.Runs != 2 {
		t.Errorf("Runs = %d, want 2", got.Runs)
	}
	if got.Tokens != 210 {
		t.Errorf("Tokens = %d, want 210 (input + cache creation + output, cache reads excluded)", got.Tokens)
	}
	if got.InputTokens != 150 || got.CacheCreationInputTokens != 20 || got.CacheReadInputTokens != 1000 || got.OutputTokens != 40 {
		t.Errorf("token columns = %+v, want accumulated usage columns", got)
	}
	if got.CostUSD < 0.149 || got.CostUSD > 0.151 {
		t.Errorf("CostUSD = %g, want ~0.15", got.CostUSD)
	}

	total, err := SumSteeringUsage(db)
	if err != nil {
		t.Fatalf("SumSteeringUsage: %v", err)
	}
	if total.Runs != 3 || total.Tokens != 220 {
		t.Errorf("total = %+v, want 3 runs and 220 tokens", total)
	}
}
