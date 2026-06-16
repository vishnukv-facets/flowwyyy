// TypeScript shapes mirroring the Go server view structs (internal/server).
// Loose where it buys nothing to be strict; the server is the source of truth.

export interface FileRef {
  filename: string
  path: string
  mtime: string
  size: number
}

export interface TaskSummary {
  slug: string
  name: string
  status: string
  priority: string
  project_slug: string | null
  updated_at: string
}

export interface WorkdirKnown {
  name?: string
  git_remote?: string
}

export interface OwnerView {
  slug: string
  name: string
  work_dir: string
  workdir_known: WorkdirKnown | null
  project_slug?: string | null
  status: string
  every: string
  next_wake_at?: string | null
  next_due: boolean
  last_tick_at?: string | null
  last_tick_status?: string | null
  tick_pid?: number
  tick_started?: string | null
  harness: string
  created_at: string
  updated_at: string
  archived_at?: string | null
  charter_path?: string
}

export interface OwnerJournalNote {
  filename: string
  path: string
  mtime: string
  content: string
}

export interface OwnerTaskRow {
  slug: string
  name: string
  status: string
  priority: string
  auto_run_status?: string | null
  worktree_path?: string | null
  has_session: boolean
  is_question: boolean
}

export interface OwnerTickRecord {
  filename: string
  path: string
  started_at: string
  status: string
  content: string
}

export interface OwnerDetailView extends OwnerView {
  journal: OwnerJournalNote[]
  tasks: OwnerTaskRow[]
  ticks: OwnerTickRecord[]
  tick_log_tail?: string
}

export interface TaskView {
  slug: string
  name: string
  project_slug: string | null
  status: string
  kind: string
  playbook_slug: string | null
  parent_slug: string | null
  parent?: TaskSummary
  parents?: TaskSummary[]
  children?: TaskSummary[]
  forked_from_slug?: string | null
  forked_from?: TaskSummary
  fork_reason?: string | null
  forks?: TaskSummary[]
  priority: string
  work_dir: string
  worktree_path?: string
  workdir_known: WorkdirKnown | null
  waiting_on: string | null
  due_date: string | null
  due_info: string | null
  assignee: string | null
  permission_mode: string
  model: string
  tags: string[]
  session_id: string | null
  session_provider: string | null
  harness?: string | null
  session_started: string | null
  session_last_resumed: string | null
  live: boolean
  runtime_status?: string
  auto_run_status?: string
  auto_run_pid?: number
  auto_run_started?: string
  auto_run_finished?: string
  auto_run_log?: string
  days_in_status: number
  stale_days: number | null
  temporal_summary: string
  inbox_path?: string
  inbox_unread_count: number
  created_at: string
  updated_at: string
  archived_at: string | null
  deleted_at: string | null
  brief_path: string
  updates: FileRef[]
  aux_files: FileRef[]
  transcript_available: boolean
}

export interface BrainRunView {
  run_id: string
  family_slug: string
  task_slug: string
  task_name?: string
  task_status?: string
  plan_id?: string | null
  role: string
  provider: string
  requested_model?: string | null
  requested_tier?: string | null
  resolved_model?: string | null
  permission_mode: string
  status: string
  pid?: number | null
  session_id?: string | null
  log_path?: string | null
  input_summary?: string | null
  output_json?: unknown
  evidence_json?: unknown
  error_text?: string | null
  started_at?: string | null
  finished_at?: string | null
  created_at: string
  updated_at: string
  legacy?: boolean
}

export interface BrainRunsResponse {
  task_slug: string
  family_slug: string
  runs: BrainRunView[]
}

export type BrainGraphNodeType =
  | 'task'
  | 'worker_run'
  | 'validator_run'
  | 'steward_run'
  | 'approval'
  | 'transcript_ref'
  | 'github_ref'
  | 'event'
  | 'transcript'
  | 'log'
  | 'pr'
  | 'closeout'
  | 'owner'
  | (string & {})

export interface BrainGraphView {
  generated_at: string
  freshness: string
  controller: BrainGraphController
  owners: BrainGraphOwnerView[]
  nodes: BrainGraphNode[]
  edges: BrainGraphEdge[]
  counts: BrainGraphCounts
  selected_actions: BrainGraphActionSpec[]
  warnings: BrainGraphWarning[]
}

export interface BrainGraphController {
  mode: string
  display_name: string
  status: string
}

export interface BrainGraphOwnerView {
  id: string
  slug: string
  name: string
  status: string
  task_count: number
  running_count: number
}

export interface BrainGraphNode extends Record<string, unknown> {
  id: string
  type: BrainGraphNodeType
  owner_slug?: string
  task_slug?: string
  parent_task_slug?: string
  label: string
  status: string
  priority?: string
  provider?: string
  harness?: string
  permission_mode?: string
  model?: string
  summary?: string
  expanded: boolean
  ref?: BrainGraphRef
  badges?: string[]
  actions?: string[]
  metadata?: Record<string, string>
}

export interface BrainGraphRef {
  kind: string
  id: string
  url?: string
}

export interface BrainGraphEdge {
  id: string
  type: string
  source: string
  target: string
  label?: string
  status?: string
}

export interface BrainGraphCounts {
  total_tasks: number
  running: number
  done: number
  owners: number
  warnings: number
}

export interface BrainGraphActionSpec {
  key: string
  label: string
  risky: boolean
  enabled: boolean
  disabled_reason?: string
}

export interface BrainGraphWarning {
  code: string
  message: string
  node_id?: string
}

export interface BrainGraphNodeDetail {
  id: string
  type: BrainGraphNodeType
  task?: BrainGraphTaskDetail
  evidence?: BrainGraphEvidenceDetail
}

export interface BrainGraphTaskDetail {
  slug: string
  name: string
  status: string
  priority: string
  project_slug?: string | null
  parent_slug?: string | null
  work_dir: string
  worktree_path?: string | null
  session_provider: string
  harness: string
  permission_mode: string
  model?: string | null
  session_id?: string | null
  session_path?: string | null
  transcript?: BrainGraphEvidenceDetail
  brief_path: string
  updates: FileRef[]
}

export interface BrainGraphEvidenceDetail {
  kind: string
  task_slug?: string
  ref_id?: string
  path?: string | null
  url?: string | null
  available: boolean
  message?: string
}

export interface BrainGraphActionRequest {
  action: string
  node_id: string
  prompt?: string
  actor?: string
}

export interface BrainGraphActionResponse {
  ok: boolean
  message: string
  action?: string
  node_id?: string
  output?: string
  action_response?: ActionResponse
}

export interface TaskCounts {
  total: number
  in_progress: number
  backlog: number
  done: number
}

export interface ProjectView {
  slug: string
  name: string
  status: string
  priority: string
  work_dir: string
  workdir_known: WorkdirKnown | null
  created_at: string
  updated_at: string
  archived_at: string | null
  deleted_at: string | null
  task_counts: TaskCounts
  recent_tasks: TaskSummary[]
  brief_path: string
  updates: FileRef[]
  aux_files: FileRef[]
}

export interface RunSummary {
  slug: string
  name: string
  status: string
  priority: string
  provider: string
  created_at: string
  updated_at: string
  started_at: string | null
}

export interface PlaybookView {
  slug: string
  name: string
  project_slug: string | null
  work_dir: string
  created_at: string
  updated_at: string
  archived_at: string | null
  brief_path: string
  updates: FileRef[]
  aux_files: FileRef[]
  recent_runs: RunSummary[]
  run_count_7d: number
  run_days_30: number[]
  schedule: string | null
  schedule_spec: string | null
  schedule_paused: boolean
  next_fire_at: string | null
  last_fired_at: string | null
  last_fire_run_slug: string | null
}

export interface KBFileView {
  filename: string
  path: string
  mtime: string
  size: number
  entries: number
  preview: string
  content: string
}

export interface WorkdirView {
  path: string
  name: string | null
  description: string | null
  git_remote: string | null
  last_used_at: string | null
  created_at: string
  tasks_using_this: number
  untouched_30d: boolean
}

export interface FSBreadcrumb {
  name: string
  path: string
}
export interface FSEntryView {
  name: string
  path: string
  display_path: string
  is_dir: boolean
  is_git: boolean
  hidden: boolean
}
export interface FSEntriesView {
  path: string
  display_path: string
  parent: string | null
  is_git: boolean
  breadcrumbs: FSBreadcrumb[]
  entries: FSEntryView[]
}

export interface OverviewView {
  live_sessions: TaskView[]
  in_flight: TaskView[]
  high_priority_backlog: TaskView[]
  waiting: TaskView[]
  stale: TaskView[]
  active_playbooks: PlaybookView[]
  briefing: Briefing
}

export interface Briefing {
  generated_at: string
  window_start: string
  window_end: string
  // Three ranked tiers, scanned top-to-bottom: what needs you now, what changed
  // while you were away, and what to pick up next.
  needs_you: BriefingItem[]
  overnight: BriefingItem[]
  next_up: BriefingItem[]
}

export interface BriefingItem {
  kind: string
  ref: string
  source?: string
  project?: string
  urgency?: string
  title: string
  detail?: string
  action?: string
  links?: BriefingLink[]
}

export interface BriefingLink {
  kind: string
  label?: string
  target: string
  url?: string
}

// Live steering-cascade view: a CI-style pipeline of stage events per observed
// event. run_id equals the persisted trace id.
export interface SteeringStageEvent {
  run_id: string
  thread_key?: string
  source?: string
  stage: string // received | stage0 | stage1 | stage2 | stage3 | verdict
  status: string // running | passed | surfaced | dropped | error
  detail?: string
  stream?: string // accumulated model output for a live-streamed stage
  at: string
  elapsed_ms: number
}

export interface SteeringRun {
  run_id: string
  thread_key?: string
  source?: string
  stages: SteeringStageEvent[]
  status: string
  done: boolean
  started_at: string
  updated_at: string
  // Resolved origin (server-side, no raw IDs) — same enrichment the feed/trace
  // tabs use, so the live row reads "#general" / "DM · Alice" / "owner/repo".
  channel?: string
  channel_type?: string
  channel_name?: string
  author_name?: string
  permalink?: string
}

export interface SteeringRunsResponse {
  runs: SteeringRun[]
}

export type WorkEventBucket =
  | 'needs_action'
  | 'closeout'
  | 'waiting'
  | 'next_up'
  | 'fyi'
  | 'handled'
  | 'ignored'

export interface WorkEventLink {
  kind: string
  label?: string
  target: string
  url?: string
}

export interface WorkEvent {
  id: string
  source: string
  kind: string
  event_key?: string
  thread_key?: string
  url?: string
  title: string
  summary?: string
  actor?: string
  authored_by_self?: boolean
  occurred_at?: string
  observed_at?: string
  task_slug?: string
  project_slug?: string
  entity_kind?: string
  entity_ref?: string
  bucket: WorkEventBucket
  urgency?: string
  confidence?: number
  reason_code?: string
  reason_text?: string
  links?: WorkEventLink[]
}

export interface WorkEventCounts {
  needs_action: number
  closeout: number
  waiting: number
  next_up: number
  fyi: number
  handled: number
  ignored: number
}

export interface WorkEventResponse {
  items: WorkEvent[]
  counts: WorkEventCounts
}

export interface DiffCount {
  add: number
  rem: number
  files: number
}
export interface DiffLine {
  type: string
  n: string
  code: string
}
export interface DiffHunk {
  header: string
  lines: DiffLine[]
}
export interface DiffFile {
  name: string
  add: number
  rem: number
  hunks?: DiffHunk[]
}

export interface UiTranscript {
  type: string
  text?: string
  tool?: string
  input?: string
  summary?: string
  preview?: string
  lines?: number
  time?: string
}

export interface UiAgent {
  slug: string
  name: string
  project: string | null
  kind?: string
  playbook_slug?: string | null
  parent?: TaskSummary
  parents?: TaskSummary[]
  children?: TaskSummary[]
  forked_from_slug?: string | null
  forked_from?: TaskSummary
  fork_reason?: string | null
  forks?: TaskSummary[]
  branch: string
  branches?: string[]
  work_dir: string
  provider: string
  permission_mode: string
  priority: string
  status: string
  task_status: string
  runtime_status: string
  runtime_event?: string
  session_id: string
  started_min: number
  last_activity_sec: number
  last_action: string
  waiting_for?: { kind: string; cmd: string; why: string }
  diff: DiffCount
  tokens_used: number
  tokens_max: number
  tokens_session: number
  cost_session?: number
  activity: number[]
  tags: string[]
  summary: string
  next_step: string
  recent_tools?: { name: string; s: string }[]
  hook_health?: { status: string; message: string; action?: string }
  transcript?: UiTranscript[]
  brief?: string
  diff_files?: DiffFile[]
  brief_path?: string
  updates?: FileRef[]
  aux_files?: FileRef[]
  terminal: { mode?: string; message?: string }
  monitored: boolean
  auto_run_status?: string
  auto_run_started?: string
  auto_run_finished?: string
  auto_run_log?: string
}

export interface AutoRunFile {
  file: string
  size: number
  modified: string
}
export interface AutoRunLogResponse {
  content: string
  truncated?: boolean
}

export interface ToolCapability {
  id: string
  label: string
  available: boolean
  reason?: string
  path?: string
  status?: string
}
export interface Capabilities {
  providers: ToolCapability[]
  terminals: ToolCapability[]
  integrations: ToolCapability[]
}

export interface BacklogTask {
  slug: string
  name: string
  project: string
  parent?: TaskSummary
  children?: TaskSummary[]
  provider: string
  priority: string
  due?: string
  tags?: string[]
  waiting_on?: string | null
  started_min: number
}

export interface ProjectMC {
  slug: string
  name: string
  priority: string
  tasks: TaskCounts
  work_dir: string
}

export interface PlaybookRun {
  name: string
  status: string
  created_at: string
}

export interface PlaybookMC {
  slug: string
  name: string
  project: string | null
  runs_week: number
  last_min: number | null
  spark: number[]
  runs?: PlaybookRun[]
  work_dir: string
  schedule: string | null
  schedule_paused: boolean
  next_fire_at: string | null
}

export interface KBFile {
  name: string
  preview: string
  count: number
  entries: { d: string; t: string }[]
}

export interface MemorySource {
  id: string
  provider: string
  scope: string
  kind: string
  label: string
  path: string
  status: string
  available: boolean
  format?: string
  mtime?: string
  size?: number
  content?: string
  error?: string
}

export interface Workdir {
  path: string
  name: string
  remote: string | null
  used_min: number
  tasks: number
  untouched: boolean
}

export interface ActivityDay {
  date: string
  count: number
  tasks?: string[]
}

export interface TokenDay {
  date: string
  tokens: number
  cost_usd?: number
  task_count?: number
  tasks?: TokenTask[]
}

export interface TokenTask {
  name: string
  tokens: number
  cost_usd?: number
}

// One task's window-total token + estimated-cost usage, ranked for the
// "Top tasks by cost" leaderboard. Carries the slug for deep-linking.
export interface TopTask {
  slug: string
  name: string
  provider?: string
  tokens: number
  cost_usd?: number
}

// How many done tasks actually ran on a given model, read from the transcript
// (the real run-model, not the task's explicit pin). The client normalizes the
// raw model id to a tier label for the Composition bar.
export interface ModelCount {
  model: string
  count: number
}

export interface QuoteView {
  quote: string
  anime: string
  character: string
  author: string
}

export interface TrashItem {
  kind: string
  slug: string
  name: string
  status?: string
  priority?: string
  project?: string | null
  work_dir: string
  deleted_at: string
  archived: boolean
}

export interface UiStats {
  current_streak: number
  longest_streak: number
  active_days: number
  tokens_total: number
  tokens_claude: number
  tokens_codex: number
  cost_total?: number
  cost_claude?: number
  cost_codex?: number
  sessions_total: number
  sessions_claude: number
  sessions_codex: number
}

export interface FlowDBObject {
  name: string
  kind: string
  bytes: number
  human_size: string
  percent: number
}

export interface FlowDBDocStat {
  scope: string
  entity_type: string
  count: number
  content_bytes: number
  human_size: string
}

export interface FlowDBInfo {
  path: string
  display_path: string
  bytes: number
  human_size: string
  exists: boolean
  page_size: number
  page_count: number
  free_page_count: number
  used_bytes: number
  used_human_size: string
  reclaimable_bytes: number
  reclaimable_human_size: string
  quick_check: string
  quick_check_source: string
  quick_check_checked_at: string
  quick_check_note: string
  can_compact: boolean
  explanation: string
  objects: FlowDBObject[]
  documents: FlowDBDocStat[]
  error?: string
}

export interface UiData {
  AGENTS: UiAgent[]
  DEAD_AGENT: UiAgent | null
  DONE_AGENTS: UiAgent[]
  BACKLOG: BacklogTask[]
  DONE_TASKS: BacklogTask[]
  KB_FILES: KBFile[]
  AGENT_MEMORY_SOURCES: MemorySource[]
  WORKDIRS: Workdir[]
  PLAYBOOKS_MC: PlaybookMC[]
  PROJECTS_MC: ProjectMC[]
  ACTIVITY_HEATMAP: ActivityDay[]
  TOKEN_SERIES: TokenDay[]
  TOP_TASKS?: TopTask[]
  MODEL_MIX?: ModelCount[]
  STATS: UiStats
  CAPABILITIES: Capabilities
  TRASH: { tasks: TrashItem[]; projects: TrashItem[]; playbooks: TrashItem[]; total: number }
  FLOWDB: FlowDBInfo
  USER: { name: string; full_name: string; username: string }
  FLOATING_SESSIONS: FloatingSession[]
}

// FloatingSession is one adhoc Ask Flow terminal tracked server-side. The tray
// renders a chip per entry; `running` reflects whether its PTY is attached.
export interface FloatingSession {
  id: string
  provider: string
  title: string
  running: boolean
  waiting?: boolean
  waiting_why?: string
  created_at: string
}

export interface HealthView {
  ok: boolean
  version: string
  flow_root: string
}

// Chat mirrors the server's chatView (internal/server/chats.go) — one adhoc
// Ask Flow / Slack chat session in the Chats list. `origin` is "ui" | "slack";
// `live` is true while the chat's session still has a PTY attached.
export interface Chat {
  slug: string
  title: string
  provider: string
  origin: string
  created_at: string
  last_activity_at: string
  archived: boolean
  live: boolean
  /** One-line preview of the agent's most recent response in this chat. */
  last_reply?: string
}

export interface TranscriptEntry {
  type: string
  text?: string
  tool_name?: string
  tool_input_summary?: string
  tool_result_text?: string
  is_error?: boolean
  byte_offset: number
  timestamp?: string
}
export interface TranscriptResponse {
  available: boolean
  message?: string
  entries: TranscriptEntry[]
}

export interface SearchResult {
  type: string
  scope?: string
  slug: string
  name: string
  url: string
  snippet: string
  source_path?: string
}
export interface SearchResponse {
  query: string
  tasks?: SearchResult[]
  projects?: SearchResult[]
  playbooks?: SearchResult[]
  updates?: SearchResult[]
  transcripts?: SearchResult[]
  memories?: SearchResult[]
}

export interface AskFlowCitation {
  type: string
  id?: string
  slug?: string
  title: string
  url?: string
  source_path?: string
  snippet?: string
}
export interface AskFlowResponse {
  query: string
  intent: string
  answer: string
  citations: AskFlowCitation[]
}

export interface InboxConversationMessage {
  source: string
  kind: string
  sender_name: string
  timestamp: string
  title: string
  body: string
  permalink?: string
  reaction?: string
}
export interface InboxConversation {
  slug: string
  name: string
  project_slug?: string
  status: string
  provider: string
  live: boolean
  monitored: boolean
  source: string
  channel_name?: string
  messages: InboxConversationMessage[]
}
export interface InboxFeedEntry {
  task_slug: string
  task_name: string
  project_slug?: string
  status: string
  timestamp: string
  sender: string
  body: string
  body_snippet: string
  unread: boolean
  source?: string
  live: boolean
  monitored: boolean
}
export interface InboxFeed {
  entries: InboxFeedEntry[]
  unread_count: number
  task_count: number
  generated_at: string
}

export interface ActionResponse {
  ok: boolean
  message: string
  output?: string
  agent?: UiAgent
  floating_terminal?: {
    id: string
    provider: string
    title: string
  }
  bridge?: boolean
  already_live?: boolean
}

// One configurable setting surfaced in the Settings page. `value` is the
// current value (always "" for secrets); `set` reports whether an explicit
// (non-default) value is present; `source` is config | env | default.
export interface SettingField {
  key: string
  label: string
  group: string
  // Connector taxonomy — present only for connector-owned settings (Slack,
  // GitHub, ingress). Generic settings omit both and stay on the Settings page.
  category?: string
  connector?: string
  type: 'string' | 'secret' | 'bool' | 'int' | 'enum'
  default?: string
  options?: string[]
  help?: string
  value: string
  set: boolean
  source: 'config' | 'env' | 'default'
}
export interface SettingsResponse {
  fields: SettingField[]
}

/** GET /api/slack/setup/status — drives the Connect Slack wizard. */
export interface SlackSetupStatus {
  app_created: boolean
  app_id?: string
  manage_url?: string
  app_token_url?: string
  app_token_set: boolean
  bot_token_set: boolean
  user_token_set: boolean
  self_user_ids?: string
  redirect_url: string
  callback_mode: 'localhost'
  oauth_active: boolean
  oauth_status?: string
  oauth_error?: string
  oauth_authorize_url?: string
  oauth_team?: string
  listener_running: boolean
  listener_connected: boolean
  listener_suppressed: boolean
}

// GitHubSetupStatus drives the resumable Connect-GitHub (App-manifest) wizard,
// mirroring the GitHubSetupStatus struct served at GET /api/github/setup/status.
export interface GitHubSetupStatus {
  ingress_ready: boolean
  webhook_url?: string
  redirect_url?: string
  app_created: boolean
  app_id?: string
  app_slug?: string
  html_url?: string
  pem_set: boolean
  webhook_secret_set: boolean
  install_url?: string
  installation_ids?: string
  installed: boolean
  self_logins?: string
  self_logins_set: boolean
  transport: string
  summary: string
}

/** One account the connected GitHub App is installed on. */
export interface GitHubInstallation {
  id: number
  account: string
  type: string // "User" | "Organization"
}

/** Accounts the connected App is installed on (personal + orgs). */
export interface GitHubInstallations {
  installations: GitHubInstallation[]
  /** Present when the App-JWT call to GitHub failed. */
  error?: string
}

/** Orgs the active `gh` identity can target when creating a GitHub App. */
export interface GitHubOrgs {
  orgs: string[]
  active_login?: string
  /** Present when gh is missing / unauthenticated / the API call failed; the wizard falls back to manual org entry. */
  error?: string
}

/** One identity `gh` is logged in as. */
export interface GitHubAccount {
  login: string
  active: boolean
  source?: string // "keyring" | "GH_TOKEN" | "GITHUB_TOKEN" | …
}

/** GET /api/github/auth/status — who flow polls GitHub as, and switch targets. */
export interface GitHubAuthStatus {
  installed: boolean
  authenticated: boolean
  path?: string
  host?: string
  active_login?: string
  active_source?: string
  // env_pinned: the active identity comes from a GH_TOKEN/GITHUB_TOKEN env var,
  // which overrides keyring accounts — switching is a no-op until it's unset.
  env_pinned: boolean
  accounts: GitHubAccount[]
  error?: string
}

// GitHubWebhookStatus mirrors GET /api/github/webhook/status — the live
// webhook transport state for the Connectors Git card.
export interface GitHubWebhookStatus {
  transport: string
  secret_configured: boolean
  webhook_url?: string
  deliveries_total: number
  last_received_at?: string
  last_status?: string
  last_error?: string
  receiving: boolean
  summary: string
}

export interface IngressStatus {
  provider: string
  // Public base URL, discovered from zrok at runtime (or operator-supplied for
  // manual). Empty until the share is established.
  base_url?: string
  running: boolean
  env_enabled?: boolean
  // Reserved zrok share unique-name (FLOW_ZROK_SHARE_NAME), if configured.
  share_name?: string
  share_running?: boolean
  last_error?: string
  // Whether a GitHub webhook signing secret is configured. The value itself is
  // never sent here; use the reveal-webhook-secret action to copy it.
  webhook_secret_set?: boolean
  github_webhook_url?: string
}

export interface ActionRequest {
  kind: string
  target?: string
  slug?: string
  settings?: Record<string, string>
  name?: string
  path?: string
  description?: string
  project?: string
  work_dir?: string
  priority?: string
  prompt?: string
  session_id?: string
  branch?: string
  entity_kind?: string
  provider?: string
  permission_mode?: string
  mkdir?: boolean
  no_open?: boolean
  attention_action?: string
  reply_text?: string
  reply_instructions?: string
  correction_text?: string
  remember?: boolean
  schedule?: string
  schedule_op?: string
}

export interface AttentionItem {
  id: string
  source: string
  thread_key: string
  summary: string
  suggested_action: string
  matched_task?: string
  suggested_project?: string
  suggested_priority?: string
  urgency?: string
  is_vip: boolean
  confidence: number
  draft?: string
  reason?: string
  status: string
  retriaging?: boolean
  created_at: string
  acted_at?: string
  linked_task?: string
  // Resolved origin fields (no raw IDs) — where the message came from + a link.
  channel?: string
  channel_type?: string
  channel_name?: string // "#general" (slack), "owner/repo" (github), or "DM · Name" / "Direct message"
  author?: string
  author_name?: string // resolved display name / GitHub login
  permalink?: string // slack:// deep link OR https GitHub URL
  why: AttentionWhy
  action_previews?: AttentionActionPreview[]
  handoff?: AttentionHandoff
}

export interface AttentionTaskMatch {
  slug: string
  name?: string
  status?: string
  priority?: string
  project_slug?: string
  session_provider?: string
}

export interface AttentionWhy {
  source: string
  context_summary?: string
  fetch_status?: string
  fetch_error?: string
  evidence_count?: number
  participants?: string[]
  parent_preview?: string
  latest_preview?: string
  reason?: string
  confidence: number
  stage_reached?: string
  stage_action?: string
  stage_confidence?: number
  stage1_relevant?: boolean
  suggested_project?: string
  suggested_priority?: string
  matched_task?: AttentionTaskMatch
}

export interface AttentionActionPreview {
  action: string
  label: string
  description: string
  target?: string
  primary?: boolean
  destructive?: boolean
}

export interface AttentionHandoff {
  id: string
  feed_item_id: string
  sender: string
  receiver: string
  requested_verdict: string
  status: string
  reason?: string
  requested_at: string
  expires_at: string
  responded_at?: string
}

export interface SteeringFunnel {
  observed: number
  dropped_stage0: number
  dropped_cache: number
  dropped_stage1: number
  dropped_stage2: number
  surfaced: number
  errors: number
}
export interface SteeringTrace {
  id: string
  created_at: string
  origin: string
  source: string
  channel?: string
  channel_type?: string
  author?: string
  thread_key?: string
  text_preview?: string
  // Resolved, human-readable fields from the server (no raw IDs):
  channel_name?: string // "#general" (slack) or "owner/repo" (github)
  author_name?: string // display name (slack) or login (github)
  text?: string // full message text, @mentions resolved to names
  permalink?: string // slack:// deep link, or the GitHub URL
  ts?: string
  team_id?: string
  url?: string
  disposition: string
  stage_reached: string
  drop_reason?: string
  stage1_relevant?: boolean
  stage1_reason?: string
  stage2_action?: string
  stage2_confidence?: number
  stage3_action?: string
  stage3_confidence?: number
  final_action?: string
  final_confidence?: number
  feed_item_id?: string
  linked_task?: string
  matched_task?: AttentionTaskMatch
  error?: string
  autonomy_action?: string
  autonomy_decision?: string
  autonomy_reason?: string
  latency_ms: number
  model?: string
}
export interface AttentionTraceResponse {
  funnel: SteeringFunnel
  items: SteeringTrace[]
}

export interface SlackChannel {
  id: string
  name: string
  is_private: boolean
  is_member: boolean
}
