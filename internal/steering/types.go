// Package steering holds the connector-blind attention-router ("steerer")
// triage layer: shared types, the Stage 0 deterministic filter, and the
// autonomy gate. It observes incoming events across pluggable connectors,
// triages them cheap-to-expensive, and surfaces candidates for the operator.
package steering

import (
	"context"
	"strings"

	"flow/internal/monitor"
)

// Action is a triage outcome the steerer can take or surface to the operator.
type Action string

const (
	ActionMakeTask   Action = "make_task"
	ActionForward    Action = "forward"
	ActionReply      Action = "reply"
	ActionAFKReply   Action = "afk_reply"
	ActionDigestOnly Action = "digest_only"
	ActionDrop       Action = "drop"
)

// ParseAction parses a triage action string (trimmed, case-insensitive).
// Returns ok=false for unknown values so classifier output can be validated
// before it drives any side effect.
func ParseAction(s string) (Action, bool) {
	switch Action(strings.ToLower(strings.TrimSpace(s))) {
	case ActionMakeTask:
		return ActionMakeTask, true
	case ActionForward:
		return ActionForward, true
	case ActionReply:
		return ActionReply, true
	case ActionAFKReply:
		return ActionAFKReply, true
	case ActionDigestOnly:
		return ActionDigestOnly, true
	case ActionDrop:
		return ActionDrop, true
	default:
		return "", false
	}
}

// Urgency is the steerer's coarse time-sensitivity bucket.
type Urgency string

const (
	UrgencyUrgent Urgency = "urgent"
	UrgencyNormal Urgency = "normal"
	UrgencyLow    Urgency = "low"
)

// Verdict is the structured triage output (Stage 2 router / Stage 3 deep
// agent). It is connector-blind and is what populates the Attention feed.
// See spec §6.3.
type Verdict struct {
	Source            string  `json:"source"`
	ThreadKey         string  `json:"thread_key"`
	SuggestedAction   Action  `json:"suggested_action"`
	MatchedTask       string  `json:"matched_task,omitempty"`
	SuggestedProject  string  `json:"suggested_project,omitempty"`
	SuggestedPriority string  `json:"suggested_priority,omitempty"`
	Urgency           Urgency `json:"urgency,omitempty"`
	IsVIP             bool    `json:"is_vip,omitempty"`
	Confidence        float64 `json:"confidence"`
	Summary           string  `json:"summary,omitempty"`
	Draft             string  `json:"draft,omitempty"`
	Reason            string  `json:"reason,omitempty"`
}

// OperatorIdentity is the set of identifiers that count as "the operator" on
// a connector (Slack user IDs, GitHub logins, email addresses). Stage 0 uses
// it to drop self-authored events.
type OperatorIdentity struct {
	UserIDs []string
}

// ContextMessage is one message inside a fetched thread.
type ContextMessage struct {
	Author string `json:"author"`
	Text   string `json:"text"`
	TS     string `json:"ts"`
}

// ThreadContext is a normalized bundle of richer context a connector fetches
// on demand for the deep triage stage.
type ThreadContext struct {
	Summary      string           `json:"summary,omitempty"`
	Participants []string         `json:"participants,omitempty"`
	Messages     []ContextMessage `json:"messages,omitempty"`
	Permalink    string           `json:"permalink,omitempty"`
}

// Connector abstracts a monitored source (slack, github, gmail). The cascade,
// feed, and actions never know which connector an item came from. SendReply
// is invoked ONLY by the autonomy gate — never by triage code (spec §5).
type Connector interface {
	Name() string
	Events(ctx context.Context) <-chan monitor.InboundEvent
	FetchContext(ctx context.Context, ev monitor.InboundEvent) (ThreadContext, error)
	SendReply(ctx context.Context, ev monitor.InboundEvent, text string) error
	Identity() OperatorIdentity
}
