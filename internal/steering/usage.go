package steering

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"flow/internal/flowdb"
)

type steeringUsageContext struct {
	db   *sql.DB
	kind string
}

type steeringUsageKey struct{}

func withSteeringUsage(ctx context.Context, db *sql.DB, kind string) context.Context {
	if db == nil || strings.TrimSpace(kind) == "" {
		return ctx
	}
	return context.WithValue(ctx, steeringUsageKey{}, steeringUsageContext{db: db, kind: strings.TrimSpace(kind)})
}

func decodeClaudeJSONOutput(ctx context.Context, fallbackKind string, raw []byte) (string, error) {
	kind := fallbackKind
	if rec, ok := ctx.Value(steeringUsageKey{}).(steeringUsageContext); ok && rec.kind != "" {
		kind = rec.kind
	}
	result, delta, err := parseClaudeJSONOutput(raw, kind, time.Now().In(time.Local).Format("2006-01-02"))
	if err != nil {
		return "", err
	}
	recordSteeringUsage(ctx, delta)
	return result, nil
}

func recordSteeringUsage(ctx context.Context, delta flowdb.SteeringUsageDelta) {
	if delta.Tokens == 0 && delta.CostUSD == 0 {
		return
	}
	rec, ok := ctx.Value(steeringUsageKey{}).(steeringUsageContext)
	if !ok || rec.db == nil {
		return
	}
	if delta.Kind == "" {
		delta.Kind = rec.kind
	}
	if err := flowdb.RecordSteeringUsage(rec.db, delta); err != nil {
		fmt.Fprintf(os.Stderr, "steering: record usage: %v\n", err)
	}
}

func parseClaudeJSONOutput(raw []byte, kind, day string) (string, flowdb.SteeringUsageDelta, error) {
	var env struct {
		Result       string          `json:"result"`
		TotalCostUSD float64         `json:"total_cost_usd"`
		Usage        claudeJSONUsage `json:"usage"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &env); err != nil {
		return "", flowdb.SteeringUsageDelta{}, fmt.Errorf("steering: parse claude json output: %w", err)
	}
	delta := env.Usage.delta(kind, day)
	delta.CostUSD = env.TotalCostUSD
	return env.Result, delta, nil
}

type claudeJSONUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreation            struct {
		Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (u claudeJSONUsage) delta(kind, day string) flowdb.SteeringUsageDelta {
	cacheCreation := u.CacheCreationInputTokens
	if cacheCreation == 0 {
		cacheCreation = u.CacheCreation.Ephemeral5m + u.CacheCreation.Ephemeral1h
	}
	tokens := u.InputTokens + cacheCreation + u.OutputTokens
	return flowdb.SteeringUsageDelta{
		Kind:                     strings.TrimSpace(kind),
		Day:                      strings.TrimSpace(day),
		Runs:                     1,
		Tokens:                   tokens,
		InputTokens:              u.InputTokens,
		CacheCreationInputTokens: cacheCreation,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		OutputTokens:             u.OutputTokens,
	}
}
