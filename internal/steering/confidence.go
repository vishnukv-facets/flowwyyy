package steering

import (
	"fmt"
	"strings"
)

// confidenceAnchor defines, for one action, what a high (~0.9) / mid (~0.5) /
// low (~0.2) confidence means. The rubric exists so the model grounds the number
// in a shared, action-specific scale instead of emitting a vague "how sure am I"
// — the root cause the operator named ("confidence is too low to trust").
//
// Confidence is ALWAYS P(the operator agrees with THIS exact action). The same
// definition is later checked by ConfidenceCalibrator against the observed
// agreement rate in the matching band, closing the loop: a 0.9 the model emits is
// graded against how often the operator actually agreed at 0.90-1.00.
type confidenceAnchor struct {
	Action Action
	High   string // ~0.9
	Mid    string // ~0.5
	Low    string // ~0.2
}

// confidenceAnchors covers every surfacing action the classifier chooses between.
// drop carries no surfacing decision, so it gets a single line in the preamble
// rather than an anchor; keep this list in sync with the Action taxonomy.
var confidenceAnchors = []confidenceAnchor{
	{ActionMakeTask,
		"unmistakable, concrete work the operator must own, and no existing task fits",
		"plausibly a task, but the scope or whether the operator owns it is unclear",
		"a guess that this needs tracking as work at all"},
	{ActionForward,
		"deterministic link to the matched task: same thread/DM/PR/issue or an explicit reference to its specific work",
		"same participants or topic as the task but no hard link",
		"thematic-only guess; it could belong to any of several tasks"},
	{ActionReply,
		"the operator clearly must respond and the drafted reply is what they would say",
		"a reply is likely wanted but the right answer is genuinely unclear",
		"unsure the operator needs to reply at all"},
	{ActionCaptureKB,
		"a durable, clearly-stated decision/plan/fact worth remembering long-term",
		"possibly durable but vaguely stated or maybe already known",
		"unsure there is any lasting knowledge here"},
	{ActionDigestOnly,
		"clearly FYI: noteworthy but with nothing for the operator to do",
		"borderline between FYI and actually actionable",
		"unsure it is even worth a mention"},
	{ActionAFKReply,
		"a holding reply is clearly appropriate and its wording is safe to send unattended",
		"a holding reply might help but could also be unwanted",
		"unsure any auto-acknowledgement is warranted"},
}

// confidenceRubric renders the shared confidence definition plus the per-action
// anchors. It is injected verbatim into both the Stage 2 and Stage 3 prompts so
// the cheap scorer and the deep triager grade confidence on the same scale.
func confidenceRubric() string {
	var b strings.Builder
	b.WriteString("CONFIDENCE — define it, do not guess it. `confidence` (0.0-1.0) is your estimate of P(the operator agrees with this exact suggested_action). It is NOT a vague feeling and NOT how interesting the message is. Anchor the number to these per-action bands:\n")
	for _, a := range confidenceAnchors {
		fmt.Fprintf(&b, "- %s: ~0.9 = %s; ~0.5 = %s; ~0.2 = %s.\n", a.Action, a.High, a.Mid, a.Low)
	}
	b.WriteString("- drop: confidence is how sure you are the message is noise not worth surfacing.\n")
	b.WriteString("When required context is missing or ambiguous, lower the confidence rather than rounding up — an honest 0.5 is more useful than an optimistic 0.9.")
	return b.String()
}
