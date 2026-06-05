// internal/steering/triage_test.go
package steering

import (
	"context"
	"strings"
	"testing"
)

func stubDeepTriage(t *testing.T, fn func(prompt string) (string, error)) {
	t.Helper()
	old := deepTriageRunner
	deepTriageRunner = func(ctx context.Context, prompt string) (string, error) { return fn(prompt) }
	t.Cleanup(func() { deepTriageRunner = old })
}

func TestContextHintFor(t *testing.T) {
	gh := contextHintFor("github")
	if !strings.Contains(gh, "GitHub") || !strings.Contains(gh, "gh-pr/gh-issue") {
		t.Errorf("github hint = %q, want GitHub-specific guidance", gh)
	}
	sl := contextHintFor("slack")
	if !strings.Contains(sl, "Slack MCP") || !strings.Contains(sl, "thread_ts") {
		t.Errorf("slack hint = %q, want Slack-specific guidance", sl)
	}
	// Unknown source falls back to the Slack hint (default connector).
	if contextHintFor("email") != sl {
		t.Errorf("unknown source should default to the Slack hint")
	}
}

func TestDeepTriagePromptUsesSourceHint(t *testing.T) {
	ghPrompt := deepTriagePrompt(ClassifyInput{ThreadKey: "o/r:gh-pr:o/r#5", Source: "github", Text: "review?"}, "Tasks:\n(none)")
	if !strings.Contains(ghPrompt, "use the `gh` CLI") {
		t.Errorf("github deep prompt missing the gh hint:\n%s", ghPrompt)
	}
	if strings.Contains(ghPrompt, "Slack MCP") {
		t.Errorf("github deep prompt must NOT carry the Slack hint:\n%s", ghPrompt)
	}
}

func TestPromptsInstructUseNamesNotIDs(t *testing.T) {
	const want = "never output raw platform IDs"
	if !strings.Contains(stage1Prime(), want) {
		t.Errorf("stage1Prime must instruct the model to use names not IDs")
	}
	if !strings.Contains(stage2Prime("Tasks:\n(none)"), want) {
		t.Errorf("stage2Prime must instruct the model to use names not IDs")
	}
	if !strings.Contains(deepTriagePrompt(ClassifyInput{Source: "slack"}, "Tasks:\n(none)"), want) {
		t.Errorf("deepTriagePrompt must instruct the model to use names not IDs")
	}
}

func TestDeepTriage(t *testing.T) {
	stubDeepTriage(t, func(prompt string) (string, error) {
		if !strings.Contains(prompt, "MODE: stage3-deep") {
			t.Fatalf("deep prompt missing marker")
		}
		return "```json\n" + `{"suggested_action":"reply","confidence":0.93,
		  "summary":"customer wants ETA","draft":"Targeting Friday — will confirm.",
		  "urgency":"urgent","reason":"direct question to operator"}` + "\n```", nil
	})
	v, err := DeepTriage(context.Background(), ClassifyInput{ThreadKey: "C1:9.9", Source: "slack", Text: "ETA?"}, "Tasks:\n(none)")
	if err != nil {
		t.Fatalf("DeepTriage: %v", err)
	}
	if v.SuggestedAction != ActionReply || v.Draft == "" || v.ThreadKey != "C1:9.9" {
		t.Errorf("verdict = %+v", v)
	}
}
