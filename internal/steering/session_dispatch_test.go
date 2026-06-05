// internal/steering/session_dispatch_test.go
package steering

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestStage1PromptCompositionUnchanged verifies that splitting stage1 into
// prime+payload and rejoining with "\n\n" produces byte-identical output to the
// original stage1Prompt function.
func TestStage1PromptCompositionUnchanged(t *testing.T) {
	inputsJSON := `[{"thread_key":"C1:1","source":"slack","author":"alice","text":"when does it ship?"}]`

	original := stage1Prompt(inputsJSON)
	composed := stage1Prime() + "\n\n" + stage1Payload(inputsJSON)

	if composed != original {
		t.Errorf("composed stage1 prompt differs from original:\noriginal=%q\ncomposed=%q", original, composed)
	}
	if !strings.Contains(composed, "MODE: stage1-relevance") {
		t.Errorf("composed stage1 prompt missing MODE marker")
	}
	if !strings.Contains(composed, "Events (JSON array):\n"+inputsJSON) {
		t.Errorf("composed stage1 prompt missing events tail")
	}
	if !strings.HasSuffix(composed, inputsJSON) {
		t.Errorf("composed stage1 prompt does not end with inputsJSON")
	}
}

// TestStage2PromptCompositionUnchanged verifies that splitting stage2 into
// prime+payload and rejoining with "\n\n" produces byte-identical output to the
// original stage2Prompt function.
func TestStage2PromptCompositionUnchanged(t *testing.T) {
	in := ClassifyInput{ThreadKey: "C1:1.1", Source: "slack", Author: "bob", Text: "can you approve the PR?"}
	taskIndex := "Tasks:\n- kong-split [goniyo] (in-progress): Kong split"

	original := stage2Prompt(in, taskIndex)
	composed := stage2Prime(taskIndex) + "\n\n" + stage2Payload(in)

	if composed != original {
		t.Errorf("composed stage2 prompt differs from original:\noriginal=%q\ncomposed=%q", original, composed)
	}
	if !strings.Contains(composed, "Operator task/project index:\n"+taskIndex) {
		t.Errorf("composed stage2 prompt missing task index section")
	}
	if !strings.Contains(composed, "Message (JSON):\n") {
		t.Errorf("composed stage2 prompt missing message section")
	}
}

// TestRunClassifierUsesPoolWhenEnabled verifies that when activeClassifierPool
// is set, Stage1Relevance routes through the pool (the recorded args include
// --session-id).
func TestRunClassifierUsesPoolWhenEnabled(t *testing.T) {
	// Save and restore the global.
	old := activeClassifierPool
	t.Cleanup(func() { activeClassifierPool = nil; activeClassifierPool = old })

	var recordedArgs []string
	pool := newClassifierPool(10, time.Hour)
	pool.exec = func(ctx context.Context, args []string) (string, error) {
		cp := make([]string, len(args))
		copy(cp, args)
		recordedArgs = cp
		return `[{"thread_key":"C1:1","relevant":true,"category":"question","urgency_hint":"normal"}]`, nil
	}
	activeClassifierPool = pool

	out, err := Stage1Relevance(context.Background(), []ClassifyInput{{ThreadKey: "C1:1", Text: "hello?"}})
	if err != nil {
		t.Fatalf("Stage1Relevance: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 verdict, got %d", len(out))
	}
	if !out[0].Relevant {
		t.Errorf("expected verdict to be relevant")
	}
	if !hasArg(recordedArgs, "--session-id") {
		t.Errorf("pool path: expected --session-id in args, got %v", recordedArgs)
	}
}

// TestSteeringEnvBool verifies the env-bool helper for all recognized values
// and the default fallback.
func TestSteeringEnvBool(t *testing.T) {
	cases := []struct {
		val  string
		def  bool
		want bool
	}{
		{"1", false, true},
		{"true", false, true},
		{"TRUE", false, true},
		{"yes", false, true},
		{"on", false, true},
		{"0", true, false},
		{"false", true, false},
		{"no", true, false},
		{"off", true, false},
		{"", true, true},   // empty → default=true
		{"", false, false}, // empty → default=false
		{"random", true, true},
		{"random", false, false},
	}
	for _, c := range cases {
		t.Setenv("TEST_STEERING_BOOL_KEY", c.val)
		got := steeringEnvBool("TEST_STEERING_BOOL_KEY", c.def)
		if got != c.want {
			t.Errorf("steeringEnvBool(%q, %v) = %v, want %v", c.val, c.def, got, c.want)
		}
	}
}

// TestShortHashStable verifies that shortHash is deterministic and returns a
// 12-character hex string. Different inputs typically produce different output.
func TestShortHashStable(t *testing.T) {
	h1 := shortHash("hello world")
	h2 := shortHash("hello world")
	if h1 != h2 {
		t.Errorf("shortHash not stable: %q != %q", h1, h2)
	}
	if len(h1) != 12 {
		t.Errorf("shortHash length = %d, want 12", len(h1))
	}
	h3 := shortHash("different input")
	if h1 == h3 {
		t.Errorf("shortHash collision: same hash for different inputs (%q)", h1)
	}
}
