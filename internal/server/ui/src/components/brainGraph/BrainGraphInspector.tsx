import { Link, useLocation } from 'wouter'
import { useState, type ReactNode } from 'react'
import {
  AlertTriangle,
  ArrowUpRight,
  FileText,
  GitBranch,
  Info,
  Loader2,
  ScrollText,
  SendHorizontal,
  TerminalSquare,
  X,
} from 'lucide-react'
import { StatusDot } from '../ui'
import { Modal } from '../Modal'
import { useBrainGraphAction, useBrainGraphNodeDetail, useTaskTranscript } from '../../lib/query'
import { dateTime } from '../../lib/format'
import type {
  BrainGraphActionSpec,
  BrainGraphEvidenceDetail,
  BrainGraphNode,
  BrainGraphTaskDetail,
  BrainGraphWarning,
  TranscriptEntry,
} from '../../lib/types'

function actionByKey(actions: BrainGraphActionSpec[], key: string) {
  return actions.find((action) => action.key === key)
}

function nodeTone(status: string) {
  switch (status) {
    case 'waiting':
      return 'warn'
    case 'dead':
    case 'error':
    case 'failed':
      return 'danger'
    case 'running':
    case 'in-progress':
      return 'ok'
    case 'done':
    case 'completed':
      return 'info'
    default:
      return ''
  }
}

function errorText(error: unknown) {
  return error instanceof Error ? error.message : 'detail unavailable'
}

export function BrainGraphInspector({
  open,
  selected,
  actions,
  warnings,
  onClose,
}: {
  open: boolean
  selected: BrainGraphNode | null
  actions: BrainGraphActionSpec[]
  warnings: BrainGraphWarning[]
  onClose: () => void
}) {
  const nodeWarnings = selected ? warnings.filter((warning) => warning.node_id === selected.id) : []
  const detailQuery = useBrainGraphNodeDetail(selected?.id ?? null)
  const detail = detailQuery.data?.id === selected?.id ? detailQuery.data : undefined
  const detailLoading = Boolean(selected) && (detailQuery.isLoading || (detailQuery.isFetching && !detail))

  return (
    <aside className={`brain-inspector ${open ? 'open' : ''}`} aria-hidden={!open}>
      <div className="brain-inspector-head">
        {selected ? <Info size={15} /> : <AlertTriangle size={15} />}
        <span>{selected ? 'Inspector' : 'Warnings'}</span>
        <button type="button" className="brain-inspector-close" onClick={onClose} aria-label="Close inspector">
          <X size={16} />
        </button>
      </div>
      <div className="brain-inspector-scroll">
        {selected ? (
          <>
            <NodeSummary selected={selected} actions={actions} warnings={nodeWarnings} />
            <DetailState loading={detailLoading} error={detailQuery.error} />
            {detail?.task ? <TaskDetail detail={detail.task} /> : null}
            {detail?.evidence ? <EvidenceDetail detail={detail.evidence} /> : null}
            {warnings.length > 0 ? <WarningsSummary warnings={warnings} /> : null}
          </>
        ) : warnings.length > 0 ? (
          <Warnings warnings={warnings} />
        ) : (
          <div className="brain-inspector-empty">No node selected</div>
        )}
      </div>
    </aside>
  )
}

function NodeSummary({
  selected,
  actions,
  warnings,
}: {
  selected: BrainGraphNode
  actions: BrainGraphActionSpec[]
  warnings: BrainGraphWarning[]
}) {
  return (
    <>
      <div>
        <div className="brain-inspector-title">{selected.label}</div>
        <div className="brain-inspector-sub">
          <span className={`badge ${nodeTone(selected.status)}`}>
            <StatusDot status={selected.status} />
            {selected.status}
          </span>
          <span className="badge">{String(selected.type).replace(/_/g, ' ')}</span>
        </div>
      </div>

      {selected.summary ? <div className="brain-inspector-summary">{selected.summary}</div> : null}

      <div className="brain-kv">
        <KV k="id" v={selected.id} />
        {selected.task_slug ? <KV k="task" v={selected.task_slug} /> : null}
        {selected.owner_slug ? <KV k="owner" v={selected.owner_slug} /> : null}
        {selected.parent_task_slug ? <KV k="parent" v={selected.parent_task_slug} /> : null}
        {selected.provider ? <KV k="provider" v={selected.provider} /> : null}
        {selected.harness ? <KV k="harness" v={selected.harness} /> : null}
        {selected.permission_mode ? <KV k="permission" v={selected.permission_mode} /> : null}
        {selected.model ? <KV k="model" v={selected.model} /> : null}
      </div>

      {selected.ref ? <NodeRef node={selected} /> : null}
      {selected.actions && selected.actions.length > 0 ? <NodeActions node={selected} actions={actions} /> : null}
      {selected.metadata && Object.keys(selected.metadata).length > 0 ? <MetadataTable metadata={selected.metadata} /> : null}
      {warnings.length > 0 ? <Warnings warnings={warnings} /> : null}
    </>
  )
}

function DetailState({ loading, error }: { loading: boolean; error: unknown }) {
  if (loading) {
    return (
      <div className="brain-detail-state">
        <Loader2 size={14} className="spin" />
        <span>loading detail</span>
      </div>
    )
  }
  if (error) {
    return (
      <div className="brain-detail-state danger">
        <AlertTriangle size={14} />
        <span>{errorText(error)}</span>
      </div>
    )
  }
  return null
}

function TaskDetail({ detail }: { detail: BrainGraphTaskDetail }) {
  return (
    <>
      <DetailSection title="Task" icon={<FileText size={14} />}>
        <div className="brain-kv">
          <KV k="slug" v={detail.slug} />
          <KV k="status" v={detail.status} />
          <KV k="priority" v={detail.priority} />
          <KV k="project" v={detail.project_slug} />
          <KV k="parent" v={detail.parent_slug} />
          <KV k="work dir" v={detail.work_dir} />
          <KV k="worktree" v={detail.worktree_path} />
        </div>
      </DetailSection>
      <DetailSection title="Session" icon={<TerminalSquare size={14} />}>
        <div className="brain-kv">
          <KV k="provider" v={detail.session_provider} />
          <KV k="harness" v={detail.harness} />
          <KV k="permission" v={detail.permission_mode} />
          <KV k="model" v={detail.model} />
          <KV k="session" v={detail.session_id} />
          <KV k="path" v={detail.session_path} />
          {detail.transcript ? <KV k="transcript" v={detail.transcript.available ? 'available' : detail.transcript.message || 'unavailable'} /> : null}
        </div>
      </DetailSection>
      <DetailSection title="Files" icon={<ScrollText size={14} />}>
        <div className="brain-kv">
          <KV k="brief" v={detail.brief_path} />
        </div>
        {detail.updates.length > 0 ? (
          <div className="brain-detail-list">
            {detail.updates.map((update) => (
              <div className="brain-detail-list-row" key={update.path} title={update.path}>
                <span className="clip">{update.filename}</span>
                <strong>{dateTime(update.mtime)}</strong>
              </div>
            ))}
          </div>
        ) : null}
      </DetailSection>
      <TaskTranscriptPreview slug={detail.slug} enabled={Boolean(detail.session_id)} />
    </>
  )
}

function EvidenceDetail({ detail }: { detail: BrainGraphEvidenceDetail }) {
  return (
    <DetailSection title="Evidence" icon={<ScrollText size={14} />}>
      <div className="brain-kv">
        <KV k="kind" v={detail.kind} />
        <KV k="task" v={detail.task_slug} />
        <KV k="ref" v={detail.ref_id} />
        <KV k="state" v={detail.available ? 'available' : detail.message || 'unavailable'} />
        <KV k="path" v={detail.path} />
        <KV k="url" v={detail.url} />
      </div>
    </DetailSection>
  )
}

function DetailSection({ title, icon, children }: { title: string; icon: ReactNode; children: ReactNode }) {
  return (
    <div className="brain-detail-section">
      <div className="brain-detail-heading">
        {icon}
        <span>{title}</span>
      </div>
      {children}
    </div>
  )
}

function KV({ k, v }: { k: string; v?: string | number | null }) {
  const text = v == null || v === '' ? '—' : String(v)
  return (
    <div className="brain-kv-row">
      <span>{k}</span>
      <strong title={text}>{text}</strong>
    </div>
  )
}

function NodeRef({ node }: { node: BrainGraphNode }) {
  if (!node.ref) return null
  if (node.ref.kind === 'task') {
    return (
      <Link className="brain-ref-link" href={`/session/${encodeURIComponent(node.ref.id)}`}>
        <GitBranch size={14} />
        <span className="clip">{node.ref.id}</span>
        <ArrowUpRight size={13} />
      </Link>
    )
  }
  if (node.ref.url) {
    return (
      <a className="brain-ref-link" href={node.ref.url} target="_blank" rel="noreferrer">
        <GitBranch size={14} />
        <span className="clip">{node.ref.id}</span>
        <ArrowUpRight size={13} />
      </a>
    )
  }
  return (
    <div className="brain-ref-static">
      <GitBranch size={14} />
      <span className="clip">{node.ref.kind}:{node.ref.id}</span>
    </div>
  )
}

function NodeActions({ node, actions }: { node: BrainGraphNode; actions: BrainGraphActionSpec[] }) {
  const graphAction = useBrainGraphAction()
  const [, navigate] = useLocation()
  const [promptAction, setPromptAction] = useState<BrainGraphActionSpec | null>(null)
  const [prompt, setPrompt] = useState('')
  const pendingAction = graphAction.isPending ? graphAction.variables?.action : ''

  const run = async (key: string, action?: BrainGraphActionSpec) => {
    const enabled = action?.enabled ?? true
    if (!enabled || graphAction.isPending) return
    if (key === 'seed' || key === 'send_event') {
      setPrompt('')
      setPromptAction(action ?? { key, label: key.replace(/_/g, ' '), risky: false, enabled: true })
      return
    }
    try {
      const resp = await graphAction.mutateAsync({ action: key, node_id: node.id })
      if ((key === 'open_session' || key === 'resume') && resp.action_response?.bridge) {
        const slug = resp.action_response.agent?.slug || node.task_slug
        if (slug) navigate(`/session/${encodeURIComponent(slug)}`)
      }
    } catch {
      // The hook emits the toast; keep the click handler quiet.
    }
  }
  const submitPromptAction = async () => {
    const text = prompt.trim()
    if (!promptAction || !text || graphAction.isPending) return
    try {
      await graphAction.mutateAsync({ action: promptAction.key, node_id: node.id, prompt: text })
      setPrompt('')
      setPromptAction(null)
    } catch {
      // The hook emits the toast; keep the modal open so the text can be retried.
    }
  }

  return (
    <>
      <div className="brain-action-list">
        {(node.actions ?? []).map((key) => {
          const action = actionByKey(actions, key)
          const enabled = action?.enabled ?? true
          const pending = pendingAction === key
          return (
            <button
              type="button"
              className={`brain-action-button ${action?.risky ? 'risky' : ''}`}
              key={key}
              title={action?.disabled_reason || undefined}
              disabled={!enabled || graphAction.isPending}
              aria-busy={pending}
              onClick={() => void run(key, action)}
            >
              {pending ? <Loader2 size={13} className="spin" /> : null}
              <span>{action?.label ?? key.replace(/_/g, ' ')}</span>
            </button>
          )
        })}
      </div>
      <Modal
        open={Boolean(promptAction)}
        onClose={() => {
          if (graphAction.isPending) return
          setPromptAction(null)
          setPrompt('')
        }}
        title={promptAction?.label ?? 'Send input'}
        width={560}
        footer={
          <>
            <button type="button" className="btn" disabled={graphAction.isPending} onClick={() => setPromptAction(null)}>
              Cancel
            </button>
            <button type="button" className="btn primary" disabled={!prompt.trim() || graphAction.isPending} onClick={submitPromptAction}>
              {graphAction.isPending ? <Loader2 size={14} className="spin" /> : <SendHorizontal size={14} />}
              Send
            </button>
          </>
        }
      >
        <div className="brain-action-modal">
          <div className="brain-action-modal-target">
            <span>task</span>
            <strong>{node.task_slug || node.id}</strong>
          </div>
          <textarea
            className="textarea brain-action-prompt"
            aria-label={promptAction?.key === 'seed' ? 'Seed input' : 'Session event'}
            rows={6}
            value={prompt}
            disabled={graphAction.isPending}
            placeholder={promptAction?.key === 'seed' ? 'Seed input for this task session…' : 'Event to send into this task session…'}
            onChange={(e) => setPrompt(e.target.value)}
            onKeyDown={(e) => {
              if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
                e.preventDefault()
                void submitPromptAction()
              }
            }}
          />
        </div>
      </Modal>
    </>
  )
}

function TaskTranscriptPreview({ slug, enabled }: { slug: string; enabled: boolean }) {
  const { data, isLoading, error } = useTaskTranscript(slug, enabled)
  if (!enabled) return null
  if (isLoading) {
    return (
      <DetailSection title="Transcript" icon={<ScrollText size={14} />}>
        <div className="brain-detail-state">
          <Loader2 size={14} className="spin" />
          <span>loading transcript</span>
        </div>
      </DetailSection>
    )
  }
  if (error) {
    return (
      <DetailSection title="Transcript" icon={<ScrollText size={14} />}>
        <div className="brain-detail-state danger">
          <AlertTriangle size={14} />
          <span>{errorText(error)}</span>
        </div>
      </DetailSection>
    )
  }
  if (!data?.available || data.entries.length === 0) {
    return (
      <DetailSection title="Transcript" icon={<ScrollText size={14} />}>
        <div className="brain-detail-text">{data?.message || 'No transcript captured yet.'}</div>
      </DetailSection>
    )
  }
  const entries = data.entries.slice(-6)
  return (
    <DetailSection title="Transcript" icon={<ScrollText size={14} />}>
      <div className="brain-transcript-list">
        {entries.map((entry, index) => (
          <div className={`brain-transcript-row ${entry.is_error ? 'danger' : ''}`} key={`${entry.byte_offset}:${index}`}>
            <div className="brain-transcript-meta">
              <span>{entry.type}</span>
              {entry.timestamp ? <strong>{dateTime(entry.timestamp)}</strong> : null}
            </div>
            <div className="brain-transcript-text">{transcriptEntryText(entry)}</div>
          </div>
        ))}
      </div>
    </DetailSection>
  )
}

function transcriptEntryText(entry: TranscriptEntry) {
  if (entry.type === 'tool_use') return `${entry.tool_name ?? 'tool'} ${entry.tool_input_summary ?? ''}`.trim()
  if (entry.type === 'tool_result') return entry.tool_result_text || ''
  return entry.text || ''
}

function MetadataTable({ metadata }: { metadata: Record<string, string> }) {
  return (
    <div className="brain-metadata">
      {Object.entries(metadata).map(([key, value]) => (
        <KV key={key} k={key} v={value} />
      ))}
    </div>
  )
}

function Warnings({ warnings }: { warnings: BrainGraphWarning[] }) {
  return (
    <div className="brain-warning-list">
      {warnings.map((warning) => (
        <div className="brain-warning" key={`${warning.code}:${warning.node_id}:${warning.message}`}>
          <AlertTriangle size={14} />
          <span>{warning.message}</span>
        </div>
      ))}
    </div>
  )
}

function WarningsSummary({ warnings }: { warnings: BrainGraphWarning[] }) {
  return (
    <div className="brain-inspector-section">
      <div className="brain-inspector-head">
        <AlertTriangle size={15} />
        <span>Warnings</span>
        <span className="badge warn">{warnings.length}</span>
      </div>
      <Warnings warnings={warnings.slice(0, 5)} />
    </div>
  )
}
