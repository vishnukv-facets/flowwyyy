package steering

import (
	"strings"
	"testing"
)

// TestConfidenceRubricCoversSurfacingActions parses the structured rubric and
// asserts every surfacing action the classifier chooses between has a complete
// (high/mid/low) anchor with no duplicates — so the prompt can never ship a
// confidence scale that silently omits an action.
func TestConfidenceRubricCoversSurfacingActions(t *testing.T) {
	want := []Action{ActionMakeTask, ActionForward, ActionReply, ActionCaptureKB, ActionDigestOnly, ActionAFKReply}
	have := map[Action]confidenceAnchor{}
	for _, a := range confidenceAnchors {
		if a.High == "" || a.Mid == "" || a.Low == "" {
			t.Errorf("anchor %q has an empty band: %+v", a.Action, a)
		}
		if _, dup := have[a.Action]; dup {
			t.Errorf("duplicate anchor for %q", a.Action)
		}
		have[a.Action] = a
	}
	for _, w := range want {
		if _, ok := have[w]; !ok {
			t.Errorf("confidenceAnchors missing %q", w)
		}
	}
}

// TestConfidenceRubricReachesBothStages asserts the rendered rubric carries the
// confidence definition and every anchor line, and that it is actually injected
// into both the Stage 2 and Stage 3 prompts.
func TestConfidenceRubricReachesBothStages(t *testing.T) {
	const def = "P(the operator agrees with this exact suggested_action)"
	r := confidenceRubric()
	if !strings.Contains(r, def) {
		t.Errorf("rubric missing the confidence definition:\n%s", r)
	}
	for _, a := range confidenceAnchors {
		if !strings.Contains(r, string(a.Action)+":") {
			t.Errorf("rubric missing anchor line for %q:\n%s", a.Action, r)
		}
	}

	if !strings.Contains(stage2Prime("Tasks:\n(none)"), def) {
		t.Error("stage2 prompt missing the confidence rubric")
	}
	stage3 := deepTriagePromptIncremental(
		ClassifyInput{Source: "slack", ThreadKey: "C:1", Text: "hi"},
		"Tasks:\n(none)",
		contextFromClassifyInput(ClassifyInput{Source: "slack"}),
		nil, IncrementalContext{},
	)
	if !strings.Contains(stage3, def) {
		t.Error("stage3 prompt missing the confidence rubric")
	}
}
