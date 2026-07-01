package server

import (
	"os"
	"strings"
	"testing"

	"flow/internal/steering"
)

// The prime must name the surface tool, the action vocabulary, the surface-only
// autonomy boundary, and context_only handling — the load-bearing contract.
func TestSteererSessionBriefContract(t *testing.T) {
	b := steererSessionBrief()
	for _, want := range []string{
		"flow attention surface",
		"make_task", "forward", "capture_kb", "digest_only", "drop",
		"context_only",
		"--context-json-file",
		"context_json_file",
		"--ask-task-agent",
		"flow tell",
		"flow read ask",
		"flow read say",
		"Open attention workstreams",
		"Active task",
		"flow attention resolve",
		"Customer-facing DMs still get drafts",
		"--context-only --thread-key",
		"never", // surface-only: never auto-send a reply
		"thread_key",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("steerer brief missing %q", want)
		}
	}
}

// The per-channel session must inject the operator's SAVED voice text, not just
// name-drop "the operator's voice" — that regression is exactly why saved Voice
// went unhonored on the live (per-channel sessions On) reply path.
func TestSteererSessionBriefInjectsOperatorVoice(t *testing.T) {
	t.Setenv("FLOW_ROOT", t.TempDir())
	const marker = "ZZ-distinct-voice-marker: sign off with cheers, lowercase only"
	if err := os.WriteFile(steering.PersonaPath(), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	b := steererSessionBrief()
	if !strings.Contains(b, marker) {
		t.Errorf("session brief does not inject the saved operator voice; missing %q", marker)
	}
	if !strings.Contains(b, "OPERATOR VOICE") {
		t.Errorf("session brief missing the OPERATOR VOICE directive header")
	}
}
