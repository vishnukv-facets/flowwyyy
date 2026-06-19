import { QueryClient, keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, apiAction, apiGet, apiGetText, apiPost } from './api'
import { rpc } from './rpc'
import { events } from './events'
import { focusedLiveInvalidationKeys } from './liveInvalidation'
import { pushToast } from './toast'
import type {
  ActionRequest,
  AskFlowResponse,
  AttentionItem,
  Chat,
  BrainGraphActionRequest,
  BrainGraphActionResponse,
  AttentionTraceResponse,
  BrainGraphNodeDetail,
  BrainGraphView,
  BrainRunView,
  BrainRunsResponse,
  GitHubAuthStatus,
  GitHubInstallations,
  GitHubOrgs,
  GitHubWebhookStatus,
  HealthView,
  IngressStatus,
  InboxConversation,
  InboxFeed,
  KBFileView,
  KBDreamStatus,
  MemorySource,
  OwnerView,
  OwnerDetailView,
  OverviewView,
  PlaybookView,
  ProjectView,
  QuoteView,
  SearchResponse,
  GitHubSetupStatus,
  SettingsResponse,
  SlackChannel,
  SlackSetupStatus,
  SteeringRunsResponse,
  SteeringTrace,
  TaskView,
  TranscriptResponse,
  UiAgent,
  UiData,
  WorkEventResponse,
  WorkdirView,
} from './types'

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 4000,
      gcTime: 5 * 60 * 1000,
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
})

// ----- live invalidation: events + reconnect drive refetches -------------
// Live events should refresh live work data, not slow/static metadata. Quote,
// settings, and Slack channel listing have their own refresh cadence; invalidating
// Slack channels on every message can hammer conversations.list into rate limits.
const liveInvalidationExclusions = new Set(['quote', 'settings', 'slack-channels', 'slack-setup', 'ingress-status', 'github-auth', 'memory-sources'])
const liveData = { predicate: (q: { queryKey: readonly unknown[] }) => !liveInvalidationExclusions.has(String(q.queryKey[0])) }
let invalidateTimer: ReturnType<typeof setTimeout> | null = null
let pendingBroadInvalidate = false
const pendingFocusedInvalidations = new Set<string>()
function scheduleInvalidate(env?: { type?: string }) {
  const focused = focusedLiveInvalidationKeys(env)
  if (focused === null) {
    pendingBroadInvalidate = true
  } else {
    focused.forEach((key) => pendingFocusedInvalidations.add(key))
  }
  if (invalidateTimer) return
  invalidateTimer = setTimeout(() => {
    invalidateTimer = null
    if (pendingBroadInvalidate) {
      queryClient.invalidateQueries(liveData)
    } else {
      for (const key of pendingFocusedInvalidations) {
        queryClient.invalidateQueries({ queryKey: [key] })
      }
    }
    pendingBroadInvalidate = false
    pendingFocusedInvalidations.clear()
  }, 500)
}
events.on((env) => scheduleInvalidate(env))
rpc.onReady(() => queryClient.invalidateQueries(liveData))

// ----- query string helper ----------------------------------------------
function qs(params: Record<string, string | boolean | number | undefined>): string {
  const parts: string[] = []
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === '' || v === false) continue
    parts.push(`${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`)
  }
  return parts.length ? `?${parts.join('&')}` : ''
}

// ----- queries ------------------------------------------------------------
export function useUiData() {
  // Runtime events refresh this during active work. Keep this event-driven so
  // large dashboards do not rebuild on a timer while idle.
  return useQuery({
    queryKey: ['ui-data'],
    queryFn: () => apiGet<UiData>('/api/ui-data'),
  })
}
export function useSettings() {
  return useQuery({
    queryKey: ['settings'],
    queryFn: () => apiGet<SettingsResponse>('/api/settings'),
  })
}
// Polls faster while an OAuth install is mid-flight (the callback lands on a
// separate ephemeral listener, so the wizard learns of completion by poll +
// the slack-setup WS event, whichever first).
export function useSlackSetupStatus() {
  return useQuery({
    queryKey: ['slack-setup'],
    queryFn: () => apiGet<SlackSetupStatus>('/api/slack/setup/status'),
    refetchInterval: (q) => (q.state.data?.oauth_active ? 1500 : 8000),
  })
}
export function useHealth() {
  return useQuery({ queryKey: ['health'], queryFn: () => apiGet<HealthView>('/api/health') })
}
// GitHub `gh` CLI identity: active login + all switchable accounts. Changes
// only on an explicit account switch (which invalidates this key), so it isn't
// part of the live-event refresh set.
export function useGitHubAuth() {
  return useQuery({
    queryKey: ['github-auth'],
    queryFn: () => apiGet<GitHubAuthStatus>('/api/github/auth/status'),
    staleTime: 10_000,
  })
}
// GitHub webhook transport status (mode, secret configured, deliveries). Polls
// while in webhook mode so "awaiting first delivery" flips to "receiving" without
// a manual reload.
export function useGitHubWebhookStatus() {
  return useQuery({
    queryKey: ['github-webhook-status'],
    queryFn: () => apiGet<GitHubWebhookStatus>('/api/github/webhook/status'),
    refetchInterval: (q) => {
      const st = q.state.data
      if (st && (st.transport === 'webhook' || st.transport === 'hybrid') && !st.receiving) return 5000
      return 20000
    },
  })
}
// Orgs the active gh identity can create an App in — feeds the wizard's org
// dropdown. Disabled until the operator picks the "Organization" target, so the
// `gh api user/orgs` shell-out doesn't run on every modal open.
export function useGitHubOrgs(enabled: boolean) {
  return useQuery({
    queryKey: ['github-orgs'],
    queryFn: () => apiGet<GitHubOrgs>('/api/github/setup/orgs'),
    enabled,
    staleTime: 30_000,
  })
}
// Accounts the connected App is installed on (personal + orgs). Enabled only
// once an App exists (install step / done), since it makes an App-JWT call to
// GitHub. Polls while enabled so a freshly-added install (done in a github.com
// tab) appears without a manual reload.
export function useGitHubInstallations(enabled: boolean) {
  return useQuery({
    queryKey: ['github-installations'],
    queryFn: () => apiGet<GitHubInstallations>('/api/github/setup/installations'),
    enabled,
    refetchInterval: enabled ? 5000 : false,
  })
}
// Connect-GitHub App-manifest wizard status. Polls quickly while the App isn't
// fully connected+installed yet, so the create→install transitions (which
// complete in a separate browser tab on github.com) surface without a reload;
// settles to a slow poll once done.
export function useGitHubSetupStatus() {
  return useQuery({
    queryKey: ['github-setup'],
    queryFn: () => apiGet<GitHubSetupStatus>('/api/github/setup/status'),
    refetchInterval: (q) => {
      const st = q.state.data
      if (st && (!st.app_created || !st.installed)) return 2500
      return 15000
    },
  })
}
// Public ingress status (zrok/manual/none) + derived connector callback URLs.
// Refetches while a zrok share is coming up so the discovered URL appears
// without a manual reload; settles to a slow poll once running/idle.
export function useIngressStatus() {
  return useQuery({
    queryKey: ['ingress-status'],
    queryFn: () => apiGet<IngressStatus>('/api/ingress/status'),
    refetchInterval: (q) => {
      const st = q.state.data
      if (st && st.provider === 'zrok' && st.env_enabled && !st.running) return 3000
      return 20000
    },
  })
}
export function useOverview() {
  return useQuery({ queryKey: ['overview'], queryFn: () => apiGet<OverviewView>('/api/overview') })
}

// Recent + in-flight steering cascade runs (the live CI-style stage view).
// Refetched on each steering_stage WS delta via focusedLiveInvalidationKeys.
export function useSteeringRuns() {
  return useQuery({ queryKey: ['steering-runs'], queryFn: () => apiGet<SteeringRunsResponse>('/api/steering/runs') })
}

export interface BrainGraphFilters {
  project?: string
  owner?: string
  status?: string
  includeDone?: boolean
  expand?: string[]
  q?: string
}
export function useBrainGraph(filters: BrainGraphFilters = {}) {
  const stableFilters = {
    project: filters.project || '',
    owner: filters.owner || '',
    status: filters.status || '',
    include_done: !!filters.includeDone,
    expand: [...(filters.expand ?? [])].filter(Boolean).sort(),
    q: filters.q || '',
  }
  return useQuery({
    queryKey: ['brain-graph', stableFilters],
    queryFn: () =>
      apiGet<BrainGraphView>(
        `/api/brain/graph${qs({
          project: stableFilters.project,
          owner: stableFilters.owner,
          status: stableFilters.status,
          include_done: stableFilters.include_done,
          expand: stableFilters.expand.join(','),
          q: stableFilters.q,
        })}`,
      ),
    placeholderData: keepPreviousData,
  })
}

export function useBrainGraphNodeDetail(nodeId?: string | null) {
  const stableNodeId = nodeId || ''
  return useQuery({
    queryKey: ['brain-graph-node', stableNodeId],
    queryFn: () => apiGet<BrainGraphNodeDetail>(`/api/brain/graph/node/${encodeURIComponent(stableNodeId)}`),
    enabled: Boolean(stableNodeId),
  })
}

async function postBrainGraphAction(req: BrainGraphActionRequest): Promise<BrainGraphActionResponse> {
  const r = await rpc.request({
    method: 'POST',
    path: '/api/brain/graph/actions',
    body: req,
    timeoutMs: 180000,
  })
  const data = (r.json ?? {}) as BrainGraphActionResponse
  if (r.status >= 400 || data.ok === false) {
    throw new ApiError(r.status, data.message || `graph action failed (${r.status})`)
  }
  return data
}

export function useBrainGraphAction() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: BrainGraphActionRequest) => postBrainGraphAction(req),
    onSuccess: (data) => {
      if (data.message) pushToast('ok', data.message)
      qc.invalidateQueries({ queryKey: ['brain-graph'] })
      qc.invalidateQueries({ queryKey: ['brain-graph-node'] })
    },
    onError: (err: Error) => {
      pushToast('error', err.message || 'graph action failed')
    },
  })
}

export interface TaskFilters {
  status?: string
  project?: string
  priority?: string
  tag?: string
  kind?: string
  include_done?: boolean
  include_archived?: boolean
  since?: string
}
export function useTasks(filters: TaskFilters = {}) {
  return useQuery({
    queryKey: ['tasks', filters],
    queryFn: () =>
      apiGet<TaskView[]>(
        `/api/tasks${qs(filters as Record<string, string | boolean | number | undefined>)}`,
      ),
  })
}

export function useTask(slug: string | undefined) {
  return useQuery({
    queryKey: ['task', slug],
    enabled: !!slug,
    queryFn: () => apiGet<TaskView>(`/api/tasks/${encodeURIComponent(slug!)}`),
  })
}
export function useTaskBridge(slug: string | undefined, enabled = true) {
  return useQuery({
    queryKey: ['task-bridge', slug],
    enabled: !!slug && enabled,
    queryFn: () => apiGet<UiAgent>(`/api/tasks/${encodeURIComponent(slug!)}/bridge`),
  })
}
export function useTaskTranscript(slug: string | undefined, enabled = true, pollMs?: number) {
  return useQuery({
    queryKey: ['task-transcript', slug],
    enabled: !!slug && enabled,
    queryFn: () => apiGet<TranscriptResponse>(`/api/tasks/${encodeURIComponent(slug!)}/transcript`),
    // While a session is live the JSONL grows continuously between push
    // events — a light poll keeps the chat view tailing smoothly. Idle
    // transcripts rely on the broad live-invalidation only (pollMs unset).
    refetchInterval: pollMs && enabled ? pollMs : false,
  })
}

export function useAutoRuns(slug: string | undefined, enabled = true) {
  return useQuery({
    queryKey: ['auto-runs', slug],
    enabled: !!slug && enabled,
    queryFn: () => apiGet<import('./types').AutoRunFile[]>(`/api/tasks/${encodeURIComponent(slug!)}/auto-runs`),
  })
}

export function useAutoRunLog(slug: string | undefined, file: string | undefined) {
  return useQuery({
    queryKey: ['auto-run-log', slug, file],
    enabled: !!slug && !!file,
    queryFn: () => apiGet<import('./types').AutoRunLogResponse>(`/api/tasks/${encodeURIComponent(slug!)}/auto-runs/log?file=${encodeURIComponent(file!)}`),
  })
}

export function useTaskRuns(slug: string | undefined, enabled = true, limit = 20) {
  return useQuery({
    queryKey: ['task-runs', slug, limit],
    enabled: !!slug && enabled,
    queryFn: () =>
      apiGet<BrainRunsResponse>(
        `/api/tasks/${encodeURIComponent(slug!)}/runs${limit > 0 ? `?limit=${encodeURIComponent(String(limit))}` : ''}`,
      ),
  })
}

export function useTaskRun(slug: string | undefined, runId: string | undefined, enabled = true) {
  return useQuery({
    queryKey: ['task-run', slug, runId],
    enabled: !!slug && !!runId && enabled,
    queryFn: () =>
      apiGet<BrainRunView>(
        `/api/tasks/${encodeURIComponent(slug!)}/runs/${encodeURIComponent(runId!)}`,
      ),
  })
}

export interface OwnerFilters {
  status?: string
  include_archived?: boolean
}
export function useOwners(filters: OwnerFilters = {}) {
  return useQuery({
    queryKey: ['owners', filters],
    queryFn: () =>
      apiGet<OwnerView[]>(
        `/api/owners${qs(filters as Record<string, string | boolean | number | undefined>)}`,
      ),
  })
}
export function useOwner(slug: string | undefined, opts: { enabled?: boolean; poll?: boolean } = {}) {
  const enabled = (opts.enabled ?? true) && !!slug
  return useQuery({
    queryKey: ['owner', slug],
    enabled,
    queryFn: () => apiGet<OwnerDetailView>(`/api/owners/${encodeURIComponent(slug!)}`),
    // While a tick is running, poll so the journal + owned-task status refresh.
    refetchInterval: opts.poll ? 4000 : false,
  })
}

export interface ProjectListOpts {
  include_archived?: boolean
  include_deleted?: boolean
}

export function useProjects(opts: ProjectListOpts = {}) {
  return useQuery({
    queryKey: ['projects', opts],
    queryFn: () =>
      apiGet<ProjectView[]>(
        `/api/projects${qs(opts as Record<string, string | boolean | number | undefined>)}`,
      ),
  })
}
export function useProject(slug: string | undefined) {
  return useQuery({
    queryKey: ['project', slug],
    enabled: !!slug,
    queryFn: () => apiGet<ProjectView>(`/api/projects/${encodeURIComponent(slug!)}`),
  })
}
export function useProjectTasks(slug: string | undefined) {
  return useQuery({
    queryKey: ['project-tasks', slug],
    enabled: !!slug,
    queryFn: () =>
      apiGet<TaskView[]>(`/api/projects/${encodeURIComponent(slug!)}/tasks?include_done=1`),
  })
}

export interface PlaybookListOpts {
  include_archived?: boolean
  project?: string
}
export function usePlaybooks(opts: PlaybookListOpts = {}) {
  return useQuery({
    queryKey: ['playbooks', opts],
    queryFn: () =>
      apiGet<PlaybookView[]>(
        `/api/playbooks${qs(opts as Record<string, string | boolean | number | undefined>)}`,
      ),
  })
}
export function usePlaybook(slug: string | undefined) {
  return useQuery({
    queryKey: ['playbook', slug],
    enabled: !!slug,
    queryFn: () => apiGet<PlaybookView>(`/api/playbooks/${encodeURIComponent(slug!)}`),
  })
}

export function useKB() {
  // No polling: the server's kb file watcher pushes a "kb" ui_change over SSE on
  // any kb/*.md write (agent capture, dreamer prune, UI edit), which
  // focus-invalidates this ['kb'] query (see liveInvalidation.ts). So the
  // Knowledge screen updates live, consistent with the rest of the app.
  return useQuery({ queryKey: ['kb'], queryFn: () => apiGet<KBFileView[]>('/api/kb') })
}
export function useKBDream() {
  // The dreamer's next-run is 24h out, so a slow poll suffices; the client-side
  // countdown ticks every second locally (useNow). A manual "dream now" trigger
  // invalidates ['kb-dream'] directly so the running state shows immediately.
  return useQuery({ queryKey: ['kb-dream'], queryFn: () => apiGet<KBDreamStatus>('/api/kb/dream'), refetchInterval: 30_000 })
}
export function useBackupStatus() {
  return useQuery({ queryKey: ['backup-status'], queryFn: () => apiGet<import('./types').BackupStatus>('/api/backup/status'), refetchInterval: 30_000 })
}
export function useBackupLog(file: string | null, enabled: boolean) {
  return useQuery({
    queryKey: ['backup-log', file ?? ''],
    queryFn: () => apiGet<import('./types').BackupCommit[]>(`/api/backup/log?limit=50${file ? `&file=${encodeURIComponent(file)}` : ''}`),
    enabled,
  })
}
export function useMemorySources() {
  return useQuery({ queryKey: ['memory-sources'], queryFn: () => apiGet<MemorySource[]>('/api/memory/sources') })
}
export function useWorkdirs() {
  return useQuery({ queryKey: ['workdirs'], queryFn: () => apiGet<WorkdirView[]>('/api/workdirs') })
}
export function useInbox() {
  return useQuery({ queryKey: ['inbox'], queryFn: () => apiGet<InboxFeed>('/api/inbox') })
}
// Adhoc Ask Flow / Slack chat sessions for the Chats screen. include_archived
// surfaces archived chats too; the server always returns a JSON array (never
// null). Live `chats` ui_change events refetch just this key (see
// focusedLiveInvalidationKeys), so the list reflects reopen/archive/delete
// without a broad UI invalidation.
export function useChats(includeArchived = false) {
  return useQuery({
    queryKey: ['chats', includeArchived],
    queryFn: () => apiGet<Chat[]>(`/api/chats${qs({ include_archived: includeArchived })}`),
    // While any chat is live, poll so the agent's latest-response preview and
    // working state stay fresh; stop polling once everything is idle.
    refetchInterval: (q) => (q.state.data?.some((c) => c.live) ? 3000 : false),
  })
}
export function useAttention(status: string = 'new') {
  const q = status ? `?status=${encodeURIComponent(status)}` : ''
  return useQuery({
    queryKey: ['attention', status],
    queryFn: () => apiGet<AttentionItem[]>(`/api/attention${q}`),
    // Keep the prior status tab's results visible while the new one loads, so
    // switching new/acted/dismissed/all feels instant instead of blanking.
    placeholderData: keepPreviousData,
  })
}
export function useAttentionTrace(since: string, disposition: string = 'all', source: string = 'all') {
  const params = new URLSearchParams({ since })
  if (disposition && disposition !== 'all') params.set('disposition', disposition)
  if (source && source !== 'all') params.set('source', source)
  return useQuery({
    queryKey: ['attention-trace', since, disposition, source],
    queryFn: () => apiGet<AttentionTraceResponse>(`/api/attention/trace?${params.toString()}`),
    refetchInterval: 15000, // keep the live window fresh while watching
    // Each 1h/24h/7d (or disposition/source) switch is a new query key; show the
    // prior window's rows immediately rather than dropping to a spinner.
    placeholderData: keepPreviousData,
  })
}
// Fetches the cascade-decision trace behind a surfaced feed item, so the Feed
// detail modal can show the same "why was this chosen" reasoning the Trace view
// does. 404 = an older item logged before decision tracing; don't retry.
export function useAttentionDecision(feedId: string | null) {
  return useQuery({
    queryKey: ['attention-decision', feedId],
    enabled: !!feedId,
    retry: false, // 404 = older item with no trace; don't hammer
    queryFn: () => apiGet<SteeringTrace>(`/api/attention/decision?feed_id=${encodeURIComponent(feedId!)}`),
  })
}
export interface WorkEventFilters {
  source?: string
  bucket?: string
  task?: string
  limit?: number
}
export function useWorkEvents(filters: WorkEventFilters = {}) {
  return useQuery({
    queryKey: ['work-events', filters],
    queryFn: () =>
      apiGet<WorkEventResponse>(
        `/api/work-events${qs(filters as Record<string, string | boolean | number | undefined>)}`,
      ),
    placeholderData: keepPreviousData,
  })
}
export function useSlackChannels() {
  return useQuery({
    queryKey: ['slack-channels'],
    queryFn: () => apiGet<SlackChannel[]>('/api/slack/channels'),
    staleTime: 10 * 60 * 1000,
    gcTime: 30 * 60 * 1000,
    retry: 0,
  })
}
// Keyed by hour bucket ("YYYY-MM-DD-HH"): a new quote is fetched only when the
// hour flips. staleTime Infinity means it's never refetched within the hour no
// matter how many times the page reloads. The server caches per bucket too, so
// animechan is hit at most once per hour across all clients.
export function useQuote(bucket: string) {
  return useQuery({
    queryKey: ['quote', bucket],
    staleTime: Infinity,
    gcTime: Infinity,
    retry: 0,
    queryFn: () => apiGet<QuoteView>(`/api/quote?bucket=${encodeURIComponent(bucket)}`),
  })
}
export function useInboxConversation(slug: string | undefined) {
  return useQuery({
    queryKey: ['inbox-convo', slug],
    enabled: !!slug,
    queryFn: () =>
      apiGet<InboxConversation>(`/api/inbox/conversation?slug=${encodeURIComponent(slug!)}`),
  })
}

/** Generic markdown fetch (briefs, updates, kb files). */
export function useMarkdown(path: string | undefined, staleMs = 15000) {
  return useQuery({
    queryKey: ['md', path],
    enabled: !!path,
    staleTime: staleMs,
    queryFn: () => apiGetText(path!),
  })
}

export function useSearch(query: string, scope = 'all') {
  const q = query.trim()
  // Transcripts are huge (whole session JSONL); FTS over them costs seconds for
  // common terms (e.g. "facets" 6.7s vs 0.3s without). They're searched ONLY
  // when the Transcripts chip is active — matching the backend's opt-in design —
  // so the default ⌘K search stays instant. Other chips (tasks/projects/etc.)
  // are filtered client-side from the briefs+updates+memories result.
  const inScopes = scope === 'transcripts' ? 'transcripts' : 'briefs,updates,memories'
  return useQuery({
    queryKey: ['search', q, inScopes],
    enabled: q.length >= 2,
    staleTime: 2000,
    // A cold-scope build can transiently 500 under rapid typing (DB write
    // contention). Retry quickly so a blip recovers at fetch time instead of
    // caching an error that sticks as a permanent "No matches" for that term.
    retry: 3,
    retryDelay: 250,
    queryFn: () =>
      apiGet<SearchResponse>(
        // `in` takes document scopes (briefs cover task/project/playbook briefs);
        // entity-type names like "tasks" are invalid and 400.
        `/api/search?q=${encodeURIComponent(q)}&in=${inScopes}&limit=8`,
      ),
  })
}

export function useAskFlow() {
  return useMutation({
    mutationFn: (query: string) => apiPost<AskFlowResponse>('/api/ask-flow', { query }),
  })
}

// ----- action mutation ----------------------------------------------------
export function useAction() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: ActionRequest) => apiAction(req),
    onSuccess: (data) => {
      if (data.message) pushToast('ok', data.message)
      qc.invalidateQueries()
    },
    onError: (err: Error) => {
      pushToast('error', err.message || 'action failed')
    },
  })
}
