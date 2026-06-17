package steering

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"flow/internal/flowdb"
)

// ActionPolicy is the operator's autonomy setting for one action: whether the
// steerer may perform it without asking, and the minimum confidence required.
type ActionPolicy struct {
	Enabled   bool    `json:"enabled"`
	Threshold float64 `json:"threshold"`
}

// AutonomyPolicy maps each action to its policy. A missing action is treated
// as disabled (deny). See spec §8.
type AutonomyPolicy map[Action]ActionPolicy

// AutonomyCapability documents one rung in the assistant-action trust ladder.
// The matrix is product policy, not just implementation detail: it records what
// can be automated today, what remains manual, and what audit trail proves it.
type AutonomyCapability struct {
	Key              string
	Action           Action
	Label            string
	Risk             string
	DefaultEnabled   bool
	DefaultThreshold float64
	Prerequisites    string
	Audit            string
	AutoActable      bool
	Configurable     bool
}

// AutonomyDecision is the explainable result of evaluating one action against
// the live policy and a triage confidence.
type AutonomyDecision struct {
	Action    Action
	Allowed   bool
	Decision  string
	Reason    string
	Threshold float64
	Risk      string
	Audit     string
}

var autonomyLadder = []AutonomyCapability{
	{
		Key: "surface", Label: "Surface only", Risk: "none", DefaultEnabled: true,
		Prerequisites: "Stage 0/1/2/3 triage passes.",
		Audit:         "steering_trace row plus attention_feed card.",
	},
	{
		Key: "muted_scope_dismiss", Label: "Mute-scope suppression", Risk: "low", DefaultEnabled: true,
		Prerequisites: "Operator explicitly muted the channel, sender, thread, or learned suppression threshold was reached.",
		Audit:         "steering_trace dropped at stage0 with the matched mute reason.",
	},
	{
		Key: "forward", Action: ActionForward, Label: "Forward to matched task", Risk: "medium", DefaultThreshold: 0.85,
		Prerequisites: "Matched task exists and confidence meets the operator threshold.",
		Audit:         "steering_trace autonomy fields plus attention_feedback and linked task.",
		AutoActable:   true, Configurable: true,
	},
	{
		Key: "make_task", Action: ActionMakeTask, Label: "Create backlog task", Risk: "medium", DefaultThreshold: 0.80,
		Prerequisites: "No existing tracked task owns the thread and confidence meets the operator threshold.",
		Audit:         "steering_trace autonomy fields plus attention_feedback and spawned task link.",
		AutoActable:   true, Configurable: true,
	},
	{
		Key: "capture_kb", Action: ActionCaptureKB, Label: "Capture to knowledge base", Risk: "low", DefaultThreshold: 0.75,
		Prerequisites: "Watched conversation carries a durable fact and confidence meets the operator threshold.",
		// Lowest auto-act threshold of the safe set: a KB capture appends one line to a
		// local, git-tracked markdown file and is never outward-facing, so a wrong call
		// is cheap and reversible. The autonomous path records NO attention_feedback row
		// (audit is the trace + the agent's dated provenance line), so it can't inflate
		// the very calibration it gates on.
		Audit:       "steering_trace autonomy fields plus the dated KB provenance line the capture agent writes; feed card marked acted on confirmed write.",
		AutoActable: true, Configurable: true,
	},
	{
		Key: "dismiss", Action: ActionDigestOnly, Label: "Auto-dismiss FYI card", Risk: "low", DefaultThreshold: 0.85,
		Prerequisites: "Surfaced verdict is a digest_only FYI and confidence meets the operator threshold.",
		// Resolves a surfaced FYI card without nagging the operator. Reversible (the card
		// is marked dismissed, recoverable), but it suppresses visibility — so it sits at
		// the high 0.85 default rather than capture_kb's 0.75. Note: a `drop` verdict is
		// already suppressed pre-card unconditionally; this gate only governs the
		// digest_only cards that DO surface.
		Audit:       "steering_trace autonomy fields plus the dismissed feed card.",
		AutoActable: true, Configurable: true,
	},
	{
		Key: "clear_waiting_on", Label: "Clear waiting_on", Risk: "medium",
		Prerequisites: "A non-operator reply lands on a thread/tag already linked to a waiting task.",
		Audit:         "task update timestamp plus inbound event lineage in the task inbox/dispatcher path.",
	},
	{
		Key: "afk_reply", Action: ActionAFKReply, Label: "AFK holding reply", Risk: "high", DefaultThreshold: 0.90,
		Prerequisites: "Presence/AFK state and connector send path are not implemented yet.",
		Audit:         "Not auto-actable today; future implementation must write trace, feedback, and source-send proof.",
	},
	{
		Key: "reply", Action: ActionReply, Label: "Auto-send outbound reply", Risk: "critical", DefaultThreshold: 0.95,
		Prerequisites: "Operator opted reply autonomy IN (off by default) and calibrated confidence meets the high threshold; the per-channel chat posts in-thread.",
		// CRITICAL risk: an autonomous reply posts to a colleague's Slack thread/DM
		// with no per-message click. OFF by default (DefaultEnabled unset) and gated at
		// the highest default threshold (0.95). When enabled, the channel's own chat
		// posts via postApprovedReplyViaChat — the autonomous path records NO
		// attention_feedback row (audit is the steering trace + the agent's send
		// confirmation), so it can't inflate the calibration it gates on.
		Audit:       "steering_trace autonomy fields plus the channel chat's agent-send confirmation.",
		AutoActable: true, Configurable: true,
	},
}

// AutonomyLadder returns a copy of the product policy matrix so callers/tests
// can inspect it without mutating package state.
func AutonomyLadder() []AutonomyCapability {
	out := make([]AutonomyCapability, len(autonomyLadder))
	copy(out, autonomyLadder)
	return out
}

func autonomyCapabilityForAction(action Action) (AutonomyCapability, bool) {
	for _, step := range autonomyLadder {
		if step.Action == action {
			return step, true
		}
	}
	return AutonomyCapability{}, false
}

// DefaultAutonomy returns the P1 posture: every action surface-only (disabled).
// The thresholds are pre-seeded with the spec's defaults so the P2 settings UI
// has sensible starting values when an action is later enabled.
func DefaultAutonomy() AutonomyPolicy {
	out := AutonomyPolicy{}
	for _, step := range autonomyLadder {
		if step.Action == "" {
			continue
		}
		out[step.Action] = ActionPolicy{Enabled: step.DefaultEnabled, Threshold: step.DefaultThreshold}
	}
	return out
}

// Allow reports whether the steerer may perform action autonomously at the
// given confidence. This is the single chokepoint every outward effect must
// pass; an action that is absent or disabled is always denied, so triage code
// can never act on its own unless the operator opted in.
func (p AutonomyPolicy) Allow(action Action, confidence float64) bool {
	return p.Evaluate(action, confidence).Allowed
}

// Evaluate explains the autonomy gate decision. It blocks unsupported and
// manual-only actions even if a hand-edited JSON policy tries to enable them.
func (p AutonomyPolicy) Evaluate(action Action, confidence float64) AutonomyDecision {
	spec, ok := autonomyCapabilityForAction(action)
	if !ok {
		return AutonomyDecision{Action: action, Decision: "unsupported", Reason: fmt.Sprintf("action %q is not in the autonomy ladder", action)}
	}
	threshold := spec.DefaultThreshold
	if !spec.AutoActable {
		return AutonomyDecision{
			Action: action, Decision: "manual_only", Threshold: threshold, Risk: spec.Risk, Audit: spec.Audit,
			Reason: fmt.Sprintf("%s is %s risk and is not auto-actable today; prerequisite: %s", spec.Key, spec.Risk, spec.Prerequisites),
		}
	}
	pol, ok := p[action]
	if !ok || !pol.Enabled {
		return AutonomyDecision{
			Action: action, Decision: "disabled", Threshold: threshold, Risk: spec.Risk, Audit: spec.Audit,
			Reason: fmt.Sprintf("%s is disabled; default threshold %.2f; prerequisite: %s", spec.Key, threshold, spec.Prerequisites),
		}
	}
	threshold = pol.Threshold
	if confidence < threshold {
		return AutonomyDecision{
			Action: action, Decision: "below_threshold", Threshold: threshold, Risk: spec.Risk, Audit: spec.Audit,
			Reason: fmt.Sprintf("%s confidence %.2f < threshold %.2f", spec.Key, confidence, threshold),
		}
	}
	return AutonomyDecision{
		Action: action, Allowed: true, Decision: "allowed", Threshold: threshold, Risk: spec.Risk, Audit: spec.Audit,
		Reason: fmt.Sprintf("%s allowed: confidence %.2f >= threshold %.2f; prerequisite: %s", spec.Key, confidence, threshold, spec.Prerequisites),
	}
}

// AutonomyFromEnv builds the autonomy policy from FLOW_STEERING_AUTONOMY — a
// JSON object mapping action name → {"enabled":bool,"threshold":float}. It
// starts from DefaultAutonomy (everything off, sensible thresholds) and applies
// any recognized overrides; an empty/unparseable value or an unknown action key
// leaves the safe defaults intact, so a malformed setting can never accidentally
// switch autonomy ON. Thresholds are clamped to [0,1].
func AutonomyFromEnv() AutonomyPolicy {
	pol := DefaultAutonomy()
	raw := strings.TrimSpace(os.Getenv("FLOW_STEERING_AUTONOMY"))
	if raw == "" {
		return pol
	}
	var parsed map[string]ActionPolicy
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return pol
	}
	for k, v := range parsed {
		a, ok := ParseAction(k)
		if !ok {
			continue
		}
		if v.Threshold < 0 {
			v.Threshold = 0
		}
		if v.Threshold > 1 {
			v.Threshold = 1
		}
		pol[a] = v
	}
	return pol
}

// AutonomyFnWithFeedback wraps a base live-policy source with learned threshold
// adjustments derived from operator feedback. It never enables an action; it
// only nudges thresholds for actions the base policy already knows about.
func AutonomyFnWithFeedback(db *sql.DB, base func() AutonomyPolicy) func() AutonomyPolicy {
	return func() AutonomyPolicy {
		pol := DefaultAutonomy()
		if base != nil {
			pol = cloneAutonomyPolicy(base())
		}
		learned, err := flowdb.LearnedAttentionPolicyFromFeedback(db, flowdb.LearnedAttentionPolicyOptions{})
		if err != nil {
			return pol
		}
		for raw, adj := range learned.ThresholdAdjustments {
			action, ok := ParseAction(raw)
			if !ok {
				continue
			}
			p, ok := pol[action]
			if !ok {
				continue
			}
			p.Threshold = clampThreshold(p.Threshold + adj)
			pol[action] = p
		}
		return pol
	}
}

func cloneAutonomyPolicy(in AutonomyPolicy) AutonomyPolicy {
	out := AutonomyPolicy{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func clampThreshold(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return math.Round(v*100) / 100
}
