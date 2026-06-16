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

func TestDeepTriageRunnerIncludesClaudeStderrOnFailure(t *testing.T) {
	stubClaudeBinary(t, "echo 'Claude quota exceeded; retry later' >&2\nexit 1\n")

	_, err := deepTriageRunner(context.Background(), "prompt")
	if err == nil {
		t.Fatal("deepTriageRunner error = nil, want command failure")
	}
	got := err.Error()
	for _, want := range []string{"exit status 1", "Claude quota exceeded; retry later"} {
		if !strings.Contains(got, want) {
			t.Fatalf("deepTriageRunner error missing %q:\n%s", want, got)
		}
	}
}

func TestDeepTriagePromptUsesContextPackAsPrimaryInput(t *testing.T) {
	pack := ThreadContext{
		Source:      "github",
		ThreadKey:   "o/r:gh-pr:o/r#5",
		Permalink:   "https://github.com/o/r/pull/5",
		FetchStatus: "ok",
		Parent: &ContextMessage{
			Kind:   "parent",
			Author: "maintainer",
			Text:   "Please review the deploy change",
			TS:     "2026-06-05T09:00:00Z",
		},
		Messages: []ContextMessage{{
			Kind:   "comment",
			Author: "reviewer",
			Text:   "Can we add a rollback note?",
			TS:     "2026-06-05T10:00:00Z",
		}},
		Participants: []string{"maintainer", "reviewer"},
		Timestamps:   []string{"2026-06-05T09:00:00Z", "2026-06-05T10:00:00Z"},
		Summary:      "2 GitHub messages from maintainer, reviewer",
	}
	prompt := deepTriagePromptWithContext(
		ClassifyInput{ThreadKey: "o/r:gh-pr:o/r#5", Source: "github", Text: "review?"},
		"Tasks:\n(none)",
		pack,
	)
	if !strings.Contains(prompt, "Context pack (JSON):") {
		t.Fatalf("deep prompt missing context-pack section:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"permalink":"https://github.com/o/r/pull/5"`) ||
		!strings.Contains(prompt, "rollback note") ||
		!strings.Contains(prompt, `"fetch_status":"ok"`) {
		t.Errorf("deep prompt did not include the structured context pack:\n%s", prompt)
	}
	if strings.Contains(prompt, "Slack MCP") || strings.Contains(prompt, "use the `gh` CLI") {
		t.Errorf("deep prompt must not primarily ask the model to fetch context itself:\n%s", prompt)
	}
}

func TestDeepTriagePromptChecksReferencedArtifactsArePresent(t *testing.T) {
	pack := ThreadContext{
		Source:      "slack",
		ThreadKey:   "C1:1780000000.000100",
		FetchStatus: "ok",
		Parent: &ContextMessage{
			Kind:   "event",
			Author: "Omendra",
			Text:   "Hi Vishnu, is this draft mail correct here?",
			TS:     "1780000000.000100",
		},
		Participants: []string{"Omendra"},
		Timestamps:   []string{"1780000000.000100"},
		Summary:      "Omendra asks about a draft mail, but no draft is included.",
	}
	prompt := deepTriagePromptWithContext(
		ClassifyInput{
			ThreadKey: "C1:1780000000.000100",
			Source:    "slack",
			Author:    "Omendra",
			Text:      "Hi Vishnu, is this draft mail correct here?",
		},
		"Tasks:\n(none)",
		pack,
	)
	for _, want := range []string{
		"referenced artifact",
		"actually present in the context pack",
		"ask the sender to share it",
		"not a context-fetch failure",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("deep prompt missing artifact-presence guidance %q:\n%s", want, prompt)
		}
	}
}

func TestDeepTriagePromptIncrementalHasPriorAndRetrieved(t *testing.T) {
	in := ClassifyInput{ThreadKey: "C1:1.1", Source: "slack", Text: "any update?"}
	inc := IncrementalContext{
		Prior: &PriorUnderstanding{
			Action: "forward", Confidence: 0.82, Summary: "asks about the oauth rollout",
			Reason: "continuation of the rollout thread", EventCount: 3,
			OperatorActions: []string{"forwarded->oauth-rollout"},
			OperatorReplies: []string{"I'll ship it Friday"},
		},
		Retrieved: []RetrievedDoc{
			{Type: "memory", Slug: "kb", Name: "user.md", Snippet: "oauth rollout slipped to next sprint"},
		},
	}
	prompt := deepTriagePromptIncremental(in, "Tasks:\n(none)", contextFromClassifyInput(in), nil, inc)

	for _, want := range []string{
		"INCREMENTAL UPDATE",                       // says so explicitly
		"Prior running understanding (JSON)",       // layer 2 present
		`"summary":"asks about the oauth rollout"`, // prior decision content
		"forwarded-\\u003eoauth-rollout",           // prior operator action survives JSON
		"Retrieved related context (JSON)",         // layer 3 present
		"oauth rollout slipped to next sprint",     // retrieved snippet content
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("incremental prompt missing %q:\n%s", want, prompt)
		}
	}

	// A zero IncrementalContext reproduces the cold prompt — no incremental
	// framing, no prior/retrieved blocks.
	cold := deepTriagePromptIncremental(in, "Tasks:\n(none)", contextFromClassifyInput(in), nil, IncrementalContext{})
	for _, absent := range []string{"INCREMENTAL UPDATE", "Prior running understanding", "Retrieved related context"} {
		if strings.Contains(cold, absent) {
			t.Fatalf("cold prompt unexpectedly contains %q:\n%s", absent, cold)
		}
	}
	if !strings.Contains(cold, "MODE: stage3-deep") || !strings.Contains(cold, "Context pack (JSON):") {
		t.Fatalf("cold prompt lost its base structure:\n%s", cold)
	}
}

// An operator correction renders an authoritative ground-truth block + directive,
// so the deep triager obeys it over its own reading. Absent when there's none.
func TestDeepTriagePromptIncrementalSurfacesOperatorCorrections(t *testing.T) {
	in := ClassifyInput{ThreadKey: "C1:1.1", Source: "slack", Text: "any update?"}
	inc := IncrementalContext{Prior: &PriorUnderstanding{
		Action: "digest_only", Confidence: 0.5, EventCount: 1,
		Corrections: []string{"This MPDM is about the CSX papertrade migration, not the Anshul demo."},
	}}
	prompt := deepTriagePromptIncremental(in, "Tasks:\n(none)", contextFromClassifyInput(in), nil, inc)
	for _, want := range []string{
		"OPERATOR CORRECTIONS",
		"AUTHORITATIVE GROUND TRUTH",
		"Operator corrections (JSON)",
		"CSX papertrade migration",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("correction prompt missing %q:\n%s", want, prompt)
		}
	}

	// No corrections ⇒ no corrections block/directive.
	plain := deepTriagePromptIncremental(in, "Tasks:\n(none)", contextFromClassifyInput(in),
		nil, IncrementalContext{Prior: &PriorUnderstanding{Action: "digest_only", EventCount: 1}})
	if strings.Contains(plain, "OPERATOR CORRECTIONS") || strings.Contains(plain, "Operator corrections (JSON)") {
		t.Fatalf("prompt without corrections unexpectedly contains the corrections block:\n%s", plain)
	}
}

func TestDeepTriagePromptHidesInternalFetchErrorsAndRawSlackIDs(t *testing.T) {
	pack := ThreadContext{
		Source:      "slack",
		ThreadKey:   "C1:1780000000.000100",
		FetchStatus: "error",
		FetchError:  "slack context fetch: not_in_channel",
		Parent: &ContextMessage{
			Kind:   "event",
			Author: "U01RTHXK7EJ",
			Text:   "I'll be on leave tomorrow and the day after.",
			TS:     "1780000000.000100",
		},
		Participants: []string{"U01RTHXK7EJ"},
		Timestamps:   []string{"1780000000.000100"},
		Summary:      "1 Slack message from U01RTHXK7EJ",
	}
	prompt := deepTriagePromptWithContext(
		ClassifyInput{
			ThreadKey: "C1:1780000000.000100",
			Source:    "slack",
			Author:    "U01RTHXK7EJ",
			Text:      "I'll be on leave tomorrow and the day after.",
		},
		"Tasks:\n(none)",
		pack,
	)
	if !strings.Contains(prompt, "I'll be on leave tomorrow and the day after.") {
		t.Fatalf("deep prompt must keep the fallback event text:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"fetch_status":"error"`) {
		t.Fatalf("deep prompt should keep the coarse fetch status for confidence calibration:\n%s", prompt)
	}
	for _, leak := range []string{"not_in_channel", "slack context fetch", "U01RTHXK7EJ"} {
		if strings.Contains(prompt, leak) {
			t.Fatalf("deep prompt leaked %q into model-facing input:\n%s", leak, prompt)
		}
	}
	if !strings.Contains(prompt, "Do not mention context fetch failures") {
		t.Fatalf("deep prompt must explicitly keep fetch failures out of operator-facing verdict text:\n%s", prompt)
	}
}

func TestDeepTriagePromptScrubsRawSlackIDsFromTextFields(t *testing.T) {
	const rawUserID = "U01RTHXK7EJ"
	pack := ThreadContext{
		Source:      "slack",
		ThreadKey:   "C1:1780000000.000100",
		FetchStatus: "ok",
		Parent: &ContextMessage{
			Kind:   "event",
			Author: "Rohit Raveendran",
			Text:   rawUserID + " is on leave tomorrow.",
			TS:     "1780000000.000100",
		},
		Messages: []ContextMessage{{
			Kind:   "reply",
			Author: "teammate",
			Text:   "Please tell " + rawUserID + " about the review.",
			TS:     "1780000000.000200",
		}},
		Participants: []string{"Rohit Raveendran", "teammate"},
		Timestamps:   []string{"1780000000.000100", "1780000000.000200"},
		Summary:      "2 Slack messages from Rohit Raveendran, teammate",
	}
	prompt := deepTriagePromptWithContext(
		ClassifyInput{
			ThreadKey: "C1:1780000000.000100",
			Source:    "slack",
			Author:    "Rohit Raveendran",
			Text:      rawUserID + " is unavailable today.",
		},
		"Tasks:\n(none)",
		pack,
	)
	if strings.Contains(prompt, rawUserID) {
		t.Fatalf("deep prompt leaked raw Slack user ID from text fields:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Slack participant is unavailable today") ||
		!strings.Contains(prompt, "Slack participant is on leave tomorrow") ||
		!strings.Contains(prompt, "Please tell Slack participant about the review") {
		t.Fatalf("deep prompt should keep sanitized text meaning:\n%s", prompt)
	}
}

func TestDeepTriagePromptIncludesTaskImpactHints(t *testing.T) {
	pack := ThreadContext{
		Source:      "slack",
		ThreadKey:   "C1:1780000000.000100",
		FetchStatus: "ok",
		Parent: &ContextMessage{
			Kind:   "event",
			Author: "Rohit Raveendran",
			Text:   "I'll be on leave tomorrow and the day after.",
			TS:     "1780000000.000100",
		},
		Participants: []string{"Rohit Raveendran"},
		Timestamps:   []string{"1780000000.000100"},
		Summary:      "1 Slack message from Rohit Raveendran",
	}
	hints := []TaskImpactHint{{
		TaskSlug: "raptor-review",
		TaskName: "Raptor review",
		Status:   "in-progress",
		Priority: "high",
		Strength: "strong",
		Reason:   "waiting_on mentions Rohit Raveendran",
		Evidence: "Rohit review on PR #159",
	}}
	prompt := deepTriagePromptWithContextAndHints(
		ClassifyInput{
			ThreadKey: "C1:1780000000.000100",
			Source:    "slack",
			Author:    "Rohit Raveendran",
			Text:      "I'll be on leave tomorrow and the day after.",
		},
		"Tasks:\n- raptor-review (in-progress): Raptor review",
		pack,
		hints,
	)
	for _, want := range []string{
		"Task-impact hints (JSON):",
		`"task_slug":"raptor-review"`,
		"waiting_on mentions Rohit Raveendran",
		"Read task-impact hints",
		"Availability/FYI events are not automatically actionable",
		"set matched_task to the strongest affected task",
		`Use "forward" when the affected task/session should know`,
		`Use "digest_only" when there is no affected task and no reply needed`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("deep prompt missing %q:\n%s", want, prompt)
		}
	}
	taskIndexAt := strings.Index(prompt, "Operator task/project index:")
	hintsAt := strings.Index(prompt, "Task-impact hints (JSON):")
	contextAt := strings.Index(prompt, "Context pack (JSON):")
	if taskIndexAt < 0 || hintsAt < 0 || contextAt < 0 {
		t.Fatalf("deep prompt missing expected sections:\n%s", prompt)
	}
	if !(taskIndexAt < hintsAt && hintsAt < contextAt) {
		t.Fatalf("deep prompt sections out of order: taskIndex=%d hints=%d context=%d\n%s", taskIndexAt, hintsAt, contextAt, prompt)
	}
}

func TestDeepTriagePromptScrubsTaskImpactHintFields(t *testing.T) {
	const rawUserID = "U01RTHXK7EJ"
	prompt := deepTriagePromptWithContextAndHints(
		ClassifyInput{
			ThreadKey: "C1:1780000000.000100",
			Source:    "slack",
			Author:    "Rohit Raveendran",
			Text:      "Rohit is on leave tomorrow.",
		},
		"Tasks:\n- raptor-review (in-progress): Raptor review",
		ThreadContext{Source: "slack", ThreadKey: "C1:1780000000.000100", FetchStatus: "ok"},
		[]TaskImpactHint{{
			TaskSlug: "raptor-review",
			TaskName: "Review task for " + rawUserID,
			Status:   "in-progress",
			Priority: "high",
			Strength: "strong",
			Reason:   "waiting_on mentions " + rawUserID,
			Evidence: rawUserID + " review on PR #159",
		}},
	)
	if strings.Contains(prompt, rawUserID) {
		t.Fatalf("deep prompt leaked raw Slack user ID from task-impact hints:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Review task for Slack participant") ||
		!strings.Contains(prompt, "waiting_on mentions Slack participant") ||
		!strings.Contains(prompt, "Slack participant review on PR #159") {
		t.Fatalf("deep prompt should keep sanitized task-impact hint meaning:\n%s", prompt)
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
		if !strings.Contains(prompt, "Context pack (JSON):") {
			t.Fatalf("deep prompt missing context pack:\n%s", prompt)
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
