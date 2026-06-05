package steering

import (
	"encoding/json"
	"os"
	"strings"
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

// DefaultAutonomy returns the P1 posture: every action surface-only (disabled).
// The thresholds are pre-seeded with the spec's defaults so the P2 settings UI
// has sensible starting values when an action is later enabled.
func DefaultAutonomy() AutonomyPolicy {
	return AutonomyPolicy{
		ActionForward:  {Enabled: false, Threshold: 0.85},
		ActionAFKReply: {Enabled: false, Threshold: 0.90},
		ActionMakeTask: {Enabled: false, Threshold: 0.80},
		ActionReply:    {Enabled: false, Threshold: 0.95},
	}
}

// Allow reports whether the steerer may perform action autonomously at the
// given confidence. This is the single chokepoint every outward effect must
// pass; an action that is absent or disabled is always denied, so triage code
// can never act on its own unless the operator opted in.
func (p AutonomyPolicy) Allow(action Action, confidence float64) bool {
	pol, ok := p[action]
	if !ok || !pol.Enabled {
		return false
	}
	return confidence >= pol.Threshold
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
