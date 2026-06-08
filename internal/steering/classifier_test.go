// internal/steering/classifier_test.go
package steering

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func stubClaudeBinary(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestClassifierRunnerIncludesClaudeStderrOnFailure(t *testing.T) {
	stubClaudeBinary(t, "echo 'Claude auth expired: run claude login' >&2\nexit 1\n")

	_, err := classifierRunner(context.Background(), "prompt")
	if err == nil {
		t.Fatal("classifierRunner error = nil, want command failure")
	}
	got := err.Error()
	for _, want := range []string{"exit status 1", "Claude auth expired: run claude login"} {
		if !strings.Contains(got, want) {
			t.Fatalf("classifierRunner error missing %q:\n%s", want, got)
		}
	}
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
		return `[{"thread_key":"k1","relevant":true,"category":"customer","urgency_hint":"urgent","reason":"direct rollout question"},
		         {"thread_key":"k2","relevant":false,"reason":"chit-chat"}]`, nil
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
	if !out[0].Relevant || out[0].UrgencyHint != "urgent" || out[0].Reason != "direct rollout question" {
		t.Errorf("k1 = %+v, want relevant urgent", out[0])
	}
	if out[1].Relevant || out[1].Reason != "chit-chat" {
		t.Errorf("k2 = %+v, want not relevant with reason", out[1])
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

func TestClassifierPayloadsCompactLongText(t *testing.T) {
	longRun := strings.Repeat("x", 5000)
	longText := "prefix " + longRun + " suffix"
	stubClassifier(t, func(prompt string) (string, error) {
		switch {
		case strings.Contains(prompt, "MODE: stage1-relevance"):
			tail := prompt
			if marker := strings.LastIndex(prompt, "Events (JSON array):"); marker >= 0 {
				tail = prompt[marker:]
			}
			jsonText, err := extractJSON(tail)
			if err != nil {
				t.Fatalf("stage1 prompt missing JSON: %v\n%s", err, prompt)
			}
			var inputs []ClassifyInput
			if err := json.Unmarshal([]byte(jsonText), &inputs); err != nil {
				t.Fatalf("stage1 input JSON: %v\n%s", err, jsonText)
			}
			if len(inputs) != 1 {
				t.Fatalf("stage1 inputs = %d, want 1", len(inputs))
			}
			assertCompactedClassifierText(t, inputs[0].Text, longRun)
			return `[{"thread_key":"k1","relevant":true}]`, nil
		case strings.Contains(prompt, "MODE: stage2-score"):
			tail := prompt
			if marker := strings.LastIndex(prompt, "Message (JSON):"); marker >= 0 {
				tail = prompt[marker:]
			}
			jsonText, err := extractJSON(tail)
			if err != nil {
				t.Fatalf("stage2 prompt missing JSON: %v\n%s", err, prompt)
			}
			var input ClassifyInput
			if err := json.Unmarshal([]byte(jsonText), &input); err != nil {
				t.Fatalf("stage2 input JSON: %v\n%s", err, jsonText)
			}
			assertCompactedClassifierText(t, input.Text, longRun)
			return `{"suggested_action":"drop","confidence":0.8}`, nil
		default:
			t.Fatalf("unknown classifier prompt:\n%s", prompt)
			return "", nil
		}
	})

	if _, err := Stage1Relevance(context.Background(), []ClassifyInput{{ThreadKey: "k1", Text: longText}}); err != nil {
		t.Fatalf("Stage1Relevance: %v", err)
	}
	if _, err := Stage2Score(context.Background(), ClassifyInput{ThreadKey: "k1", Text: longText}, "Tasks:\n- k"); err != nil {
		t.Fatalf("Stage2Score: %v", err)
	}
}

func assertCompactedClassifierText(t *testing.T, got, fullRun string) {
	t.Helper()
	if strings.Contains(got, fullRun) {
		t.Fatalf("classifier text was not compacted; length=%d", len(got))
	}
	for _, want := range []string{"prefix", "suffix", "truncated"} {
		if !strings.Contains(got, want) {
			t.Fatalf("compacted classifier text missing %q:\n%s", want, got)
		}
	}
	if len(got) >= len(fullRun) {
		t.Fatalf("compacted classifier text length = %d, want less than original run %d", len(got), len(fullRun))
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
