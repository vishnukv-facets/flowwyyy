package steering

import (
	"strings"
	"testing"
)

func TestParseClaudeStreamForwardsDeltasAndUsesResult(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"{\"suggested_action\":"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"\"forward\"}"}}}`,
		`{"type":"result","subtype":"success","result":"{\"suggested_action\":\"forward\"}"}`,
	}, "\n")

	var deltas []string
	final, _, err := parseClaudeStreamWithUsage(strings.NewReader(ndjson), func(d string) { deltas = append(deltas, d) }, "", "")
	if err != nil {
		t.Fatalf("parseClaudeStream: %v", err)
	}
	if final != `{"suggested_action":"forward"}` {
		t.Fatalf("final = %q, want the result-event text", final)
	}
	// Deltas stream live, in order.
	if strings.Join(deltas, "") != `{"suggested_action":"forward"}` {
		t.Fatalf("deltas joined = %q, want the reassembled text", strings.Join(deltas, ""))
	}
	if len(deltas) != 2 {
		t.Fatalf("delta count = %d, want 2 (one per text_delta)", len(deltas))
	}
}

func TestParseClaudeStreamWithUsageReadsResultEnvelope(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"{\"ok\":"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"true}"}}}`,
		`{"type":"result","result":"{\"ok\":true}","usage":{"input_tokens":10,"cache_creation_input_tokens":2,"cache_read_input_tokens":99,"output_tokens":3},"total_cost_usd":0.07}`,
	}, "\n")

	final, delta, err := parseClaudeStreamWithUsage(strings.NewReader(ndjson), nil, "classifier", "2026-06-15")
	if err != nil {
		t.Fatalf("parseClaudeStreamWithUsage: %v", err)
	}
	if final != `{"ok":true}` {
		t.Fatalf("final = %q, want result-event text", final)
	}
	if delta.Kind != "classifier" || delta.Day != "2026-06-15" {
		t.Fatalf("delta identity = %+v", delta)
	}
	if delta.Tokens != 15 || delta.CacheReadInputTokens != 99 || delta.CostUSD != 0.07 {
		t.Fatalf("delta = %+v, want usage/cost from result event", delta)
	}
}

func TestParseClaudeStreamFallsBackToAccumulatedWhenNoResult(t *testing.T) {
	// No "result" event (e.g. truncated stream) → reassembled deltas are returned.
	ndjson := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"weighing the thread… "}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"{\"x\":1}"}}}`,
	}, "\n")

	var streamed strings.Builder
	final, _, err := parseClaudeStreamWithUsage(strings.NewReader(ndjson), func(d string) { streamed.WriteString(d) }, "", "")
	if err != nil {
		t.Fatalf("parseClaudeStream: %v", err)
	}
	if final != `weighing the thread… {"x":1}` {
		t.Fatalf("final = %q, want accumulated thinking + text", final)
	}
	if streamed.String() != final {
		t.Fatalf("sink saw %q but final is %q", streamed.String(), final)
	}
}

func TestParseClaudeStreamToleratesNoise(t *testing.T) {
	// Blank lines and non-JSON noise must not fail the whole parse.
	ndjson := "garbage not json\n\n" +
		`{"type":"result","result":"{\"ok\":true}"}` + "\n"
	final, _, err := parseClaudeStreamWithUsage(strings.NewReader(ndjson), nil, "", "")
	if err != nil {
		t.Fatalf("parseClaudeStream: %v", err)
	}
	if final != `{"ok":true}` {
		t.Fatalf("final = %q, want the result text", final)
	}
}
