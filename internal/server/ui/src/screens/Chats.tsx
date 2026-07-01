import { useRef, useState } from 'react'
import { Archive, ArchiveRestore, ArrowLeftRight, Bell, BellOff, Check, ChevronDown, MessagesSquare, Pencil, Play, TerminalSquare, Trash2, X } from 'lucide-react'
import { useAction, useChats } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { useFloatingTerminals } from '../lib/floatingTerminals'
import { TaskTerminal } from '../components/Terminal'
import { EmptyState, ErrorNote, Loading, ProviderIcon, SourceIcon } from '../components/ui'
import { ago, compactTokens, fmtUSD, PROVIDER_LABEL } from '../lib/format'
import { confirmAction } from '../lib/confirm'
import type { ActionResponse, Chat } from '../lib/types'

// Chats — the adhoc Ask Flow / Slack chat sessions started outside the task
// flow. Each row reopens its session into a floating terminal, or
// archives/deletes it. Mirrors the Attention/Tasks card idiom (.page,
// .page-head, .card, .btn sm, .badge, .dot) — no bespoke design system.
export function Chats() {
  useDocumentTitle('Chats')
  const [includeArchived, setIncludeArchived] = useState(false)
  const { data, isLoading, error } = useChats(includeArchived)
  const chats = data ?? []

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="eyebrow">workspace</div>
          <h1 className="h-xl">Chats</h1>
        </div>
        <div className="spacer" />
        <button
          type="button"
          className={`btn sm ${includeArchived ? 'primary' : 'ghost'}`}
          onClick={() => setIncludeArchived((v) => !v)}
        >
          <Archive size={14} /> {includeArchived ? 'Hiding nothing' : 'Show archived'}
        </button>
      </div>

      {isLoading ? (
        <Loading label="loading chats" />
      ) : error ? (
        <ErrorNote error={error} />
      ) : chats.length === 0 ? (
        <EmptyState
          icon={<MessagesSquare size={28} />}
          title="No chats yet"
          hint="Start one from Ask Flow (the ✨ in the topbar) or DM the flow Slack bot."
        />
      ) : (
        <div className="col" style={{ gap: 10 }}>
          {chats.map((c) => (
            <ChatRow key={c.slug} chat={c} />
          ))}
        </div>
      )}
    </div>
  )
}

function ChatRow({ chat }: { chat: Chat }) {
  const action = useAction()
  const { open: openFloatingTerminal, minimize: minimizeFloatingTerminal } = useFloatingTerminals()
  const providerLabel = PROVIDER_LABEL[chat.provider] ?? chat.provider
  const isSlack = chat.origin === 'slack'
  const isSteerer = chat.origin === 'steerer'
  const src = chatSource(chat)
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const renameInputRef = useRef<HTMLInputElement>(null)
  const [inlineTerminal, setInlineTerminal] = useState<ActionResponse['floating_terminal'] | null>(null)
  const [inlineStatus, setInlineStatus] = useState('disconnected')
  const inlineOpen = !!inlineTerminal

  const saveRename = () => {
    const name = draft.trim()
    if (action.isPending || !name || name === chat.title) {
      setEditing(false)
      return
    }
    action.mutate({ kind: 'chat-rename', slug: chat.slug, name }, { onSuccess: () => setEditing(false) })
  }

  // Manual provider switch (GAP-11) — tears down + re-primes the session on the
  // other agent. Only meaningful for steerer chats; either direction.
  const switchProvider = async () => {
    if (action.isPending) return
    const target = chat.provider === 'codex' ? 'claude' : 'codex'
    const ok = await confirmAction({
      title: `Switch to ${PROVIDER_LABEL[target] ?? target}?`,
      body: `This restarts "${chat.title}" on ${PROVIDER_LABEL[target] ?? target}, re-primed from the current transcript.`,
      confirmLabel: 'Switch',
    })
    if (!ok) return
    action.mutate({ kind: 'chat-set-provider', slug: chat.slug, provider: target })
  }

  const reopen = () => {
    if (action.isPending) return
    action.mutate(
      { kind: 'chat-reopen', slug: chat.slug },
      {
        onSuccess: (resp) => {
          if (resp.floating_terminal) openFloatingTerminal(resp.floating_terminal)
        },
      },
    )
  }

  const toggleInlineTerminal = () => {
    if (inlineOpen) {
      setInlineTerminal(null)
      return
    }
    if (action.isPending) return
    action.mutate(
      { kind: 'chat-reopen', slug: chat.slug },
      {
        onSuccess: (resp) => {
          if (!resp.floating_terminal) return
          setInlineStatus('connecting')
          setInlineTerminal(resp.floating_terminal)
          minimizeFloatingTerminal(resp.floating_terminal.id)
        },
      },
    )
  }

  const toggleArchive = () => {
    if (action.isPending) return
    action.mutate({ kind: chat.archived ? 'chat-unarchive' : 'chat-archive', slug: chat.slug })
  }

  const toggleMute = () => {
    if (action.isPending) return
    action.mutate({ kind: chat.muted ? 'chat-unmute' : 'chat-mute', slug: chat.slug })
  }

  const remove = async () => {
    if (action.isPending) return
    const ok = await confirmAction({
      title: 'Delete chat?',
      body: `"${chat.title}" will be removed and its session ended. This can't be undone.`,
      confirmLabel: 'Delete',
      danger: true,
    })
    if (!ok) return
    action.mutate({ kind: 'chat-delete', slug: chat.slug })
  }

  const beginRename = () => {
    setDraft(chat.title)
    setEditing(true)
    requestAnimationFrame(() => renameInputRef.current?.focus())
  }

  return (
    <div className="card chat-row">
      <div className="chat-row-main">
        <span
          className={`dot ${chat.live ? 'running' : 'idle'}`}
          title={chat.live ? 'working — agent is processing a command' : 'idle'}
        />
        <div className="col" style={{ gap: 4, minWidth: 0, flex: 1 }}>
          <div className="row gap" style={{ alignItems: 'center' }}>
            {editing ? (
              <input
                ref={renameInputRef}
                className="input sm"
                aria-label="Chat name"
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') saveRename()
                  if (e.key === 'Escape') setEditing(false)
                }}
                style={{ flex: 1, minWidth: 0 }}
              />
            ) : (
              <strong className="clip">{chat.title || 'New chat'}</strong>
            )}
            {!editing && src ? (
              <span
                className="chat-source"
                title={`${src.source === 'github' ? 'GitHub' : 'Slack'} · ${src.id}`}
              >
                <SourceIcon source={src.source} size={11} />
                {/* Skip the text when it would just repeat the chat name (channels,
                    where name === id) — keep it where it adds info (DMs/groups). */}
                {src.id !== chat.title.trim() ? <span className="chat-source-id">{src.id}</span> : null}
              </span>
            ) : null}
            {editing ? (
              <>
                <button type="button" className="btn icon ghost sm" title="Save name" aria-label="Save name" onClick={saveRename}>
                  <Check size={13} />
                </button>
                <button type="button" className="btn icon ghost sm" title="Cancel" aria-label="Cancel rename" onClick={() => setEditing(false)}>
                  <X size={13} />
                </button>
              </>
            ) : (
              <button
                type="button"
                className="btn icon ghost sm chat-rename"
                title="Rename chat"
                aria-label="Rename chat"
                onClick={beginRename}
              >
                <Pencil size={12} />
              </button>
            )}
            {chat.live ? <span className="faint">working…</span> : null}
            {chat.muted ? <span className="badge warn" title="Muted — no events are forwarded to this chat">muted</span> : null}
            {chat.archived ? <span className="badge">archived</span> : null}
          </div>
          {chat.last_reply ? (
            <div className="faint clip" style={{ fontSize: 12 }} title={chat.last_reply}>
              <span style={{ opacity: 0.6 }}>↳ </span>
              {chat.last_reply}
            </div>
          ) : null}
          <div className="row gap faint" style={{ alignItems: 'center', fontSize: 12 }}>
            <span className="row gap" style={{ alignItems: 'center', gap: 5 }}>
              <ProviderIcon provider={chat.provider} size={13} />
              {providerLabel}
            </span>
            {isSlack ? (
              <span className="badge accent" title="Started from Slack">
                <SourceIcon source="slack" size={11} /> Slack
              </span>
            ) : null}
            {isSteerer ? (
              <span className="badge" title="Always-on attention steerer session">Steering</span>
            ) : null}
            {chat.tokens ? (
              <span className="mono" title="Cumulative session tokens · estimated cost">
                {compactTokens(chat.tokens)} tok · ~{fmtUSD(chat.cost_usd ?? 0)}
              </span>
            ) : null}
            {chat.occupancy_pct ? <CtxDial pct={chat.occupancy_pct} /> : null}
            <span className="mono" title={chat.last_activity_at}>{ago(chat.last_activity_at)}</span>
          </div>
        </div>
        <div className="chat-actions">
          {/* Secondary actions roll out to the LEFT of Reopen on hover/focus (grid
              0fr→1fr) so Reopen stays pinned to the right corner. State that matters
              at rest (muted/archived) is shown as a badge by the title. */}
          <div className="chat-actions-rollout">
            <div className="row gap chat-actions-secondary" style={{ alignItems: 'center' }}>
              {isSteerer ? (
                <button
                  type="button"
                  className="btn ghost sm"
                  disabled={action.isPending}
                  title={`Switch to ${chat.provider === 'codex' ? 'Claude' : 'Codex'}`}
                  onClick={switchProvider}
                >
                  <ArrowLeftRight size={13} /> {chat.provider === 'codex' ? 'Claude' : 'Codex'}
                </button>
              ) : null}
              {isSteerer ? (
                <button
                  type="button"
                  className={`btn ghost sm${chat.muted ? ' primary' : ''}`}
                  disabled={action.isPending}
                  title={chat.muted ? 'Unmute — resume forwarding events to this chat' : 'Mute — stop forwarding events to this chat until unmuted'}
                  onClick={toggleMute}
                >
                  {chat.muted ? <Bell size={13} /> : <BellOff size={13} />}
                  {chat.muted ? 'Unmute' : 'Mute'}
                </button>
              ) : null}
              <button type="button" className="btn ghost sm" disabled={action.isPending} onClick={toggleArchive}>
                {chat.archived ? <ArchiveRestore size={13} /> : <Archive size={13} />}
                {chat.archived ? 'Unarchive' : 'Archive'}
              </button>
              <button
                type="button"
                className="btn icon ghost sm"
                title="Delete chat"
                aria-label="Delete chat"
                disabled={action.isPending}
                onClick={remove}
              >
                <Trash2 size={13} />
              </button>
            </div>
          </div>
          <div className="chat-open-group" aria-label="Open chat session">
            <button type="button" className="btn sm chat-reopen" disabled={action.isPending} onClick={reopen}>
              <Play size={13} /> Reopen
            </button>
            <button
              type="button"
              className={`btn icon sm chat-inline-toggle${inlineOpen ? ' active' : ''}`}
              title={inlineOpen ? 'Hide inline terminal' : 'Open inline terminal'}
              aria-label={inlineOpen ? 'Hide inline terminal' : 'Open inline terminal'}
              aria-expanded={inlineOpen}
              disabled={action.isPending && !inlineOpen}
              onClick={toggleInlineTerminal}
            >
              <ChevronDown size={14} />
            </button>
          </div>
        </div>
      </div>
      {inlineTerminal ? (
        <div className="chat-inline-terminal">
          <div className="chat-inline-head">
            <div className="chat-inline-title">
              <TerminalSquare size={14} />
              <span className="clip">{inlineTerminal.title || chat.title || chat.slug}</span>
            </div>
            <span className="provider-chip">
              <ProviderIcon provider={inlineTerminal.provider} size={13} />
              {PROVIDER_LABEL[inlineTerminal.provider] ?? inlineTerminal.provider}
            </span>
            <span className="mono faint">{inlineStatus}</span>
            <button
              type="button"
              className="btn icon ghost sm"
              title="Close inline terminal"
              aria-label="Close inline terminal"
              onClick={() => setInlineTerminal(null)}
            >
              <X size={14} />
            </button>
          </div>
          <div className="chat-inline-body">
            <TaskTerminal
              slug={inlineTerminal.id}
              kind="floating"
              provider={inlineTerminal.provider}
              onStatus={(kind, message) => setInlineStatus(kind === 'open' ? 'connected' : message || kind)}
            />
          </div>
        </div>
      ) : null}
    </div>
  )
}

// chatSource derives the connector + a compact identifier for a steering chat
// from its title. The steerer mints titles in fixed shapes (#channel / DM · Name
// / Group · Name for Slack, owner/repo#N for GitHub), so the source logo and the
// channel/username/group pill come straight off the title — no backend field.
// Returns null for non-steerer chats and for the unresolved "Steering: <key>"
// placeholder (nothing trustworthy to show yet).
// ponytail: title-pattern parse. If the steerer ever persists an explicit
// source + identifier on the chat row, read those instead of re-parsing here.
function chatSource(chat: Chat): { source: 'slack' | 'github'; id: string } | null {
  if (chat.origin !== 'steerer') return null
  const t = chat.title.trim()
  if (!t || t.startsWith('Steering:')) return null // unresolved placeholder
  if (t.startsWith('#')) return { source: 'slack', id: t }
  // "DM · Name" / "Group · Name" — tolerate either separator the backend has used
  // over time (middot · or a plain period), and any surrounding spacing.
  const dm = t.match(/^DM\s*[·.]\s*(.+)$/)
  if (dm) return { source: 'slack', id: dm[1].trim() }
  const grp = t.match(/^Group\s*[·.]\s*(.+)$/)
  if (grp) return { source: 'slack', id: grp[1].trim() }
  if (/^[^/\s]+\/[^/\s]+(#\d+)?$/.test(t)) return { source: 'github', id: t }
  return null
}

// CtxDial — a small radial gauge for a chat's context-window occupancy. The arc
// fills with pct and the stroke grades through the theme tokens: it starts on the
// brand accent (plenty of headroom), warms to --warn approaching the 60% /compact
// line, then to --danger past it — so one glance reads both "how full" and "how
// urgent". Built from theme vars via color-mix, so it tracks light/dark for free.
const CTX_DIAL_R = 7
const CTX_DIAL_CIRC = 2 * Math.PI * CTX_DIAL_R
const CTX_COMPACT_AT = 60 // the session /compacts at 60% — the grading pivot

function ctxDialColor(pct: number): string {
  if (pct < CTX_COMPACT_AT) {
    return `color-mix(in oklab, var(--accent), var(--warn) ${Math.round((pct / CTX_COMPACT_AT) * 100)}%)`
  }
  return `color-mix(in oklab, var(--warn), var(--danger) ${Math.round(((pct - CTX_COMPACT_AT) / (100 - CTX_COMPACT_AT)) * 100)}%)`
}

function CtxDial({ pct }: { pct: number }) {
  const clamped = Math.max(0, Math.min(100, pct))
  const dash = (clamped / 100) * CTX_DIAL_CIRC
  return (
    <span
      className="ctx-dial"
      title={`${Math.round(clamped)}% of the context window used — the session /compacts at ${CTX_COMPACT_AT}%`}
      style={{ ['--ctx-color' as string]: ctxDialColor(clamped) }}
    >
      <svg viewBox="0 0 18 18" width="18" height="18" aria-hidden="true">
        <circle className="ctx-dial-track" cx="9" cy="9" r={CTX_DIAL_R} />
        <circle className="ctx-dial-arc" cx="9" cy="9" r={CTX_DIAL_R} strokeDasharray={`${dash} ${CTX_DIAL_CIRC}`} />
      </svg>
      <span className="ctx-dial-label mono">{Math.round(clamped)}%</span>
    </span>
  )
}
