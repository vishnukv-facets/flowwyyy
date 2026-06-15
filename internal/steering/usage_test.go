package steering

import (
	"context"
	"testing"
)

func TestParseClaudeJSONOutputReturnsResultAndUsage(t *testing.T) {
	raw := []byte(`{
		"type":"result",
		"result":"{\"suggested_action\":\"drop\"}",
		"usage":{
			"input_tokens":100,
			"cache_creation_input_tokens":20,
			"cache_read_input_tokens":1000,
			"output_tokens":30
		},
		"total_cost_usd":0.42
	}`)

	result, delta, err := parseClaudeJSONOutput(raw, "classifier", "2026-06-15")
	if err != nil {
		t.Fatalf("parseClaudeJSONOutput: %v", err)
	}
	if result != `{"suggested_action":"drop"}` {
		t.Fatalf("result = %q", result)
	}
	if delta.Kind != "classifier" || delta.Day != "2026-06-15" {
		t.Fatalf("delta identity = %+v", delta)
	}
	if delta.Tokens != 150 {
		t.Errorf("Tokens = %d, want 150", delta.Tokens)
	}
	if delta.InputTokens != 100 || delta.CacheCreationInputTokens != 20 || delta.CacheReadInputTokens != 1000 || delta.OutputTokens != 30 {
		t.Errorf("delta usage = %+v, want parsed usage", delta)
	}
	if delta.CostUSD != 0.42 {
		t.Errorf("CostUSD = %g, want 0.42", delta.CostUSD)
	}
}

func TestClassifierRunnerRequestsClaudeJSONOutput(t *testing.T) {
	stubClaudeBinary(t, `
case "$*" in
  *"--output-format json"*) ;;
  *) echo "missing output format: $*" >&2; exit 2 ;;
esac
cat <<'JSON'
{"type":"result","result":"[{\"thread_key\":\"k\",\"relevant\":true}]","usage":{"input_tokens":1,"output_tokens":2},"total_cost_usd":0.01}
JSON
`)

	out, err := classifierRunner(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("classifierRunner: %v", err)
	}
	if out != `[{"thread_key":"k","relevant":true}]` {
		t.Fatalf("classifierRunner output = %q", out)
	}
}

func TestDecodeClaudeJSONOutputRecordsUsage(t *testing.T) {
	db := backfillTestDB(t)
	raw := []byte(`{"result":"ok","usage":{"input_tokens":4,"output_tokens":6},"total_cost_usd":0.02}`)

	out, err := decodeClaudeJSONOutput(withSteeringUsage(context.Background(), db, "capture_kb"), "classifier", raw)
	if err != nil {
		t.Fatalf("decodeClaudeJSONOutput: %v", err)
	}
	if out != "ok" {
		t.Fatalf("out = %q, want ok", out)
	}
	var kind string
	var tokens, runs int
	err = db.QueryRow(`SELECT kind, tokens, runs FROM steering_usage`).Scan(&kind, &tokens, &runs)
	if err != nil {
		t.Fatalf("load steering usage: %v", err)
	}
	if kind != "capture_kb" || tokens != 10 || runs != 1 {
		t.Fatalf("usage row = {kind:%q tokens:%d runs:%d}, want one capture_kb row with 10 tokens", kind, tokens, runs)
	}
}
