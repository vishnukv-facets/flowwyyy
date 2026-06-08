import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { useLocation, useSearch } from 'wouter'
import { AlertTriangle, ArrowRight, AtSign, BellOff, Check, ChevronDown, ExternalLink, Filter, Github, Handshake, Hash, Inbox, Info, ListPlus, Lock, MessageSquare, Play, RefreshCw, Send, Share2 } from 'lucide-react'
import { useAction, useAttention, useAttentionDecision, useAttentionTrace, useWorkEvents } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { EmptyState, ErrorNote, Loading, SourceIcon } from '../components/ui'
import { WorkEventRow } from '../components/WorkEventRow'
import { Modal } from '../components/Modal'
import { dateTimeFull, dateTimeSec, titleCase } from '../lib/format'
import { nextTraceWindowAnchor, traceSinceForWindow } from '../lib/traceWindow'
import type { AttentionItem, SteeringFunnel, SteeringTrace, WorkEvent } from '../lib/types'

const STATUSES = ['new', 'acted', 'dismissed', 'all'] as const
const VIEWS = ['feed', 'trace'] as const
type View = (typeof VIEWS)[number]

export function Attention() {
  useDocumentTitle('Attention')
  const search = useSearch()
  const [, navigate] = useLocation()
  const params = useMemo(() => new URLSearchParams(search), [search])
  const routedView: View = params.get('view') === 'trace' ? 'trace' : 'feed'
  const routedItem = routedView === 'feed' ? params.get('item') : null
  const routedTrace = routedView === 'trace' ? params.get('trace') : null
  const view = routedView

  const chooseView = (next: View) => {
    navigate(next === 'trace' ? '/attention?view=trace' : '/attention')
  }
  const clearDeepLink = () => {
    navigate(routedView === 'trace' ? '/attention?view=trace' : '/attention')
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="eyebrow">attention</div>
          <h1 className="h-xl">Attention Feed</h1>
        </div>
        <div className="spacer" />
        <div className="row gap">
          {VIEWS.map((v) => (
            <button
              key={v}
              type="button"
              className={`btn sm ${view === v ? 'primary' : 'ghost'}`}
              onClick={() => chooseView(v)}
            >
              {v}
            </button>
          ))}
        </div>
      </div>

      {view === 'feed' ? (
        <FeedView selectedItemId={routedItem} onClearDeepLink={clearDeepLink} />
      ) : (
        <TraceView selectedTraceId={routedTrace} onClearDeepLink={clearDeepLink} />
      )}
    </div>
  )
}

// signature of the fields a re-triage can change — used to detect when a
// re-triaged card has actually been refreshed (so we can stop its spinner).
const cardSig = (it: AttentionItem) =>
  `${it.suggested_action}|${it.matched_task ?? ''}|${it.confidence}|${it.reason ?? ''}|${it.summary ?? ''}`

function FeedView({
  selectedItemId,
  onClearDeepLink,
}: {
  selectedItemId?: string | null
  onClearDeepLink: () => void
}) {
  const [status, setStatus] = useState<string>('new')
  const [detail, setDetail] = useState<AttentionItem | null>(null)
  const { data, isLoading, error } = useAttention(status)
  const { data: workEvents } = useWorkEvents({ limit: 200 })
  const action = useAction()
  const eventByAttentionId = useMemo(() => {
    const map = new Map<string, WorkEvent>()
    for (const event of workEvents?.items ?? []) {
      if (event.id.startsWith('attention:')) map.set(event.id.slice('attention:'.length), event)
    }
    return map
  }, [workEvents])
  const routedDetail = selectedItemId ? (data ?? []).find((it) => it.id === selectedItemId) ?? null : null
  const activeDetail = detail ?? routedDetail
  // Cards with an in-flight re-triage → id mapped to the card signature at click
  // time. Deep triage is async (~a minute), so we hold a spinner until the card's
  // content actually changes (SSE refetch) or a safety timeout fires.
  const [retriaging, setRetriaging] = useState<Record<string, string>>({})

  // Stop the spinner once a re-triaged card's content has changed.
  useEffect(() => {
    if (Object.keys(retriaging).length === 0) return
    setRetriaging((prev) => {
      let changed = false
      const next = { ...prev }
      for (const it of data ?? []) {
        if (next[it.id] !== undefined && next[it.id] !== cardSig(it)) {
          delete next[it.id]
          changed = true
        }
      }
      return changed ? next : prev
    })
  }, [data]) // eslint-disable-line react-hooks/exhaustive-deps

  const act = (item: AttentionItem, verb: string) => {
    if (verb === 'retriage') {
      if (retriaging[item.id]) return // already re-running for this card
      setRetriaging((r) => ({ ...r, [item.id]: cardSig(item) }))
      action.mutate({ kind: 'attention-act', target: item.id, attention_action: verb })
      // Safety net: clear the spinner even if the verdict comes back identical.
      window.setTimeout(
        () => setRetriaging((r) => { const n = { ...r }; delete n[item.id]; return n }),
        120000,
      )
      return
    }
    if (action.isPending) return
    action.mutate({ kind: 'attention-act', target: item.id, attention_action: verb })
  }

  return (
    <>
      <div className="row gap" style={{ marginBottom: 16 }}>
        {STATUSES.map((s) => (
          <button
            key={s}
            type="button"
            className={`btn sm ${status === s ? 'primary' : 'ghost'}`}
            onClick={() => setStatus(s)}
          >
            {s}
          </button>
        ))}
      </div>

      {isLoading ? (
        <Loading label="loading attention feed" />
      ) : error ? (
        <ErrorNote error={error} />
      ) : (data ?? []).length === 0 ? (
        <EmptyState
          title="Nothing needs you"
          hint="The steerer surfaces messages worth your attention here — from watched channels, DMs, and mentions."
        />
      ) : (
        <div className="att-list">
          {(data ?? []).map((it) => (
            <AttentionCard
              key={it.id}
              item={it}
              workEvent={eventByAttentionId.get(it.id)}
              disabled={action.isPending}
              retriaging={!!retriaging[it.id]}
              onAct={act}
              onOpen={() => setDetail(it)}
            />
          ))}
        </div>
      )}

      <FeedDetail
        item={activeDetail}
        workEvent={activeDetail ? eventByAttentionId.get(activeDetail.id) : undefined}
        onClose={() => {
          if (selectedItemId) onClearDeepLink()
          setDetail(null)
        }}
      />
    </>
  )
}

// Small glyph for the origin line: a Slack channel (#) vs DM (lock/@) vs GitHub
// repo. `channel_type` carries slack's im/mpim/channel hint when present.
function OriginIcon({ source, channelType }: { source?: string; channelType?: string }) {
  if (source === 'github') return <Github size={12} />
  const isDM = channelType === 'im' || channelType === 'mpim' || channelType === 'dm'
  if (source === 'slack') {
    if (channelType === 'mpim') return <AtSign size={12} />
    if (isDM) return <Lock size={12} />
    return <Hash size={12} />
  }
  return <MessageSquare size={12} />
}

function AttentionCard({
  item,
  workEvent,
  disabled,
  retriaging,
  onAct,
  onOpen,
}: {
  item: AttentionItem
  workEvent?: WorkEvent
  disabled: boolean
  retriaging?: boolean
  onAct: (item: AttentionItem, verb: string) => void
  onOpen: () => void
}) {
  const urgent = item.urgency === 'urgent'
  // Re-triage is in flight if the client just fired it OR the server says so
  // (the server flag survives a page refresh and blocks double-firing).
  const busy = !!retriaging || !!item.retriaging
  const [, navigate] = useLocation()
  const channelLabel = item.channel_name || item.channel || ''
  const linkLabel =
    item.source === 'slack' ? 'View in Slack' : item.source === 'github' ? 'View on GitHub' : 'Open source'
  // Clicking the card body opens the decision detail; the origin link and the
  // action rows stop propagation so their own onClicks don't also open it.
  const stop = (e: { stopPropagation: () => void }) => e.stopPropagation()
  return (
    <div
      className={`card att-card clickable${urgent ? ' att-urgent' : ''}`}
      role="button"
      tabIndex={0}
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          onOpen()
        }
      }}
    >
      <div className="att-head row gap">
        <SourceIcon source={item.source} />
        <span className="badge accent">{item.suggested_action.replace(/_/g, ' ')}</span>
        {item.handoff ? <span className={`badge ${handoffTone(item.handoff.status)}`}>handoff {item.handoff.status}</span> : null}
        {item.urgency ? <span className={`badge ${urgent ? 'warn' : ''}`}>{item.urgency}</span> : null}
        {item.is_vip ? <span className="badge info">vip</span> : null}
        <span className="spacer" />
        <span className="num faint" title="confidence">{Math.round(item.confidence * 100)}%</span>
      </div>

      {channelLabel || item.permalink || item.author_name ? (
        <div className="att-origin row gap" onClick={stop}>
          <span className="att-origin-where faint" title={channelLabel || undefined}>
            <OriginIcon source={item.source} channelType={item.channel_type} />
            <span>{channelLabel || '—'}</span>
          </span>
          {item.author_name ? <span className="att-origin-from faint">from {item.author_name}</span> : null}
          {item.permalink ? (
            <a
              className="btn ghost sm"
              href={item.permalink}
              target="_blank"
              rel="noreferrer"
              onClick={() => onAct(item, 'open-source')}
            >
              <ExternalLink size={13} /> {linkLabel}
            </a>
          ) : null}
        </div>
      ) : null}

      <div className="att-summary">{item.summary || <span className="faint">(no summary)</span>}</div>
      <WorkEventRow event={workEvent} onOpen={(href) => navigate(href)} />
      <WhyThis item={item} compact onNavigate={(slug) => navigate(`/session/${slug}`)} />
      <HandoffStatus item={item} />

      {item.draft ? (
        <div className="att-draft">
          <div className="eyebrow">drafted reply</div>
          <div className="att-draft-body">{item.draft}</div>
        </div>
      ) : null}

      {item.status === 'new' ? (
        <div className="att-actions row gap" onClick={stop}>
          <button type="button" className="btn primary sm" disabled={disabled} onClick={() => onAct(item, 'make-task')}>
            <ListPlus size={13} /> Make task
          </button>
          <button type="button" className="btn sm" disabled={disabled} onClick={() => onAct(item, 'make-task-start')}>
            <Play size={13} /> Make task & start
          </button>
          {item.matched_task ? (
            <>
              <button
                type="button"
                className="btn sm"
                disabled={disabled || item.handoff?.status === 'pending'}
                onClick={() => onAct(item, 'confirm-handoff')}
              >
                <Handshake size={13} /> Ask owner
              </button>
              <button type="button" className="btn sm" disabled={disabled} onClick={() => onAct(item, 'forward')}>
                <Share2 size={13} /> Forward
              </button>
            </>
          ) : null}
          {item.draft ? (
            // Opens the detail modal (review/edit before sending) rather than
            // blind-sending — the action row already stopPropagation's the
            // card-body open, so call onOpen explicitly here.
            <button type="button" className="btn sm" disabled={disabled} onClick={onOpen}>
              <Send size={13} /> Send reply
            </button>
          ) : null}
          <button type="button" className="btn ghost sm" disabled={disabled} onClick={() => onAct(item, 'dismiss')}>
            <Check size={13} /> Dismiss
          </button>
          <button
            type="button"
            className="btn icon ghost sm"
            title={busy ? 'Re-running triage…' : 'Re-run triage (re-read task context, refresh the decision)'}
            aria-label="Re-run triage"
            disabled={disabled || busy}
            onClick={() => onAct(item, 'retriage')}
          >
            <RefreshCw size={13} className={busy ? 'spin' : undefined} />
          </button>
          {busy ? <span className="dim mono" style={{ fontSize: 11.5 }}>re-triaging…</span> : null}
          <MuteMenu item={item} disabled={disabled} onAct={onAct} />
        </div>
      ) : (
        <div className="att-resolved row gap faint mono" onClick={stop}>
          <span>
            {item.status}
            {item.acted_at ? ` · ${item.acted_at.slice(0, 10)}` : ''}
          </span>
          {item.linked_task ? (
            <button
              type="button"
              className="btn ghost sm"
              onClick={() => {
                onAct(item, 'open-session')
                navigate(`/session/${item.linked_task}`)
              }}
            >
              <ArrowRight size={13} /> Go to session
            </button>
          ) : null}
        </div>
      )}
    </div>
  )
}

function handoffTone(status?: string): string {
  switch (status) {
    case 'accepted':
      return 'accent'
    case 'declined':
    case 'timeout':
      return 'warn'
    default:
      return ''
  }
}

function HandoffStatus({ item }: { item: AttentionItem }) {
  const h = item.handoff
  if (!h) return null
  const target = item.why?.matched_task?.name || h.receiver
  const when =
    h.status === 'pending'
      ? `expires ${dateTimeSec(h.expires_at)}`
      : h.responded_at
        ? dateTimeSec(h.responded_at)
        : dateTimeSec(h.requested_at)
  return (
    <div className="att-evidence-meta row gap faint" style={{ marginTop: 8 }}>
      <Handshake size={13} />
      <span>handoff {h.status}</span>
      <span>{target}</span>
      <span>{when}</span>
      {h.reason ? <span className="clip">reason: {h.reason}</span> : null}
    </div>
  )
}

// MuteMenu is the "perma drop" control: a small dropdown that permanently
// suppresses future messages by channel, sender, or just this thread. The mute
// is recorded server-side (steering_mutes) and Stage 0 drops matching events on
// the next message — and any open cards matching it are cleared immediately.
function MuteMenu({
  item,
  disabled,
  onAct,
  align = 'right',
}: {
  item: AttentionItem
  disabled?: boolean
  onAct: (item: AttentionItem, verb: string) => void
  align?: 'left' | 'right'
}) {
  const [open, setOpen] = useState(false)
  const choose = (verb: string) => {
    setOpen(false)
    onAct(item, verb)
  }
  const chanLabel = item.channel_name || 'this channel'
  const senderLabel = item.author_name || 'this sender'
  return (
    <div className={`mute-menu ${align}`}>
      <button
        type="button"
        className="btn ghost sm"
        disabled={disabled}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
      >
        <BellOff size={13} /> Mute <ChevronDown size={11} />
      </button>
      {open ? (
        <>
          <div className="mute-backdrop" onClick={() => setOpen(false)} />
          <div className="mute-pop" role="menu">
            {item.channel ? (
              <button type="button" role="menuitem" className="mute-item" onClick={() => choose('mute-channel')}>
                <Hash size={12} className="faint" /> Mute channel <span className="clip">{chanLabel}</span>
              </button>
            ) : null}
            {item.author ? (
              <button type="button" role="menuitem" className="mute-item" onClick={() => choose('mute-sender')}>
                <AtSign size={12} className="faint" /> Mute sender <span className="clip">{senderLabel}</span>
              </button>
            ) : null}
            <button type="button" role="menuitem" className="mute-item" onClick={() => choose('mute-thread')}>
              <BellOff size={12} className="faint" /> Never show this thread
            </button>
          </div>
        </>
      ) : null}
    </div>
  )
}

function WhyThis({
  item,
  compact = false,
  onNavigate,
}: {
  item: AttentionItem
  compact?: boolean
  onNavigate?: (slug: string) => void
}) {
  const why = item.why ?? { source: item.source, confidence: item.confidence }
  const match = why.matched_task
  const stage =
    why.stage_action
      ? `${titleCase(why.stage_action.replace(/_/g, ' '))} · ${pctConf(why.stage_confidence)}`
      : why.stage_reached || '—'
  const evidence = why.context_summary || item.channel_name || item.thread_key || titleCase(why.source || item.source || 'source')
  const confidence = pctConf(why.confidence ?? item.confidence)
  const participants = (why.participants ?? []).slice(0, compact ? 3 : 6).join(', ')
  const taskStatus = [match?.status, match?.priority, match?.project_slug].filter(Boolean).join(' · ')
  return (
    <div className={`att-why${compact ? ' compact' : ''}`}>
      <div className="att-why-title">
        <Info size={13} /> why this
        <span className="spacer" />
        <span className="mono faint">{confidence}</span>
      </div>
      <div className="att-why-grid">
        <span>source</span>
        <strong>{evidence}</strong>
        {match || item.matched_task ? (
          <>
            <span>match</span>
            <div className="att-match">
              <div>
                <strong>{match?.name || match?.slug || item.matched_task}</strong>
                <div className="mono faint">
                  {match?.slug || item.matched_task}
                  {taskStatus ? ` · ${taskStatus}` : ''}
                </div>
              </div>
              {onNavigate && (match?.slug || item.matched_task) ? (
                <button
                  type="button"
                  className="btn ghost sm"
                  onClick={(e) => {
                    e.stopPropagation()
                    onNavigate(match?.slug || item.matched_task || '')
                  }}
                >
                  <ArrowRight size={13} /> Open
                </button>
              ) : null}
            </div>
          </>
        ) : why.suggested_project || why.suggested_priority ? (
          <>
            <span>target</span>
            <strong>{[why.suggested_project, why.suggested_priority].filter(Boolean).join(' · ')}</strong>
          </>
        ) : null}
        <span>reason</span>
        <strong>{why.reason || item.reason || 'No reason recorded.'}</strong>
        <span>stage</span>
        <strong>{stage}</strong>
      </div>
      {!compact ? (
        <div className="att-evidence">
          {why.parent_preview ? (
            <div>
              <span className="eyebrow">parent</span>
              <div>{why.parent_preview}</div>
            </div>
          ) : null}
          {why.latest_preview ? (
            <div>
              <span className="eyebrow">latest</span>
              <div>{why.latest_preview}</div>
            </div>
          ) : null}
          <div className="att-evidence-meta row gap faint">
            {why.evidence_count ? <span>{why.evidence_count} evidence item{why.evidence_count === 1 ? '' : 's'}</span> : null}
            {participants ? <span>participants: {participants}</span> : null}
            {why.fetch_status && why.fetch_status !== 'ok' ? <span>context: {why.fetch_status}</span> : null}
          </div>
          {why.fetch_error ? <div className="warn-text">{why.fetch_error}</div> : null}
        </div>
      ) : null}
    </div>
  )
}

function ActionPreviewList({ item, previews }: { item: AttentionItem; previews?: AttentionItem['action_previews'] }) {
  if (!previews || previews.length === 0) {
    return <div className="faint" style={{ marginTop: 6 }}>No actions are available for this card.</div>
  }
  return (
    <div className="att-action-preview">
      {previews.map((p) => {
        const target = actionPreviewTarget(item, p)
        return (
          <div key={p.action} className={`att-action-preview-row${p.primary ? ' primary' : ''}${p.destructive ? ' destructive' : ''}`}>
            <div>
              <div className="row gap">
                <strong>{p.label}</strong>
                {p.primary ? <span className="badge accent">suggested</span> : null}
                {p.destructive ? <span className="badge warn">suppresses</span> : null}
              </div>
              <div className="dim">{p.description}</div>
            </div>
            {target ? <span className="mono faint">{target}</span> : null}
          </div>
        )
      })}
    </div>
  )
}

function actionPreviewTarget(item: AttentionItem, p: NonNullable<AttentionItem['action_previews']>[number]): string {
  switch (p.action) {
    case 'mute_channel':
      return item.channel_name || p.target || ''
    case 'mute_sender':
      return item.author_name || p.target || ''
    case 'forward':
    case 'confirm_handoff':
      return item.why?.matched_task?.name || item.why?.matched_task?.slug || p.target || ''
    default:
      return p.target || ''
  }
}

// ----- Feed detail modal --------------------------------------------------
// Clicking a feed card opens this: the message context taken straight from the
// item, plus the SAME cascade-decision grid the Trace view shows — fetched by
// feed id — so the operator can audit "why was this surfaced / chosen". Reuses
// the TraceDetail layout (.meta-grid + KV, .td-section, .att-draft).
function FeedDetail({ item, workEvent, onClose }: { item: AttentionItem | null; workEvent?: WorkEvent; onClose: () => void }) {
  // Keep the modal mounted (empty) while closed so content doesn't blank mid-anim.
  if (!item) {
    return (
      <Modal open={false} onClose={onClose} title="">
        {null}
      </Modal>
    )
  }
  return <FeedDetailOpen key={item.id} item={item} workEvent={workEvent} onClose={onClose} />
}

function FeedDetailOpen({ item, workEvent, onClose }: { item: AttentionItem; workEvent?: WorkEvent; onClose: () => void }) {
  const { data: trace, isLoading, isError } = useAttentionDecision(item.id)
  const action = useAction()
  const [, navigate] = useLocation()
  // This component is keyed by item.id, so switching cards remounts it and
  // naturally resets the editable draft without syncing props through an effect.
  const [replyText, setReplyText] = useState(item.draft ?? '')
  const [replyInstructions, setReplyInstructions] = useState('')

  const sendReply = () => {
    if (action.isPending || !replyText.trim()) return
    action.mutate(
      {
        kind: 'attention-act',
        target: item.id,
        attention_action: 'send-reply',
        reply_text: replyText,
        reply_instructions: replyInstructions.trim() || undefined,
      },
      // Slack sends spin an ephemeral floating session that posts via the Slack
      // MCP in the background — DON'T pop it open; it appears as a tray chip the
      // operator can click to watch. It self-closes on success, stays on failure.
      { onSuccess: onClose },
    )
  }

  const muteAct = (_it: AttentionItem, verb: string) => {
    if (action.isPending) return
    action.mutate(
      { kind: 'attention-act', target: item.id, attention_action: verb },
      { onSuccess: onClose },
    )
  }
  const recordOpenSource = () => {
    if (action.isPending) return
    action.mutate({ kind: 'attention-act', target: item.id, attention_action: 'open-source' })
  }

  const sourceLabel = item.source === 'github' ? 'GitHub' : titleCase(item.source || 'message')
  const title = `${sourceLabel} · ${titleCase(item.suggested_action || '—')}`
  const channel = item.channel_name || item.channel || '—'
  const from = item.author_name || '—'
  const linkLabel = item.source === 'github' ? 'Open on GitHub' : 'Open in Slack'

  return (
    <Modal open onClose={onClose} title={title} width={620}>
      <div className="trace-detail-view">
        <div className="meta-grid">
          <KV k="when" v={dateTimeFull(item.created_at)} />
          <KV k="channel" v={channel} />
          <KV k="from" v={from} />
          <KV k="confidence" v={`${Math.round(item.confidence * 100)}%`} />
        </div>

        <div className="td-section">
          <div className="eyebrow">summary</div>
          <div className="att-draft-body td-message">{item.summary || '(no summary)'}</div>
        </div>

        {workEvent ? (
          <div className="td-section">
            <div className="eyebrow">work event</div>
            <WorkEventRow event={workEvent} onOpen={(href) => navigate(href)} />
          </div>
        ) : null}

        <div className="td-section">
          <WhyThis
            item={item}
            onNavigate={(slug) => {
              onClose()
              navigate(`/session/${slug}`)
            }}
          />
        </div>

        <div className="td-section">
          <div className="eyebrow">action preview</div>
          <ActionPreviewList item={item} previews={item.action_previews} />
        </div>

        {item.draft ? (
          item.status === 'new' ? (
            // Editable before sending — the agent posts the (possibly edited)
            // text from its own session (Slack/GitHub).
            <div className="td-section">
              <div className="eyebrow">drafted reply</div>
              <textarea
                className="input"
                rows={4}
                value={replyText}
                onChange={(e) => setReplyText(e.target.value)}
                style={{ marginTop: 5 }}
              />
              <div className="eyebrow" style={{ marginTop: 10 }}>instructions for the agent (optional)</div>
              <textarea
                className="input"
                rows={2}
                value={replyInstructions}
                placeholder="e.g. make it shorter · keep it formal · also ask when the data was last refreshed"
                onChange={(e) => setReplyInstructions(e.target.value)}
                style={{ marginTop: 5 }}
              />
              <div className="row gap between" style={{ marginTop: 8 }}>
                <span className="config-help" style={{ margin: 0 }}>
                  {replyInstructions.trim()
                    ? 'The agent will revise the draft per your instructions, then post.'
                    : 'Sends the draft as-is. Add instructions above to have the agent revise first.'}
                </span>
                <button
                  type="button"
                  className="btn primary sm"
                  disabled={action.isPending || !replyText.trim()}
                  onClick={sendReply}
                >
                  <Send size={13} /> {replyInstructions.trim() ? 'Revise & send' : 'Send reply'}
                </button>
              </div>
            </div>
          ) : (
            <div className="td-section">
              <div className="eyebrow">drafted reply</div>
              <div className="att-draft-body td-message">{item.draft}</div>
            </div>
          )
        ) : null}

        <div className="td-section">
          <div className="eyebrow">thread</div>
          <div className="row gap" style={{ marginTop: 5 }}>
            <span className="mono dim td-thread" title={item.thread_key || ''}>
              {item.thread_key || '—'}
            </span>
            {item.permalink ? (
              <a className="btn ghost sm" href={item.permalink} target="_blank" rel="noreferrer" onClick={recordOpenSource}>
                <ExternalLink size={13} /> {linkLabel}
              </a>
            ) : null}
          </div>
        </div>

        <div className="td-section">
          <div className="eyebrow">suppress</div>
          <div className="row gap between" style={{ marginTop: 5 }}>
            <span className="config-help" style={{ margin: 0 }}>
              Permanently drop messages like this — by channel, sender, or just this thread.
            </span>
            <MuteMenu item={item} disabled={action.isPending} onAct={muteAct} align="right" />
          </div>
        </div>

        <div className="td-section">
          <div className="eyebrow">why · cascade decision</div>
          {isLoading ? (
            <div style={{ marginTop: 6 }}>
              <Loading label="loading decision trace" />
            </div>
          ) : isError || !trace ? (
            <>
              <div className="faint" style={{ marginTop: 6 }}>
                No cascade trace recorded for this item (it predates decision logging).
              </div>
              {item.reason ? <div className="att-reason dim" style={{ marginTop: 6 }}>{item.reason}</div> : null}
            </>
          ) : (
            <div className="meta-grid" style={{ marginTop: 6 }}>
              <KV k="disposition" v={<span className={DISPOSITION_TONE[trace.disposition] ?? 'badge'}>{trace.disposition}</span>} />
              <KV k="stage reached" v={trace.stage_reached || '—'} />
              <KV k="stage 1 relevant" v={relevantLabel(trace.stage1_relevant)} />
              <KV k="stage 1 reason" v={trace.stage1_reason || '—'} />
              <KV k="stage 2 action" v={trace.stage2_action ? `${trace.stage2_action} · ${pctConf(trace.stage2_confidence)}` : '—'} />
              <KV k="stage 3 action" v={trace.stage3_action ? `${trace.stage3_action} · ${pctConf(trace.stage3_confidence)}` : '—'} />
              <KV k="final action" v={trace.final_action ? `${trace.final_action} · ${pctConf(trace.final_confidence)}` : '—'} />
              <KV k="drop reason" v={trace.drop_reason || '—'} />
              <KV k="latency" v={trace.latency_ms != null ? `${trace.latency_ms} ms` : '—'} />
              <KV k="model" v={trace.model || '—'} />
              {trace.error ? <KV k="error" v={<span className="dim">{trace.error}</span>} /> : null}
            </div>
          )}
        </div>
      </div>
    </Modal>
  )
}

// ----- Trace (decision-log) view -----------------------------------------
const WINDOWS = [
  { id: '1h', label: '1h', ms: 60 * 60 * 1000 },
  { id: '24h', label: '24h', ms: 24 * 60 * 60 * 1000 },
  { id: '7d', label: '7d', ms: 7 * 24 * 60 * 60 * 1000 },
] as const

const DISPOSITIONS = ['all', 'surfaced', 'dropped', 'error'] as const
const SOURCES = ['all', 'slack', 'github'] as const

// A labeled segmented control reusing the same .btn sm primary/ghost pattern as
// the window buttons. Used for the disposition + source row filters.
function SegFilter({
  label,
  options,
  value,
  onChange,
}: {
  label: string
  options: readonly string[]
  value: string
  onChange: (v: string) => void
}) {
  return (
    <div className="row gap trace-filter">
      <span className="eyebrow trace-filter-label">{label}</span>
      {options.map((o) => (
        <button
          key={o}
          type="button"
          className={`btn sm ${value === o ? 'primary' : 'ghost'}`}
          onClick={() => onChange(o)}
        >
          {o}
        </button>
      ))}
    </div>
  )
}

function TraceView({
  selectedTraceId,
  onClearDeepLink,
}: {
  selectedTraceId?: string | null
  onClearDeepLink: () => void
}) {
  const [windowAnchor, setWindowAnchor] = useState(() => nextTraceWindowAnchor(null, '24h', Date.now()))
  const windowId = windowAnchor.windowId
  const [disposition, setDisposition] = useState<string>('all')
  const [source, setSource] = useState<string>('all')
  const [selected, setSelected] = useState<SteeringTrace | null>(null)
  const win = WINDOWS.find((w) => w.id === windowId) ?? WINDOWS[1]
  const since = traceSinceForWindow(windowAnchor, win.ms)
  const { data, isLoading, error } = useAttentionTrace(since, disposition, source)
  const items = data?.items ?? []
  const routedSelected = selectedTraceId ? items.find((it) => it.id === selectedTraceId) ?? null : null
  const activeSelected = selected ?? routedSelected
  const chooseWindow = (id: string) => setWindowAnchor((current) => nextTraceWindowAnchor(current, id, Date.now()))

  return (
    <>
      <div className="trace-controls">
        <div className="row gap">
          {WINDOWS.map((w) => (
            <button
              key={w.id}
              type="button"
              className={`btn sm ${windowId === w.id ? 'primary' : 'ghost'}`}
              onClick={() => chooseWindow(w.id)}
            >
              {w.label}
            </button>
          ))}
        </div>
        <SegFilter label="disposition" options={DISPOSITIONS} value={disposition} onChange={setDisposition} />
        <SegFilter label="source" options={SOURCES} value={source} onChange={setSource} />
      </div>

      {/* Funnel stays full-window: the backend leaves it unfiltered so the row
          filters above only narrow the list, not the totals. */}
      {data ? <FunnelStrip funnel={data.funnel} /> : null}

      {isLoading ? (
        <Loading label="loading triage decisions" />
      ) : error ? (
        <ErrorNote error={error} />
      ) : items.length === 0 ? (
        <EmptyState
          title="No decisions yet"
          hint="No triage decisions in this window. The steerer logs every message it sees here."
        />
      ) : (
        <div className="trace-list">
          <div className="trace-row trace-head faint mono">
            <span>time</span>
            <span>source</span>
            <span>disposition</span>
            <span>stage</span>
            <span className="trace-conf">conf</span>
            <span>channel</span>
            <span>from</span>
            <span>detail</span>
          </div>
          {items.map((it) => (
            <TraceRow key={it.id} item={it} onOpen={() => setSelected(it)} />
          ))}
        </div>
      )}

      <TraceDetail
        item={activeSelected}
        onClose={() => {
          if (selectedTraceId) onClearDeepLink()
          setSelected(null)
        }}
      />
    </>
  )
}

function FunnelStrip({ funnel }: { funnel: SteeringFunnel }) {
  const cells: { key: keyof SteeringFunnel; label: string; mark?: string; tone?: string }[] = [
    { key: 'observed', label: 'Observed' },
    { key: 'dropped_stage0', label: 'Stage 0', mark: '✕' },
    { key: 'dropped_cache', label: 'Cache', mark: '✕' },
    { key: 'dropped_stage1', label: 'Stage 1', mark: '✕' },
    { key: 'dropped_stage2', label: 'Stage 2', mark: '✕' },
    { key: 'surfaced', label: 'Surfaced', mark: '✓', tone: 'accent' },
    { key: 'errors', label: 'Errors', mark: '⚠', tone: 'warn' },
  ]
  return (
    <div className="funnel-strip row gap">
      {cells.map((c, i) => {
        const n = funnel[c.key]
        // Only emphasize the error chip when there's actually something to flag.
        const tone = c.tone === 'warn' ? (n > 0 ? 'warn' : '') : c.tone ?? ''
        const icon =
          c.key === 'observed' ? (
            <Inbox size={12} />
          ) : c.key === 'errors' ? (
            <AlertTriangle size={12} />
          ) : c.key === 'surfaced' ? (
            <Check size={12} />
          ) : (
            <Filter size={12} />
          )
        return (
          <div key={c.key} className={`funnel-cell card${tone ? ` funnel-${tone}` : ''}`}>
            <div className="funnel-top row">
              <span className="funnel-icon faint">{icon}</span>
              <span className="num funnel-count">{n}</span>
            </div>
            <div className="funnel-label faint">
              {c.mark ? <span className="funnel-mark">{c.mark} </span> : null}
              {c.label}
            </div>
            {i < cells.length - 1 ? <span className="funnel-arrow faint">→</span> : null}
          </div>
        )
      })}
    </div>
  )
}

const DISPOSITION_TONE: Record<string, string> = {
  surfaced: 'badge accent',
  dropped: 'badge',
  error: 'badge warn',
}

function TraceRow({ item, onOpen }: { item: SteeringTrace; onOpen: () => void }) {
  const conf =
    item.final_confidence ?? item.stage2_confidence ?? item.stage3_confidence ?? undefined
  const detail =
    item.disposition === 'error'
      ? item.error
      : item.drop_reason || item.text || item.text_preview || ''
  const dispClass = DISPOSITION_TONE[item.disposition] ?? 'badge'
  const dimDetail = item.disposition === 'dropped' && !item.drop_reason
  // Prefer the resolved channel name; never show a raw ID when a name exists.
  const where = item.channel_name || item.channel || item.channel_type || '—'
  const from = item.author_name || '(system/bot)'
  return (
    <div
      className={`trace-row trace-clickable trace-${item.disposition}`}
      role="button"
      tabIndex={0}
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          onOpen()
        }
      }}
    >
      <span className="mono faint trace-time">{dateTimeSec(item.created_at)}</span>
      <span className="row gap trace-src">
        <span className="badge">{item.origin}</span>
        <span className="badge">{item.source}</span>
      </span>
      <span>
        <span className={dispClass}>{item.disposition}</span>
      </span>
      <span className="mono dim trace-stage">{item.stage_reached || '—'}</span>
      <span className="num faint trace-conf">
        {conf != null ? `${Math.round(conf * 100)}%` : ''}
      </span>
      <span className="trace-channel" title={item.channel_name || item.channel || ''}>
        {where}
      </span>
      <span className="dim trace-from" title={from}>
        {from}
      </span>
      <span className={`trace-detail ${dimDetail ? 'faint' : 'dim'}`} title={detail}>
        {detail || <span className="faint">—</span>}
      </span>
    </div>
  )
}

// ----- Trace detail modal -------------------------------------------------
// One row of the cascade key/value list. Reuses the .meta-grid cell styling so
// the modal matches the session/project detail look.
function KV({ k, v }: { k: string; v: ReactNode }) {
  return (
    <div className="meta-cell">
      <div className="meta-k">{k}</div>
      <div className="meta-v">{v ?? '—'}</div>
    </div>
  )
}

function pctConf(c: number | null | undefined): string {
  return c != null ? `${Math.round(c * 100)}%` : '—'
}

function relevantLabel(b: boolean | null | undefined): string {
  if (b === true) return 'yes'
  if (b === false) return 'no'
  return '—'
}

function TraceDetail({ item, onClose }: { item: SteeringTrace | null; onClose: () => void }) {
  const [, navigate] = useLocation()
  // Keep the last item around so content doesn't blank during the close anim.
  if (!item) {
    return (
      <Modal open={false} onClose={onClose} title="">
        {null}
      </Modal>
    )
  }

  const sourceLabel = item.source === 'github' ? 'GitHub' : titleCase(item.source || 'message')
  const title = `${sourceLabel} · ${item.disposition}`
  const channel = item.channel_name || item.channel || '—'
  const from = item.author_name || '(system/bot — no user)'
  const message = item.text || item.text_preview || '(no text)'
  const linkLabel = item.source === 'github' ? 'Open in GitHub' : 'Open in Slack'
  const targetTask = item.matched_task
  const targetSlug = item.linked_task || targetTask?.slug || ''
  const targetLabel = item.autonomy_action === 'forward' || item.final_action === 'forward' ? 'forwarded task' : 'task'

  return (
    <Modal open onClose={onClose} title={title} width={620}>
      <div className="trace-detail-view">
        <div className="meta-grid">
          <KV k="when" v={dateTimeFull(item.created_at)} />
          <KV
            k="source"
            v={
              <span className="row gap">
                <span className="badge">{item.origin}</span>
                <span className="badge">{item.source || '—'}</span>
              </span>
            }
          />
          <KV k="channel" v={channel} />
          <KV k="from" v={from} />
        </div>

        <div className="td-section">
          <div className="eyebrow">message</div>
          <div className="att-draft-body td-message">{message}</div>
        </div>

        <div className="td-section">
          <div className="eyebrow">thread</div>
          <div className="row gap" style={{ marginTop: 5 }}>
            <span className="mono dim td-thread" title={item.thread_key || ''}>
              {item.thread_key || '—'}
            </span>
            {item.permalink ? (
              <a className="btn ghost sm" href={item.permalink} target="_blank" rel="noreferrer">
                <ExternalLink size={13} /> {linkLabel}
              </a>
            ) : null}
          </div>
        </div>

        <div className="td-section">
          <div className="eyebrow">why · cascade decision</div>
          <div className="meta-grid" style={{ marginTop: 6 }}>
            <KV k="disposition" v={<span className={DISPOSITION_TONE[item.disposition] ?? 'badge'}>{item.disposition}</span>} />
            <KV k="stage reached" v={item.stage_reached || '—'} />
            <KV k="stage 1 relevant" v={relevantLabel(item.stage1_relevant)} />
            <KV k="stage 1 reason" v={item.stage1_reason || '—'} />
            <KV k="stage 2 action" v={item.stage2_action ? `${item.stage2_action} · ${pctConf(item.stage2_confidence)}` : '—'} />
            <KV k="stage 3 action" v={item.stage3_action ? `${item.stage3_action} · ${pctConf(item.stage3_confidence)}` : '—'} />
            <KV k="final action" v={item.final_action ? `${item.final_action} · ${pctConf(item.final_confidence)}` : '—'} />
            <KV k="autonomy" v={item.autonomy_decision ? `${item.autonomy_action || item.final_action || 'action'} · ${item.autonomy_decision}` : '—'} />
            {targetSlug ? (
              <KV
                k={targetLabel}
                v={
                  <span className="row gap">
                    <span>
                      <strong>{targetTask?.name || targetSlug}</strong>
                      {targetTask?.name ? <span className="mono faint"> · {targetSlug}</span> : null}
                    </span>
                    <button type="button" className="btn ghost sm" onClick={() => navigate(`/session/${targetSlug}`)}>
                      <ArrowRight size={13} /> Open
                    </button>
                  </span>
                }
              />
            ) : null}
            <KV k="autonomy reason" v={item.autonomy_reason || '—'} />
            <KV k="drop reason" v={item.drop_reason || '—'} />
            <KV k="latency" v={item.latency_ms != null ? `${item.latency_ms} ms` : '—'} />
            <KV k="model" v={item.model || '—'} />
            {item.error ? <KV k="error" v={<span className="dim">{item.error}</span>} /> : null}
          </div>
        </div>

        {item.feed_item_id ? (
          <div className="td-section">
            <div className="dim">
              <Check size={13} /> Surfaced to the Attention feed.{' '}
              {targetSlug ? (
                <>
                  Linked to{' '}
                  <button type="button" className="btn ghost sm" onClick={() => navigate(`/session/${targetSlug}`)}>
                    <ArrowRight size={13} /> {targetTask?.name || targetSlug}
                  </button>{' '}
                </>
              ) : null}
              <span className="faint">Find it under the Feed tab.</span>
            </div>
          </div>
        ) : null}

        <div className="td-tech faint mono">
          channel={item.channel || '—'} · author={item.author || '—'} · thread={item.thread_key || '—'}
          {item.ts ? ` · ts=${item.ts}` : ''}
        </div>
      </div>
    </Modal>
  )
}
