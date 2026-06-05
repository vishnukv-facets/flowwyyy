// internal/steering/classifier_test.go
package steering

import (
	"context"
	"strings"
	"testing"
)

// stubClassifier swaps the package classifierRunner with one that returns
// canned output based on a MODE marker embedded in each prompt builder.
func stubClassifier(t *testing.T, fn func(prompt string) (string, error)) {
	t.Helper()
	old := classifierRunner
	classifierRunner = func(ctx context.Context, prompt string) (string, error) { return fn(prompt) }
	t.Cleanup(func() { classifierRunner = old })
}

func TestExtractJSON(t *testing.T) {
	cases := []struct{ in, want string }{
		{`[{"a":1}]`, `[{"a":1}]`},
		{"prefix noise\n```json\n{\"a\":1}\n```\ntrailing", `{"a":1}`},
		{`  {"nested":{"b":[1,2]}} junk`, `{"nested":{"b":[1,2]}}`},
		{`text [1, {"x":"]"}] more`, `[1, {"x":"]"}]`}, // bracket inside string ignored
	}
	for _, c := range cases {
		got, err := extractJSON(c.in)
		if err != nil || got != c.want {
			t.Errorf("extractJSON(%q) = (%q, %v), want %q", c.in, got, err, c.want)
		}
	}
	if _, err := extractJSON("no json here"); err == nil {
		t.Error("expected error for input with no JSON")
	}
}

func TestStage1Relevance(t *testing.T) {
	stubClassifier(t, func(prompt string) (string, error) {
		if !strings.Contains(prompt, "MODE: stage1-relevance") {
			t.Fatalf("stage1 prompt missing marker: %q", prompt)
		}
		// Model blesses k1, drops k2, and omits k3 entirely.
		return `[{"thread_key":"k1","relevant":true,"category":"customer","urgency_hint":"urgent"},
		         {"thread_key":"k2","relevant":false}]`, nil
	})
	inputs := []ClassifyInput{
		{ThreadKey: "k1", Text: "rollout date?"},
		{ThreadKey: "k2", Text: "lol"},
		{ThreadKey: "k3", Text: "???"},
	}
	out, err := Stage1Relevance(context.Background(), inputs)
	if err != nil {
		t.Fatalf("Stage1Relevance: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3 (one per input)", len(out))
	}
	if !out[0].Relevant || out[0].UrgencyHint != "urgent" {
		t.Errorf("k1 = %+v, want relevant urgent", out[0])
	}
	if out[1].Relevant {
		t.Errorf("k2 should be not relevant")
	}
	if out[2].Relevant { // omitted by model → fail-closed to not relevant
		t.Errorf("k3 omitted by model must default to not relevant")
	}
}

func TestStage2Score(t *testing.T) {
	stubClassifier(t, func(prompt string) (string, error) {
		if !strings.Contains(prompt, "MODE: stage2-score") {
			t.Fatalf("stage2 prompt missing marker")
		}
		if !strings.Contains(prompt, "kong-split") {
			t.Fatalf("stage2 prompt should embed the task index")
		}
		return `{"suggested_action":"make_task","matched_task":"kong-split",
		         "suggested_project":"goniyo","suggested_priority":"high",
		         "urgency":"urgent","confidence":0.88,"summary":"asks rollout date",
		         "reason":"customer question naming the project"}`, nil
	})
	v, err := Stage2Score(context.Background(), ClassifyInput{ThreadKey: "C1:1.1", Source: "slack", Text: "when does kong-split ship?"}, "Tasks:\n- kong-split [goniyo] (in-progress): Kong split")
	if err != nil {
		t.Fatalf("Stage2Score: %v", err)
	}
	if v.SuggestedAction != ActionMakeTask || v.MatchedTask != "kong-split" || v.Confidence != 0.88 {
		t.Errorf("verdict = %+v", v)
	}
	if v.Source != "slack" || v.ThreadKey != "C1:1.1" {
		t.Errorf("Source/ThreadKey should be filled from input: %+v", v)
	}
}

func TestParseVerdictRejectsBadAction(t *testing.T) {
	v, err := parseVerdict(`{"suggested_action":"frobnicate","confidence":0.9}`, "slack", "k1")
	if err != nil {
		t.Fatalf("parseVerdict: %v", err)
	}
	if v.SuggestedAction != ActionDrop {
		t.Errorf("unknown action must normalize to drop, got %q", v.SuggestedAction)
	}
}
