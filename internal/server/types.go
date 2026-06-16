package server

import (
	"database/sql"
	"encoding/json"
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
	// sessionToken gates the local data plane (WS handshakes + state-changing
	// /api/* routes). Minted with crypto/rand in New(); see session_token.go.
	sessionToken   string
	terminals      *terminalHub
	events         *eventHub
	reconcile      *livenessReconciler
	kbDistiller    *kbDistiller
	kbDreamer      *kbDreamer
	kbWatcher      *kbWatcher
	transcripts    *transcriptCache
	caches         *uiCaches
	slackListener  *monitor.SlackListener
	githubListener *monitor.GitHubListener
	// cascade is the steering (attention-router) triage brain the dispatcher
	// routes untracked messages into. Held on the server so the steerer
	// backfill (ListenAndServe) can replay catch-up messages through the SAME
	// cascade via ObserveBatch. Nil when no DB is configured.
	cascade *steering.Cascade
	// steeringRuns holds the recent + in-flight cascade runs (the live CI-style
	// stage view). Populated by the cascade's Progress hook; read by the inbox
	// UI over /api/steering/runs + the steering_stage WS event. Always non-nil
	// after New().
	steeringRuns  *steeringRunStore
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
	// playbookSched is the in-process heartbeat that fires due scheduled
	// playbook runs by shelling out to `flow playbook tick-due`. Nil when
	// disabled or when there's no flow binary to invoke.
	playbookSched *playbookScheduler
	// ownerSched is the owner twin of playbookSched: it fires due owner ticks by
	// shelling out to `flow owner tick-due`. Nil when disabled or when there's
	// no flow binary to invoke.
	ownerSched *ownerScheduler
	// respawn debounces agent respawns triggered by inbox events.
	respawn *respawnGate

	// zrok manages an optional `zrok share reserved` subprocess when
	// FLOW_ZROK_AUTO_START is enabled. Always non-nil after New().
	zrok *zrokManager

	// slackOAuth is the in-flight Connect-Slack install attempt (the
	// ephemeral TLS callback listener + state nonce). At most one at a time;
	// guarded by slackSetupMu. Nil when no install is in progress.
	slackSetupMu sync.Mutex
	slackOAuth   *slackOAuthDance

	// githubSetup is the in-flight Connect-GitHub App-manifest attempt: the
	// state nonce and chosen install target, kept server-side so the manifest
	// conversion callback can validate the redirect. Guarded by githubSetupMu;
	// nil when no setup is in progress.
	githubSetupMu sync.Mutex
	githubSetup   *githubManifestPending

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

	// steererSlots serializes concurrent deliveries to the same per-channel
	// steerer session and carries its lifecycle state (GAP-5). Keyed by chat slug.
	// Only used when FLOW_STEERING_SESSIONS is enabled.
	steererSlots   map[string]*steererSlot
	steererSlotsMu sync.Mutex
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

type OwnerView struct {
	Slug           string        `json:"slug"`
	Name           string        `json:"name"`
	WorkDir        string        `json:"work_dir"`
	WorkdirKnown   *WorkdirKnown `json:"workdir_known"`
	ProjectSlug    *string       `json:"project_slug,omitempty"`
	Status         string        `json:"status"`
	Every          string        `json:"every"`
	NextWakeAt     *string       `json:"next_wake_at,omitempty"`
	NextDue        bool          `json:"next_due"`
	LastTickAt     *string       `json:"last_tick_at,omitempty"`
	LastTickStatus *string       `json:"last_tick_status,omitempty"`
	TickPID        *int64        `json:"tick_pid,omitempty"`
	TickStarted    *string       `json:"tick_started,omitempty"`
	Harness        string        `json:"harness"`
	CreatedAt      string        `json:"created_at"`
	UpdatedAt      string        `json:"updated_at"`
	ArchivedAt     *string       `json:"archived_at,omitempty"`
	CharterPath    string        `json:"charter_path,omitempty"`
}

// OwnerJournalNote is one dated note the owner wrote under owners/<slug>/updates/.
type OwnerJournalNote struct {
	Filename string `json:"filename"`
	Path     string `json:"path"`
	MTime    string `json:"mtime"`
	Content  string `json:"content"`
}

// OwnerTaskRow is a compact, live view of one task the owner controls
// (tagged owner:<slug>) — enough to show what a tick dispatched and where it is.
type OwnerTaskRow struct {
	Slug          string  `json:"slug"`
	Name          string  `json:"name"`
	Status        string  `json:"status"`
	Priority      string  `json:"priority"`
	AutoRunStatus *string `json:"auto_run_status,omitempty"`
	WorktreePath  *string `json:"worktree_path,omitempty"`
	HasSession    bool    `json:"has_session"`
	IsQuestion    bool    `json:"is_question"`
}

// OwnerTickRecord is one past tick the owner ran — its streamed activity log,
// kept on disk under owners/<slug>/ticks/, so each tick stays revisitable.
type OwnerTickRecord struct {
	Filename  string `json:"filename"`
	Path      string `json:"path"`
	StartedAt string `json:"started_at"`
	Status    string `json:"status"`
	Content   string `json:"content"`
}

// OwnerDetailView is the single-owner payload: the base view plus the
// observability surface (journal notes, owned-task status, tick history, and
// the latest tick log for the live-stream view while a tick is running).
type OwnerDetailView struct {
	OwnerView
	Journal     []OwnerJournalNote `json:"journal"`
	Tasks       []OwnerTaskRow     `json:"tasks"`
	Ticks       []OwnerTickRecord  `json:"ticks"`
	TickLogTail string             `json:"tick_log_tail,omitempty"`
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
	Model               string        `json:"model"`
	Tags                []string      `json:"tags"`
	SessionID           *string       `json:"session_id"`
	SessionProvider     *string       `json:"session_provider"`
	Harness             *string       `json:"harness,omitempty"`
	SessionStarted      *string       `json:"session_started"`
	SessionLastResumed  *string       `json:"session_last_resumed"`
	SessionPath         *string       `json:"session_path,omitempty"`
	Live                bool          `json:"live"`
	RuntimeStatus       *string       `json:"runtime_status,omitempty"`
	AutoRunStatus       *string       `json:"auto_run_status,omitempty"`
	AutoRunPID          *int64        `json:"auto_run_pid,omitempty"`
	AutoRunStarted      *string       `json:"auto_run_started,omitempty"`
	AutoRunFinished     *string       `json:"auto_run_finished,omitempty"`
	AutoRunLog          *string       `json:"auto_run_log,omitempty"`
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

// BrainRunView is one persisted or compatibility run ledger row as surfaced to
// the UI. The list endpoint returns the summary fields; the detail endpoint
// also fills the JSON evidence payloads.
type BrainRunView struct {
	RunID          string          `json:"run_id"`
	FamilySlug     string          `json:"family_slug"`
	TaskSlug       string          `json:"task_slug"`
	TaskName       string          `json:"task_name,omitempty"`
	TaskStatus     string          `json:"task_status,omitempty"`
	PlanID         *string         `json:"plan_id,omitempty"`
	Role           string          `json:"role"`
	Provider       string          `json:"provider"`
	RequestedModel *string         `json:"requested_model,omitempty"`
	RequestedTier  *string         `json:"requested_tier,omitempty"`
	ResolvedModel  *string         `json:"resolved_model,omitempty"`
	PermissionMode string          `json:"permission_mode"`
	Status         string          `json:"status"`
	PID            *int64          `json:"pid,omitempty"`
	SessionID      *string         `json:"session_id,omitempty"`
	LogPath        *string         `json:"log_path,omitempty"`
	InputSummary   *string         `json:"input_summary,omitempty"`
	OutputJSON     json.RawMessage `json:"output_json,omitempty"`
	EvidenceJSON   json.RawMessage `json:"evidence_json,omitempty"`
	ErrorText      *string         `json:"error_text,omitempty"`
	StartedAt      *string         `json:"started_at,omitempty"`
	FinishedAt     *string         `json:"finished_at,omitempty"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
	Legacy         bool            `json:"legacy,omitempty"`
}

type BrainRunsResponse struct {
	TaskSlug   string         `json:"task_slug"`
	FamilySlug string         `json:"family_slug"`
	Runs       []BrainRunView `json:"runs"`
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
	Provider   string  `json:"provider"`
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
	// Scheduling. Schedule is the operator's phrase ("every 6 hours"),
	// ScheduleSpec the canonical cron. SchedulePaused => retained but not
	// firing. All nil/false when the playbook has no schedule.
	Schedule        *string `json:"schedule"`
	ScheduleSpec    *string `json:"schedule_spec"`
	SchedulePaused  bool    `json:"schedule_paused"`
	NextFireAt      *string `json:"next_fire_at"`
	LastFiredAt     *string `json:"last_fired_at"`
	LastFireRunSlug *string `json:"last_fire_run_slug"`
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
	ID               string                  `json:"id"`
	CreatedAt        string                  `json:"created_at"`
	Origin           string                  `json:"origin"`
	Source           string                  `json:"source"`
	Channel          string                  `json:"channel,omitempty"`
	ChannelType      string                  `json:"channel_type,omitempty"`
	Author           string                  `json:"author,omitempty"`
	ThreadKey        string                  `json:"thread_key,omitempty"`
	TextPreview      string                  `json:"text_preview,omitempty"`
	Disposition      string                  `json:"disposition"`
	StageReached     string                  `json:"stage_reached"`
	DropReason       string                  `json:"drop_reason,omitempty"`
	Stage1Relevant   *bool                   `json:"stage1_relevant,omitempty"`
	Stage1Reason     string                  `json:"stage1_reason,omitempty"`
	Stage2Action     string                  `json:"stage2_action,omitempty"`
	Stage2Confidence float64                 `json:"stage2_confidence,omitempty"`
	Stage3Action     string                  `json:"stage3_action,omitempty"`
	Stage3Confidence float64                 `json:"stage3_confidence,omitempty"`
	FinalAction      string                  `json:"final_action,omitempty"`
	FinalConfidence  float64                 `json:"final_confidence,omitempty"`
	FeedItemID       string                  `json:"feed_item_id,omitempty"`
	LinkedTask       string                  `json:"linked_task,omitempty"`
	MatchedTask      *AttentionTaskMatchView `json:"matched_task,omitempty"`
	Error            string                  `json:"error,omitempty"`
	AutonomyAction   string                  `json:"autonomy_action,omitempty"`
	AutonomyDecision string                  `json:"autonomy_decision,omitempty"`
	AutonomyReason   string                  `json:"autonomy_reason,omitempty"`
	LatencyMS        int64                   `json:"latency_ms"`
	Model            string                  `json:"model,omitempty"`
	ChannelName      string                  `json:"channel_name,omitempty"`
	AuthorName       string                  `json:"author_name,omitempty"`
	Text             string                  `json:"text,omitempty"` // mentions resolved, full (not just preview)
	Permalink        string                  `json:"permalink,omitempty"`
	TS               string                  `json:"ts,omitempty"`
	TeamID           string                  `json:"team_id,omitempty"`
	URL              string                  `json:"url,omitempty"` // connector permalink (GitHub item URL, etc.)
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
	ToolInput        string `json:"tool_input,omitempty"`
	ToolResultText   string `json:"tool_result_text,omitempty"`
	ToolUseID        string `json:"tool_use_id,omitempty"`
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
