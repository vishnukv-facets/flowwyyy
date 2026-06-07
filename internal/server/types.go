package server

import (
	"database/sql"
	"flow/internal/briefing"
	"net/http"
	"sync"
	"time"

	"flow/internal/monitor"
	"flow/internal/steering"
)

type Config struct {
	DB          *sql.DB
	FlowRoot    string
	Version     string
	CommandPath string
	HookURL     string
	// DisableQuote turns off the anime quote beside the Mission Control
	// greeting (flow ui serve --no-quote). The endpoint then returns an empty
	// quote, which the UI hides — falling back to the static subtitle.
	DisableQuote bool
}

type Server struct {
	cfg            Config
	terminals      *terminalHub
	events         *eventHub
	reconcile      *livenessReconciler
	transcripts    *transcriptCache
	caches         *uiCaches
	slackListener  *monitor.SlackListener
	githubListener *monitor.GitHubListener
	// cascade is the steering (attention-router) triage brain the dispatcher
	// routes untracked messages into. Held on the server so the steerer
	// backfill (ListenAndServe) can replay catch-up messages through the SAME
	// cascade via ObserveBatch. Nil when no DB is configured.
	cascade       *steering.Cascade
	inboxMonitors *inboxMonitorManager
	dbWatcher     *dbWatcher
	// nameResolver maps Slack user/channel IDs to display names for the
	// Inbox UI, caching lookups across requests. Nil when no Slack token is
	// configured; all of its methods are nil-safe.
	nameResolver *monitor.SlackNameResolver
	// slackPermalinker resolves (channel, message-ts) → canonical https Slack
	// permalink via chat.getPermalink (needs only channel+ts, no team_id), so a
	// real "Open in Slack" link works even for items captured before the
	// channel/ts/team_id columns existed. Nil when no token; methods nil-safe.
	slackPermalinker *monitor.SlackPermalinker
	// monitorReconcile keeps persistent background monitors converged with the
	// set of tasks that need one (origin/branch-linked + active), restoring
	// them on boot and recreating any that die.
	monitorReconcile *monitorReconciler
	// respawn debounces agent respawns triggered by inbox events.
	respawn *respawnGate

	// slackOAuth is the in-flight Connect-Slack install attempt (the
	// ephemeral TLS callback listener + state nonce). At most one at a time;
	// guarded by slackSetupMu. Nil when no install is in progress.
	slackSetupMu sync.Mutex
	slackOAuth   *slackOAuthDance

	// quote{Mu,Key,Val} cache the Mission Control anime quote per
	// (date + greeting bucket) so the external animechan API is called at most
	// once per greeting change — see handleQuote.
	quoteMu  sync.Mutex
	quoteKey string
	quoteVal QuoteView

	// searchSync{Mu,At,ing} serialize, throttle, and de-dupe the search-index
	// refresh. The refresh is a filesystem walk + FTS rebuild that takes
	// seconds. A scope's FIRST build is synchronous (so the query that needs it
	// returns correct results); later refreshes run in the BACKGROUND, never on
	// the /api/search request path. searchSyncAt records the last successful
	// sync per scope (keyed by scope string); searchSyncing is the in-flight
	// guard that stops a keystroke from stacking a second goroutine or racing a
	// second SQLite writer — see syncSearchThrottled.
	searchSyncMu  sync.Mutex
	searchSyncAt  map[string]time.Time
	searchSyncing bool

	// flowDBQuickCheck caches the last authoritative integrity check so the
	// sidebar can report a recent compact precheck when its short live check
	// times out on a large database.
	flowDBQuickCheckMu      sync.Mutex
	flowDBQuickCheck        cachedFlowDBQuickCheck
	flowDBQuickCheckTimeout time.Duration

	// apiMux is the data-plane mux (/api/* routes only), built once and
	// reused by both the HTTP Handler and the WebSocket-RPC bridge so the
	// UI can run every data request and mutation over a single socket
	// (see rpc_bridge.go) without duplicating route wiring.
	apiOnce sync.Once
	apiMux  http.Handler
}

type cachedFlowDBQuickCheck struct {
	Path      string
	Result    string
	Source    string
	CheckedAt time.Time
}

type HealthView struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version"`
	FlowRoot string `json:"flow_root"`
}

type FileRef struct {
	Filename string `json:"filename"`
	Path     string `json:"path"`
	MTime    string `json:"mtime"`
	Size     int64  `json:"size"`
}

type FSEntriesView struct {
	Path        string         `json:"path"`
	DisplayPath string         `json:"display_path"`
	Parent      *string        `json:"parent"`
	IsGit       bool           `json:"is_git"`
	Breadcrumbs []FSBreadcrumb `json:"breadcrumbs"`
	Entries     []FSEntryView  `json:"entries"`
}

type FSBreadcrumb struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type FSEntryView struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	DisplayPath string `json:"display_path"`
	IsDir       bool   `json:"is_dir"`
	IsGit       bool   `json:"is_git"`
	Hidden      bool   `json:"hidden"`
}

type WorkdirKnown struct {
	Name      *string `json:"name,omitempty"`
	GitRemote *string `json:"git_remote,omitempty"`
}

type TaskView struct {
	Slug                string        `json:"slug"`
	Name                string        `json:"name"`
	ProjectSlug         *string       `json:"project_slug"`
	Status              string        `json:"status"`
	Kind                string        `json:"kind"`
	PlaybookSlug        *string       `json:"playbook_slug"`
	ParentSlug          *string       `json:"parent_slug"`
	Parent              *TaskSummary  `json:"parent,omitempty"`
	Parents             []TaskSummary `json:"parents,omitempty"`
	Children            []TaskSummary `json:"children,omitempty"`
	ForkedFromSlug      *string       `json:"forked_from_slug,omitempty"`
	ForkedFrom          *TaskSummary  `json:"forked_from,omitempty"`
	ForkReason          *string       `json:"fork_reason,omitempty"`
	Forks               []TaskSummary `json:"forks,omitempty"`
	Priority            string        `json:"priority"`
	WorkDir             string        `json:"work_dir"`
	WorktreePath        *string       `json:"worktree_path,omitempty"`
	WorkdirKnown        *WorkdirKnown `json:"workdir_known"`
	WaitingOn           *string       `json:"waiting_on"`
	DueDate             *string       `json:"due_date"`
	DueInfo             *string       `json:"due_info"`
	Assignee            *string       `json:"assignee"`
	PermissionMode      string        `json:"permission_mode"`
	Tags                []string      `json:"tags"`
	SessionID           *string       `json:"session_id"`
	SessionProvider     *string       `json:"session_provider"`
	SessionStarted      *string       `json:"session_started"`
	SessionLastResumed  *string       `json:"session_last_resumed"`
	SessionPath         *string       `json:"session_path,omitempty"`
	Live                bool          `json:"live"`
	RuntimeStatus       *string       `json:"runtime_status,omitempty"`
	DaysInStatus        int           `json:"days_in_status"`
	StaleDays           *int          `json:"stale_days"`
	TemporalSummary     string        `json:"temporal_summary"`
	InboxPath           string        `json:"inbox_path,omitempty"`
	InboxUnreadCount    int           `json:"inbox_unread_count"`
	InboxSeenAt         *string       `json:"inbox_seen_at,omitempty"`
	CreatedAt           string        `json:"created_at"`
	UpdatedAt           string        `json:"updated_at"`
	ArchivedAt          *string       `json:"archived_at"`
	DeletedAt           *string       `json:"deleted_at"`
	BriefPath           string        `json:"brief_path"`
	Updates             []FileRef     `json:"updates"`
	AuxFiles            []FileRef     `json:"aux_files"`
	TranscriptAvailable bool          `json:"transcript_available"`
}

// InboxEntry is one parsed message from a task's inbox.md.
type InboxEntry struct {
	Timestamp string `json:"timestamp"`
	Sender    string `json:"sender"`
	Body      string `json:"body"`
}

// MonitorSyncStateView is one source's poll status as the Inbox UI sees
// it. LastSyncAt is an RFC3339 string when the source has been polled at
// least once, empty otherwise. LastError is empty when LastStatus is "ok"
// or "unknown".
type MonitorSyncStateView struct {
	Source     string `json:"source"`
	LastSyncAt string `json:"last_sync_at,omitempty"`
	LastStatus string `json:"last_status"`
	LastError  string `json:"last_error,omitempty"`
	IsSyncing  bool   `json:"is_syncing"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

// MonitorSyncStateResponse wraps the list so future fields (e.g.
// next_scheduled_poll_at, default_interval) can be added without
// breaking the existing []shape contract.
type MonitorSyncStateResponse struct {
	States []MonitorSyncStateView `json:"states"`
}

// InboxView is the GET /api/tasks/<slug>/inbox response shape.
type InboxView struct {
	Slug        string       `json:"slug"`
	Path        string       `json:"path"`
	UnreadCount int          `json:"unread_count"`
	SeenAt      *string      `json:"seen_at,omitempty"`
	Entries     []InboxEntry `json:"entries"`
}

// InboxFeedEntry is one inbox.md row enriched with the task it belongs to.
// Used by the global /api/inbox aggregation. BodySnippet is a truncated
// preview (full body still in Body so the UI can expand on demand).
type InboxFeedEntry struct {
	TaskSlug    string  `json:"task_slug"`
	TaskName    string  `json:"task_name"`
	ProjectSlug *string `json:"project_slug,omitempty"`
	Status      string  `json:"status"`
	Timestamp   string  `json:"timestamp"`
	Sender      string  `json:"sender"`
	Body        string  `json:"body"`
	BodySnippet string  `json:"body_snippet"`
	Unread      bool    `json:"unread"`
	// Source is "slack" | "github" | "" — derived from the event, used by the
	// grouped conversation list to show a source icon without re-parsing.
	Source string `json:"source,omitempty"`
	// Live reports whether the task's session is currently running, so the
	// conversation list can show a live indicator. Matches TaskView.Live.
	Live bool `json:"live"`
	// Monitored reports whether a persistent background monitor is currently
	// running for the task (independent of whether a session is live).
	Monitored bool `json:"monitored"`
}

// InboxConversation is the GET /api/inbox/conversation?slug=<task> response:
// one task's full thread of inbox events, with every Slack ID already
// resolved to a human-readable name. Powers the Inbox right pane.
type InboxConversation struct {
	Slug        string                     `json:"slug"`
	Name        string                     `json:"name"`
	ProjectSlug *string                    `json:"project_slug,omitempty"`
	Status      string                     `json:"status"`
	Provider    string                     `json:"provider"`
	Live        bool                       `json:"live"`
	Monitored   bool                       `json:"monitored"`
	Source      string                     `json:"source"`                 // slack | github | mixed | ""
	ChannelName string                     `json:"channel_name,omitempty"` // resolved; never a raw ID
	Messages    []InboxConversationMessage `json:"messages"`               // chronological (oldest first)
}

// InboxConversationMessage is one rendered message in a conversation thread.
// SenderName and Body are guaranteed free of raw Slack IDs.
type InboxConversationMessage struct {
	Source     string `json:"source"`      // slack | github
	Kind       string `json:"kind"`        // message, app_mention, pr_review_comment, …
	SenderName string `json:"sender_name"` // resolved display name or login; never a raw ID
	Timestamp  string `json:"timestamp"`   // RFC3339 (enqueued_at) when available
	Title      string `json:"title"`       // humanised kind, e.g. "PR review requested"
	Body       string `json:"body"`        // message text with mentions/links cleaned
	Permalink  string `json:"permalink,omitempty"`
	Reaction   string `json:"reaction,omitempty"`
}

// InboxFeed is the GET /api/inbox response shape — a global aggregation of
// every task's inbox.md entries, newest first.
type InboxFeed struct {
	Entries     []InboxFeedEntry `json:"entries"`
	UnreadCount int              `json:"unread_count"`
	TaskCount   int              `json:"task_count"`
	GeneratedAt string           `json:"generated_at"`
}

// LifecycleEvent is one row of the per-session event timeline.
type LifecycleEvent struct {
	Time     string `json:"time"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Body     string `json:"body,omitempty"`
	Severity string `json:"severity,omitempty"`
}

// LifecycleView is the GET /api/tasks/<slug>/lifecycle response shape.
type LifecycleView struct {
	Slug   string           `json:"slug"`
	Events []LifecycleEvent `json:"events"`
}

type TaskSummary struct {
	Slug        string  `json:"slug"`
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	Priority    string  `json:"priority"`
	ProjectSlug *string `json:"project_slug"`
	UpdatedAt   string  `json:"updated_at"`
}

type TaskCounts struct {
	Total      int `json:"total"`
	InProgress int `json:"in_progress"`
	Backlog    int `json:"backlog"`
	Done       int `json:"done"`
}

type ProjectView struct {
	Slug         string        `json:"slug"`
	Name         string        `json:"name"`
	Status       string        `json:"status"`
	Priority     string        `json:"priority"`
	WorkDir      string        `json:"work_dir"`
	WorkdirKnown *WorkdirKnown `json:"workdir_known"`
	CreatedAt    string        `json:"created_at"`
	UpdatedAt    string        `json:"updated_at"`
	ArchivedAt   *string       `json:"archived_at"`
	DeletedAt    *string       `json:"deleted_at"`
	TaskCounts   TaskCounts    `json:"task_counts"`
	RecentTasks  []TaskSummary `json:"recent_tasks"`
	BriefPath    string        `json:"brief_path"`
	Updates      []FileRef     `json:"updates"`
	AuxFiles     []FileRef     `json:"aux_files"`
}

type RunSummary struct {
	Slug       string  `json:"slug"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Priority   string  `json:"priority"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
	StartedAt  *string `json:"started_at"`
	ArchivedAt *string `json:"archived_at"`
	DeletedAt  *string `json:"deleted_at"`
}

type PlaybookView struct {
	Slug        string       `json:"slug"`
	Name        string       `json:"name"`
	ProjectSlug *string      `json:"project_slug"`
	WorkDir     string       `json:"work_dir"`
	CreatedAt   string       `json:"created_at"`
	UpdatedAt   string       `json:"updated_at"`
	ArchivedAt  *string      `json:"archived_at"`
	DeletedAt   *string      `json:"deleted_at"`
	BriefPath   string       `json:"brief_path"`
	Updates     []FileRef    `json:"updates"`
	AuxFiles    []FileRef    `json:"aux_files"`
	RecentRuns  []RunSummary `json:"recent_runs"`
	RunCount7d  int          `json:"run_count_7d"`
	RunDays30   []int        `json:"run_days_30"`
}

type KBFileView struct {
	Filename string `json:"filename"`
	Path     string `json:"path"`
	MTime    string `json:"mtime"`
	Size     int64  `json:"size"`
	Entries  int    `json:"entries"`
	Preview  string `json:"preview"`
	Content  string `json:"content"`
}

// SteeringTraceView is the UI shape of a steering_trace row.
type SteeringTraceView struct {
	ID               string  `json:"id"`
	CreatedAt        string  `json:"created_at"`
	Origin           string  `json:"origin"`
	Source           string  `json:"source"`
	Channel          string  `json:"channel,omitempty"`
	ChannelType      string  `json:"channel_type,omitempty"`
	Author           string  `json:"author,omitempty"`
	ThreadKey        string  `json:"thread_key,omitempty"`
	TextPreview      string  `json:"text_preview,omitempty"`
	Disposition      string  `json:"disposition"`
	StageReached     string  `json:"stage_reached"`
	DropReason       string  `json:"drop_reason,omitempty"`
	Stage1Relevant   *bool   `json:"stage1_relevant,omitempty"`
	Stage1Reason     string  `json:"stage1_reason,omitempty"`
	Stage2Action     string  `json:"stage2_action,omitempty"`
	Stage2Confidence float64 `json:"stage2_confidence,omitempty"`
	Stage3Action     string  `json:"stage3_action,omitempty"`
	Stage3Confidence float64 `json:"stage3_confidence,omitempty"`
	FinalAction      string  `json:"final_action,omitempty"`
	FinalConfidence  float64 `json:"final_confidence,omitempty"`
	FeedItemID       string  `json:"feed_item_id,omitempty"`
	Error            string  `json:"error,omitempty"`
	AutonomyAction   string  `json:"autonomy_action,omitempty"`
	AutonomyDecision string  `json:"autonomy_decision,omitempty"`
	AutonomyReason   string  `json:"autonomy_reason,omitempty"`
	LatencyMS        int64   `json:"latency_ms"`
	Model            string  `json:"model,omitempty"`
	ChannelName      string  `json:"channel_name,omitempty"`
	AuthorName       string  `json:"author_name,omitempty"`
	Text             string  `json:"text,omitempty"` // mentions resolved, full (not just preview)
	Permalink        string  `json:"permalink,omitempty"`
	TS               string  `json:"ts,omitempty"`
	TeamID           string  `json:"team_id,omitempty"`
	URL              string  `json:"url,omitempty"` // connector permalink (GitHub item URL, etc.)
}

// SteeringFunnelView is the funnel aggregate for the trace panel.
type SteeringFunnelView struct {
	Observed      int `json:"observed"`
	DroppedStage0 int `json:"dropped_stage0"`
	DroppedCache  int `json:"dropped_cache"`
	DroppedStage1 int `json:"dropped_stage1"`
	DroppedStage2 int `json:"dropped_stage2"`
	Surfaced      int `json:"surfaced"`
	Errors        int `json:"errors"`
}

// AttentionTraceResponse is the /api/attention/trace payload.
type AttentionTraceResponse struct {
	Funnel SteeringFunnelView  `json:"funnel"`
	Items  []SteeringTraceView `json:"items"`
}

type AttentionTaskMatchView struct {
	Slug            string `json:"slug"`
	Name            string `json:"name,omitempty"`
	Status          string `json:"status,omitempty"`
	Priority        string `json:"priority,omitempty"`
	ProjectSlug     string `json:"project_slug,omitempty"`
	SessionProvider string `json:"session_provider,omitempty"`
}

type AttentionWhyView struct {
	Source            string                  `json:"source"`
	ContextSummary    string                  `json:"context_summary,omitempty"`
	FetchStatus       string                  `json:"fetch_status,omitempty"`
	FetchError        string                  `json:"fetch_error,omitempty"`
	EvidenceCount     int                     `json:"evidence_count,omitempty"`
	Participants      []string                `json:"participants,omitempty"`
	ParentPreview     string                  `json:"parent_preview,omitempty"`
	LatestPreview     string                  `json:"latest_preview,omitempty"`
	Reason            string                  `json:"reason,omitempty"`
	Confidence        float64                 `json:"confidence"`
	StageReached      string                  `json:"stage_reached,omitempty"`
	StageAction       string                  `json:"stage_action,omitempty"`
	StageConfidence   float64                 `json:"stage_confidence,omitempty"`
	Stage1Relevant    *bool                   `json:"stage1_relevant,omitempty"`
	SuggestedProject  string                  `json:"suggested_project,omitempty"`
	SuggestedPriority string                  `json:"suggested_priority,omitempty"`
	MatchedTask       *AttentionTaskMatchView `json:"matched_task,omitempty"`
}

type AttentionActionPreview struct {
	Action      string `json:"action"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Target      string `json:"target,omitempty"`
	Primary     bool   `json:"primary,omitempty"`
	Destructive bool   `json:"destructive,omitempty"`
}

type AttentionHandoffView struct {
	ID               string `json:"id"`
	FeedItemID       string `json:"feed_item_id"`
	Sender           string `json:"sender"`
	Receiver         string `json:"receiver"`
	RequestedVerdict string `json:"requested_verdict"`
	Status           string `json:"status"`
	Reason           string `json:"reason,omitempty"`
	RequestedAt      string `json:"requested_at"`
	ExpiresAt        string `json:"expires_at"`
	RespondedAt      string `json:"responded_at,omitempty"`
}

// AttentionItemView is the UI shape of an attention_feed row.
type AttentionItemView struct {
	ID                string                   `json:"id"`
	Source            string                   `json:"source"`
	ThreadKey         string                   `json:"thread_key"`
	Summary           string                   `json:"summary"`
	SuggestedAction   string                   `json:"suggested_action"`
	MatchedTask       string                   `json:"matched_task,omitempty"`
	SuggestedProject  string                   `json:"suggested_project,omitempty"`
	SuggestedPriority string                   `json:"suggested_priority,omitempty"`
	Urgency           string                   `json:"urgency,omitempty"`
	IsVIP             bool                     `json:"is_vip"`
	Confidence        float64                  `json:"confidence"`
	Draft             string                   `json:"draft,omitempty"`
	Reason            string                   `json:"reason,omitempty"`
	Status            string                   `json:"status"`
	LinkedTask        string                   `json:"linked_task,omitempty"`
	Retriaging        bool                     `json:"retriaging,omitempty"`
	CreatedAt         string                   `json:"created_at"`
	ActedAt           string                   `json:"acted_at,omitempty"`
	Channel           string                   `json:"channel,omitempty"`
	ChannelType       string                   `json:"channel_type,omitempty"`
	ChannelName       string                   `json:"channel_name,omitempty"`
	Author            string                   `json:"author,omitempty"`
	AuthorName        string                   `json:"author_name,omitempty"`
	Permalink         string                   `json:"permalink,omitempty"`
	Why               AttentionWhyView         `json:"why"`
	ActionPreviews    []AttentionActionPreview `json:"action_previews,omitempty"`
	Handoff           *AttentionHandoffView    `json:"handoff,omitempty"`
}

type WorkdirView struct {
	Path           string  `json:"path"`
	Name           *string `json:"name"`
	Description    *string `json:"description"`
	GitRemote      *string `json:"git_remote"`
	LastUsedAt     *string `json:"last_used_at"`
	CreatedAt      string  `json:"created_at"`
	TasksUsingThis int     `json:"tasks_using_this"`
	Untouched30d   bool    `json:"untouched_30d"`
}

type OverviewView struct {
	LiveSessions        []TaskView        `json:"live_sessions"`
	InFlight            []TaskView        `json:"in_flight"`
	HighPriorityBacklog []TaskView        `json:"high_priority_backlog"`
	Waiting             []TaskView        `json:"waiting"`
	Stale               []TaskView        `json:"stale"`
	ActivePlaybooks     []PlaybookView    `json:"active_playbooks"`
	Briefing            briefing.Briefing `json:"briefing"`
}

type SearchResponse struct {
	Query       string         `json:"query"`
	Tasks       []SearchResult `json:"tasks"`
	Projects    []SearchResult `json:"projects"`
	Playbooks   []SearchResult `json:"playbooks"`
	Updates     []SearchResult `json:"updates"`
	Transcripts []SearchResult `json:"transcripts"`
	Memories    []SearchResult `json:"memories"`
}

type SearchResult struct {
	Type       string `json:"type"`
	Scope      string `json:"scope,omitempty"`
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	Snippet    string `json:"snippet"`
	SourcePath string `json:"source_path,omitempty"`
}

type AskFlowRequest struct {
	Query string `json:"query"`
}

type AskFlowCitation struct {
	Type       string `json:"type"`
	ID         string `json:"id,omitempty"`
	Slug       string `json:"slug,omitempty"`
	Title      string `json:"title"`
	URL        string `json:"url,omitempty"`
	SourcePath string `json:"source_path,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
}

type AskFlowResponse struct {
	Query     string            `json:"query"`
	Intent    string            `json:"intent"`
	Answer    string            `json:"answer"`
	Citations []AskFlowCitation `json:"citations"`
}

type TranscriptResponse struct {
	Available bool              `json:"available"`
	Message   string            `json:"message,omitempty"`
	Entries   []TranscriptEntry `json:"entries"`
}

type TranscriptEntry struct {
	Type             string `json:"type"`
	Text             string `json:"text,omitempty"`
	ToolName         string `json:"tool_name,omitempty"`
	ToolInputSummary string `json:"tool_input_summary,omitempty"`
	ToolResultText   string `json:"tool_result_text,omitempty"`
	IsError          bool   `json:"is_error,omitempty"`
	ByteOffset       int64  `json:"byte_offset"`
	Timestamp        string `json:"timestamp,omitempty"`
}

type WorkspaceNode struct {
	Name     string          `json:"name"`
	Path     string          `json:"path"`
	Type     string          `json:"type"`
	Size     int64           `json:"size,omitempty"`
	Children []WorkspaceNode `json:"children,omitempty"`
}

type WorkspaceView struct {
	Root      string          `json:"root"`
	Exists    bool            `json:"exists"`
	Truncated bool            `json:"truncated"`
	Nodes     []WorkspaceNode `json:"nodes"`
}
