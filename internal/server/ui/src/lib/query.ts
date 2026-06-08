import { QueryClient, keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { apiAction, apiGet, apiGetText, apiPost } from './api'
import { rpc } from './rpc'
import { events } from './events'
import { UI_DATA_IDLE_REFETCH_MS, focusedLiveInvalidationKeys } from './liveInvalidation'
import { pushToast } from './toast'
import type {
  ActionRequest,
  AskFlowResponse,
  AttentionItem,
  AttentionTraceResponse,
  HealthView,
  InboxConversation,
  InboxFeed,
  KBFileView,
  MemorySource,
  OverviewView,
  PlaybookView,
  ProjectView,
  QuoteView,
  SearchResponse,
  SettingsResponse,
  SlackChannel,
  SlackSetupStatus,
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
const liveInvalidationExclusions = new Set(['quote', 'settings', 'slack-channels', 'slack-setup', 'memory-sources'])
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
  // Runtime events refresh this immediately during active work. The idle poll is
  // a slow backstop for mid-turn token growth and missed filesystem-side changes.
  return useQuery({
    queryKey: ['ui-data'],
    queryFn: () => apiGet<UiData>('/api/ui-data'),
    refetchInterval: UI_DATA_IDLE_REFETCH_MS,
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
export function useOverview() {
  return useQuery({ queryKey: ['overview'], queryFn: () => apiGet<OverviewView>('/api/overview') })
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
export function useTaskTranscript(slug: string | undefined, enabled = true) {
  return useQuery({
    queryKey: ['task-transcript', slug],
    enabled: !!slug && enabled,
    queryFn: () => apiGet<TranscriptResponse>(`/api/tasks/${encodeURIComponent(slug!)}/transcript`),
  })
}

export interface ProjectListOpts {
  include_archived?: boolean
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
  return useQuery({ queryKey: ['kb'], queryFn: () => apiGet<KBFileView[]>('/api/kb') })
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
