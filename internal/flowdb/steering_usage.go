package flowdb

import (
	"database/sql"
	"strings"
)

// SteeringUsage is the persisted per-kind/per-day aggregate. Tokens follows the
// Mission Control work-token basis: input + output + cache creation, excluding
// cache reads. CostUSD comes from Claude's JSON envelope.
type SteeringUsage struct {
	Kind                     string
	Day                      string
	Runs                     int
	Tokens                   int
	InputTokens              int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	OutputTokens             int
	CostUSD                  float64
	UpdatedAt                string
}

// SteeringUsageDelta is one completed headless automation run's Claude usage.
type SteeringUsageDelta = SteeringUsage

func RecordSteeringUsage(db *sql.DB, d SteeringUsageDelta) error {
	if db == nil {
		return nil
	}
	d.Kind = strings.TrimSpace(d.Kind)
	d.Day = strings.TrimSpace(d.Day)
	if d.Kind == "" || d.Day == "" {
		return nil
	}
	if d.Runs <= 0 {
		d.Runs = 1
	}
	if d.Tokens == 0 {
		d.Tokens = d.InputTokens + d.CacheCreationInputTokens + d.OutputTokens
	}
	now := NowISO()
	_, err := db.Exec(`
		INSERT INTO steering_usage (
			kind, day, runs, tokens, input_tokens, cache_creation_input_tokens,
			cache_read_input_tokens, output_tokens, cost_usd, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, day) DO UPDATE SET
			runs = runs + excluded.runs,
			tokens = tokens + excluded.tokens,
			input_tokens = input_tokens + excluded.input_tokens,
			cache_creation_input_tokens = cache_creation_input_tokens + excluded.cache_creation_input_tokens,
			cache_read_input_tokens = cache_read_input_tokens + excluded.cache_read_input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			cost_usd = cost_usd + excluded.cost_usd,
			updated_at = excluded.updated_at
	`, d.Kind, d.Day, d.Runs, d.Tokens, d.InputTokens, d.CacheCreationInputTokens, d.CacheReadInputTokens, d.OutputTokens, d.CostUSD, now)
	return err
}

func SumSteeringUsage(db *sql.DB) (SteeringUsage, error) {
	if db == nil {
		return SteeringUsage{}, nil
	}
	var u SteeringUsage
	err := db.QueryRow(`
		SELECT
			COALESCE(SUM(runs), 0),
			COALESCE(SUM(tokens), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(cache_creation_input_tokens), 0),
			COALESCE(SUM(cache_read_input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cost_usd), 0)
		FROM steering_usage
	`).Scan(&u.Runs, &u.Tokens, &u.InputTokens, &u.CacheCreationInputTokens, &u.CacheReadInputTokens, &u.OutputTokens, &u.CostUSD)
	return u, err
}
