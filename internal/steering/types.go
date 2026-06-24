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
	// ActionCaptureKB records the event as durable knowledge in the operator's KB
	// (kb/*.md) rather than as a task — for decisions, plans, and org/process
	// facts worth remembering long-term. Mutually exclusive with make_task at the
	// classifier level; operator-approved (never auto-acted) like outward replies.
	ActionCaptureKB Action = "capture_kb"
	ActionDrop      Action = "drop"
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
	case ActionCaptureKB:
		return ActionCaptureKB, true
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

// StageEvent is one progress signal emitted as an observed event moves through
// the cascade. The server fans these to Mission Control so the operator can
// watch triage happen live (CI-style stages) instead of only seeing the final
// trace after the fact. Connector-blind; carries just enough to render and link
// a stage row. RunID equals the trace ID, so a live run and its persisted trace
// are the same object.
type StageEvent struct {
	RunID     string `json:"run_id"`
	ThreadKey string `json:"thread_key,omitempty"`
	Source    string `json:"source,omitempty"`
	// Origin metadata, carried so the server can resolve human-readable labels
	// (Slack channel/DM name, GitHub repo) for the live triage view — the same
	// enrichment the feed/trace already get. Set from the trace at every stage,
	// so they are present even on a Stage 0 drop (where ThreadKey is still empty).
	// For GitHub, Channel is already "owner/repo" and URL is the canonical link.
	Channel     string `json:"channel,omitempty"`
	ChannelType string `json:"channel_type,omitempty"`
	Author      string `json:"author,omitempty"`
	TS          string `json:"ts,omitempty"`
	TeamID      string `json:"team_id,omitempty"`
	URL         string `json:"url,omitempty"`
	// Stage is one of: received | stage0 | stage1 | stage2 | stage3 | verdict.
	Stage string `json:"stage"`
	// Status is one of: running | passed | surfaced | dropped | error.
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Stream is the model's output accumulated so far for this stage, when the
	// stage is being streamed live (Stage 3 deep triage). Re-emitted as it grows;
	// the server updates the stage row in place rather than appending.
	Stream    string `json:"stream,omitempty"`
	At        string `json:"at"`
	ElapsedMs int64  `json:"elapsed_ms"`
}

// OperatorIdentity is the set of identifiers that count as "the operator" on
// a connector (Slack user IDs, GitHub logins, email addresses). Stage 0 uses
// it to drop self-authored events.
type OperatorIdentity struct {
	UserIDs []string
}

// ContextMessage is one message inside a fetched thread.
type ContextMessage struct {
	Kind      string `json:"kind,omitempty"`
	Author    string `json:"author"`
	Text      string `json:"text"`
	TS        string `json:"ts"`
	Permalink string `json:"permalink,omitempty"`
}

// ThreadContext is a normalized bundle of richer context a connector fetches
// on demand for the deep triage stage.
type ThreadContext struct {
	Source          string           `json:"source,omitempty"`
	ThreadKey       string           `json:"thread_key,omitempty"`
	Summary         string           `json:"summary,omitempty"`
	Participants    []string         `json:"participants,omitempty"`
	Timestamps      []string         `json:"timestamps,omitempty"`
	Parent          *ContextMessage  `json:"parent,omitempty"`
	Messages        []ContextMessage `json:"messages,omitempty"`
	AttachmentPaths []string         `json:"attachment_paths,omitempty"`
	Permalink       string           `json:"permalink,omitempty"`
	FetchStatus     string           `json:"fetch_status,omitempty"`
	FetchError      string           `json:"fetch_error,omitempty"`
}

// PriorUnderstanding is the model-facing snapshot of a thread's persistent
// running understanding (productdb.ThreadState), fed into incremental deep-triage so
// Stage 3 updates its prior decision with the new delta instead of cold
// re-deriving. The cascade builds it from the thread-state row read back before
// triaging; a nil *PriorUnderstanding means this is the thread's first triage.
type PriorUnderstanding struct {
	Action          string   `json:"action,omitempty"`
	Confidence      float64  `json:"confidence"`
	Reason          string   `json:"reason,omitempty"`
	Summary         string   `json:"summary,omitempty"`
	EventCount      int      `json:"event_count"`
	OperatorActions []string `json:"operator_actions,omitempty"`
	OperatorReplies []string `json:"operator_replies,omitempty"`
	// Corrections are authoritative operator-supplied context ("the steerer got
	// this wrong, here's what it means"). Deep triage treats them as ground truth,
	// above its own inference and retrieved history.
	Corrections []string `json:"corrections,omitempty"`
}

// RetrievedDoc is one related-context hit pulled from the FTS index (KB facts,
// task briefs/updates) for the deep triager — the "elsewhere" layer that lets it
// resolve references to things decided in other conversations or recorded long
// ago. Snippet is already bounded by the FTS snippet() window.
type RetrievedDoc struct {
	Type    string `json:"type,omitempty"`
	Slug    string `json:"slug,omitempty"`
	Name    string `json:"name,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

// IncrementalContext carries the two context layers Stage 3 gains beyond the
// current-thread pack: the thread's prior running understanding (layer 2) and
// retrieved cross-conversation/KB history (layer 3). A zero value degrades to the
// prior cold-classification behavior, so older call sites stay correct.
type IncrementalContext struct {
	Prior     *PriorUnderstanding
	Retrieved []RetrievedDoc
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
