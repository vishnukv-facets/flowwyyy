import { useState } from 'react'
import { Archive, ArchiveRestore, MessagesSquare, Play, Trash2 } from 'lucide-react'
import { useAction, useChats } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { useFloatingTerminals } from '../lib/floatingTerminals'
import { EmptyState, ErrorNote, Loading, ProviderIcon, SourceIcon } from '../components/ui'
import { ago, PROVIDER_LABEL } from '../lib/format'
import { confirmAction } from '../lib/confirm'
import type { Chat } from '../lib/types'

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
  const { open: openFloatingTerminal } = useFloatingTerminals()
  const providerLabel = PROVIDER_LABEL[chat.provider] ?? chat.provider
  const isSlack = chat.origin === 'slack'

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

  const toggleArchive = () => {
    if (action.isPending) return
    action.mutate({ kind: chat.archived ? 'chat-unarchive' : 'chat-archive', slug: chat.slug })
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

  return (
    <div className="card row gap" style={{ alignItems: 'center', padding: '12px 14px' }}>
      <span
        className={`dot ${chat.live ? 'running' : 'idle'}`}
        title={chat.live ? 'working — agent is processing a command' : 'idle'}
      />
      <div className="col" style={{ gap: 4, minWidth: 0, flex: 1 }}>
        <div className="row gap" style={{ alignItems: 'center' }}>
          <strong className="clip">{chat.title || 'New chat'}</strong>
          {chat.live ? <span className="faint">working…</span> : null}
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
          <span className="mono" title={chat.last_activity_at}>{ago(chat.last_activity_at)}</span>
        </div>
      </div>
      <div className="row gap" style={{ alignItems: 'center' }}>
        <button type="button" className="btn sm" disabled={action.isPending} onClick={reopen}>
          <Play size={13} /> Reopen
        </button>
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
  )
}
