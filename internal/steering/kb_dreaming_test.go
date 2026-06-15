package steering

import (
	"context"
	"strings"
	"testing"
)

func TestDreamKBPromptStructure(t *testing.T) {
	p := dreamKBPrompt("/home/u/.flow/kb")
	for _, want := range []string{
		"/home/u/.flow/kb/user.md",
		"/home/u/.flow/kb/business.md",
		PendingRemovalHeading,
		"[flagged ",
		"why:",
		"NEVER delete",
		"CONSERVATIVE",
		"DREAMED",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("dreamKBPrompt missing %q", want)
		}
	}
}

func TestDreamKBViaAgentRunsAndReturnsReply(t *testing.T) {
	prev := captureKBRunner
	var gotPrompt string
	captureKBRunner = func(_ context.Context, prompt string) (string, error) {
		gotPrompt = prompt
		return "DREAMED 2\n", nil
	}
	t.Cleanup(func() { captureKBRunner = prev })

	out, err := DreamKBViaAgent(context.Background(), nil, "/kb")
	if err != nil {
		t.Fatalf("DreamKBViaAgent: %v", err)
	}
	if !strings.Contains(gotPrompt, "/kb/user.md") {
		t.Errorf("agent should get a prompt scoped to the kb dir")
	}
	if !strings.Contains(out, "DREAMED 2") {
		t.Errorf("reply should be returned, got %q", out)
	}
}

func TestDreamKBViaAgentRequiresDir(t *testing.T) {
	if _, err := DreamKBViaAgent(context.Background(), nil, "  "); err == nil {
		t.Errorf("expected error for empty kb dir")
	}
}
