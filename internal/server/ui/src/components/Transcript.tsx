import { memo, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
  AlertTriangle,
  ArrowDown,
  Brain,
  Check,
  ChevronRight,
  Eye,
  FilePlus,
  FolderTree,
  Globe,
  ListTodo,
  Loader2,
  Pencil,
  Search,
  Sparkles,
  TerminalSquare,
  Wrench,
} from 'lucide-react'
import type { TranscriptEntry } from '../lib/types'
import { collapseContext, countChanges, diffLines } from '../lib/linediff'
import { Md } from './Markdown'

// ---------------------------------------------------------------------------
// Grouping: the JSONL feed is a flat list where a tool_use and its tool_result
// are separate entries (the result arrives in the next user record). The chat
// view pairs them into a single card, keyed by tool_use_id when present and
// falling back to "next unconsumed result" for older transcripts that predate
// the id being carried through.
// ---------------------------------------------------------------------------

type ChatItem =
  | { kind: 'message'; role: 'user' | 'assistant'; text: string; time?: string; key: string }
  | { kind: 'thinking'; text: string; time?: string; key: string }
  | { kind: 'tool'; call: TranscriptEntry; result?: TranscriptEntry; time?: string; key: string }
  | { kind: 'result'; result: TranscriptEntry; time?: string; key: string }

function buildItems(entries: TranscriptEntry[]): ChatItem[] {
  const consumed = new Set<number>()
  // Index tool_result entries by tool_use_id for O(1) pairing.
  const resultById = new Map<string, number>()
  entries.forEach((e, i) => {
    if (e.type === 'tool_result' && e.tool_use_id) resultById.set(e.tool_use_id, i)
  })

  const items: ChatItem[] = []
  for (let i = 0; i < entries.length; i++) {
    if (consumed.has(i)) continue
    const e = entries[i]
    const key = `${e.byte_offset}-${i}`
    switch (e.type) {
      case 'user':
      case 'assistant':
        if ((e.text || '').trim()) items.push({ kind: 'message', role: e.type, text: e.text || '', time: e.timestamp, key })
        break
      case 'thinking':
        if ((e.text || '').trim()) items.push({ kind: 'thinking', text: e.text || '', time: e.timestamp, key })
        break
      case 'tool_use': {
        let resultIdx = -1
        if (e.tool_use_id && resultById.has(e.tool_use_id)) {
          resultIdx = resultById.get(e.tool_use_id)!
        } else {
          // Fallback: nearest following unconsumed tool_result.
          for (let j = i + 1; j < entries.length; j++) {
            if (consumed.has(j)) continue
            if (entries[j].type === 'tool_result') {
              resultIdx = j
              break
            }
            if (entries[j].type === 'tool_use') break // don't reach past the next call
          }
        }
        if (resultIdx >= 0) consumed.add(resultIdx)
        items.push({ kind: 'tool', call: e, result: resultIdx >= 0 ? entries[resultIdx] : undefined, time: e.timestamp, key })
        break
      }
      case 'tool_result':
        // Orphan result (its call scrolled past the cache window).
        items.push({ kind: 'result', result: e, time: e.timestamp, key })
        break
    }
  }
  return items
}

// ---------------------------------------------------------------------------
// Tool classification — icon + how the body renders.
// ---------------------------------------------------------------------------

type ToolKind = 'edit' | 'write' | 'multiedit' | 'bash' | 'read' | 'search' | 'web' | 'todo' | 'task' | 'generic'

function classifyTool(name: string): { kind: ToolKind; Icon: typeof Wrench; accent: string } {
  const n = (name || '').toLowerCase()
  if (n === 'edit') return { kind: 'edit', Icon: Pencil, accent: 'var(--accent)' }
  if (n === 'multiedit') return { kind: 'multiedit', Icon: Pencil, accent: 'var(--accent)' }
  if (n === 'write') return { kind: 'write', Icon: FilePlus, accent: 'var(--ok)' }
  if (n === 'bash' || n === 'local_shell' || n.includes('shell')) return { kind: 'bash', Icon: TerminalSquare, accent: 'var(--warn)' }
  if (n === 'read' || n === 'notebookread') return { kind: 'read', Icon: Eye, accent: 'var(--info)' }
  if (n === 'grep' || n === 'glob' || n === 'ls') return { kind: 'search', Icon: n === 'grep' ? Search : FolderTree, accent: 'var(--info)' }
  if (n.includes('web') || n.includes('fetch')) return { kind: 'web', Icon: Globe, accent: 'var(--info)' }
  if (n === 'todowrite') return { kind: 'todo', Icon: ListTodo, accent: 'var(--text-2)' }
  if (n === 'task' || n.includes('agent')) return { kind: 'task', Icon: Sparkles, accent: 'var(--accent-2)' }
  return { kind: 'generic', Icon: Wrench, accent: 'var(--text-2)' }
}

function parseInput(raw?: string): Record<string, unknown> | null {
  if (!raw) return null
  try {
    const v = JSON.parse(raw)
    return v && typeof v === 'object' ? (v as Record<string, unknown>) : null
  } catch {
    return null
  }
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : v == null ? '' : JSON.stringify(v)
}

function lastPathSeg(p: string): string {
  const parts = p.split('/').filter(Boolean)
  return parts.length <= 2 ? p : '…/' + parts.slice(-2).join('/')
}

// "target" shown in a tool header — the one thing you scan for.
function toolTarget(kind: ToolKind, input: Record<string, unknown> | null, summary?: string): string {
  if (!input) return summary || ''
  switch (kind) {
    case 'edit':
    case 'multiedit':
    case 'write':
    case 'read':
      return lastPathSeg(str(input.file_path || input.path || input.notebook_path))
    case 'bash':
      return str(input.command || (Array.isArray(input.command) ? (input.command as string[]).join(' ') : '')) || summary || ''
    case 'search':
      return str(input.pattern || input.path || input.query)
    case 'web':
      return str(input.url || input.query)
    case 'task':
      return str(input.description || input.subagent_type)
    default:
      return summary || ''
  }
}

// ---------------------------------------------------------------------------
// Diff rendering (Edit / MultiEdit / Write)
// ---------------------------------------------------------------------------

function DiffBlock({ oldStr, newStr }: { oldStr: string; newStr: string }) {
  const rows = useMemo(() => collapseContext(diffLines(oldStr, newStr)), [oldStr, newStr])
  return (
    <div className="tx2-diff">
      {rows.map((r, i) =>
        r.type === 'gap' ? (
          <div key={i} className="tx2-diff-gap">⋯ {r.count} unchanged line{r.count === 1 ? '' : 's'}</div>
        ) : (
          <div key={i} className={`tx2-diff-row ${r.type}`}>
            <span className="tx2-diff-gutter">{r.type === 'add' ? '+' : r.type === 'del' ? '−' : ''}</span>
            <span className="tx2-diff-text">{r.text || ' '}</span>
          </div>
        ),
      )}
    </div>
  )
}

function diffMeta(rows: { add: number; del: number }): ReactNode {
  return (
    <span className="tx2-diffmeta">
      {rows.add > 0 && <span className="add">+{rows.add}</span>}
      {rows.del > 0 && <span className="del">−{rows.del}</span>}
    </span>
  )
}

// ---------------------------------------------------------------------------
// Tool body — switches on tool kind.
// ---------------------------------------------------------------------------

function ToolBody({ kind, input, call, result }: { kind: ToolKind; input: Record<string, unknown> | null; call: TranscriptEntry; result?: TranscriptEntry }) {
  const resultText = (result?.tool_result_text || '').trim()
  const resultErr = result?.is_error

  if (kind === 'edit' && input) {
    return (
      <>
        <DiffBlock oldStr={str(input.old_string)} newStr={str(input.new_string)} />
        {resultErr && <ResultPre text={resultText} error />}
      </>
    )
  }
  if (kind === 'multiedit' && input && Array.isArray(input.edits)) {
    return (
      <>
        {(input.edits as Record<string, unknown>[]).map((ed, i) => (
          <DiffBlock key={i} oldStr={str(ed.old_string)} newStr={str(ed.new_string)} />
        ))}
        {resultErr && <ResultPre text={resultText} error />}
      </>
    )
  }
  if (kind === 'write' && input) {
    return (
      <>
        <DiffBlock oldStr="" newStr={str(input.content)} />
        {resultErr && <ResultPre text={resultText} error />}
      </>
    )
  }
  if (kind === 'bash') {
    const cmd = input ? str(input.command) : call.tool_input_summary?.replace(/^\$ /, '') || ''
    return (
      <div className="tx2-term">
        {cmd && <div className="tx2-term-cmd">$ {cmd}</div>}
        {resultText && <pre className={`tx2-term-out ${resultErr ? 'error' : ''}`}>{resultText}</pre>}
      </div>
    )
  }
  if (kind === 'todo' && input && Array.isArray(input.todos)) {
    return (
      <ul className="tx2-todos">
        {(input.todos as Record<string, unknown>[]).map((t, i) => (
          <li key={i} className={`st-${str(t.status)}`}>
            <span className="tx2-todo-box">{str(t.status) === 'completed' ? '✓' : str(t.status) === 'in_progress' ? '◐' : '○'}</span>
            {str(t.content || t.activeForm)}
          </li>
        ))}
      </ul>
    )
  }
  // read / search / web / task / generic — show input (if not captured by the
  // header target) then the result peek.
  return (
    <>
      {input && (kind === 'task' || kind === 'generic') && <ResultPre text={JSON.stringify(input, null, 2)} mono />}
      {resultText ? <ResultPre text={resultText} error={resultErr} /> : !input && <div className="tx2-empty">no output</div>}
    </>
  )
}

function ResultPre({ text, error, mono }: { text: string; error?: boolean; mono?: boolean }) {
  const MAX = 4000
  const [expanded, setExpanded] = useState(false)
  const long = text.length > MAX
  const shown = long && !expanded ? text.slice(0, MAX) : text
  return (
    <div className="tx2-resultwrap">
      <pre className={`tx2-result ${error ? 'error' : ''} ${mono ? 'mono' : ''}`}>{shown}</pre>
      {long && (
        <button className="tx2-more" onClick={() => setExpanded((v) => !v)}>
          {expanded ? 'Show less' : `Show ${(text.length - MAX).toLocaleString()} more chars`}
        </button>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Tool card
// ---------------------------------------------------------------------------

function ToolCard({ call, result, live }: { call: TranscriptEntry; result?: TranscriptEntry; live?: boolean }) {
  const { kind, Icon, accent } = classifyTool(call.tool_name || '')
  const input = useMemo(() => parseInput(call.tool_input), [call.tool_input])
  const target = toolTarget(kind, input, call.tool_input_summary)

  // Default-open the kinds where the body IS the point (diffs); collapse noisy
  // reads/searches/bash so the feed stays scannable.
  const defaultOpen = kind === 'edit' || kind === 'multiedit' || kind === 'write'
  const [open, setOpen] = useState(defaultOpen)

  const pending = !result && live
  const error = result?.is_error

  // Header meta: diff counts for edits, exit/line hints for others.
  let meta: ReactNode = null
  if ((kind === 'edit' || kind === 'write') && input) {
    const o = kind === 'write' ? '' : str(input.old_string)
    const nw = kind === 'write' ? str(input.content) : str(input.new_string)
    meta = diffMeta(countChanges(diffLines(o, nw)))
  } else if (kind === 'multiedit' && input && Array.isArray(input.edits)) {
    let add = 0
    let del = 0
    for (const ed of input.edits as Record<string, unknown>[]) {
      const c = countChanges(diffLines(str(ed.old_string), str(ed.new_string)))
      add += c.add
      del += c.del
    }
    meta = diffMeta({ add, del })
  } else if (result?.tool_result_text) {
    const lines = result.tool_result_text.split('\n').length
    if (lines > 1) meta = <span className="tx2-linemeta">{lines} lines</span>
  }

  const hasBody = open

  return (
    <div className={`tx2-tool ${error ? 'is-error' : ''}`}>
      <button className="tx2-tool-head" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
        <ChevronRight size={13} className={`tx2-tool-chev ${open ? 'open' : ''}`} />
        <span className="tx2-tool-icon" style={{ color: accent }}>
          <Icon size={14} />
        </span>
        <span className="tx2-tool-name">{call.tool_name || 'tool'}</span>
        {target && <span className="tx2-tool-target" title={target}>{target}</span>}
        <span className="tx2-tool-spacer" />
        {meta}
        <StatusDot pending={pending} error={error} />
      </button>
      {hasBody && (
        <div className="tx2-tool-body">
          <ToolBody kind={kind} input={input} call={call} result={result} />
        </div>
      )}
    </div>
  )
}

function StatusDot({ pending, error }: { pending?: boolean; error?: boolean }) {
  if (pending) return <Loader2 size={13} className="tx2-spin" />
  if (error) return <AlertTriangle size={13} style={{ color: 'var(--danger)' }} />
  return <Check size={13} style={{ color: 'var(--ok)' }} />
}

// ---------------------------------------------------------------------------
// Thinking disclosure
// ---------------------------------------------------------------------------

function ThinkingCard({ text, defaultOpen }: { text: string; defaultOpen?: boolean }) {
  const [open, setOpen] = useState(!!defaultOpen)
  const preview = text.replace(/\s+/g, ' ').trim().slice(0, 90)
  return (
    <div className="tx2-think">
      <button className="tx2-think-head" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
        <Brain size={13} />
        <span className="tx2-think-label">Thinking</span>
        {!open && <span className="tx2-think-preview">{preview}{text.length > 90 ? '…' : ''}</span>}
        <span className="tx2-tool-spacer" />
        <ChevronRight size={13} className={`tx2-tool-chev ${open ? 'open' : ''}`} />
      </button>
      {open && (
        <div className="tx2-think-body">
          <Md source={text} className="tx-md" />
        </div>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Message rows
// ---------------------------------------------------------------------------

function AssistantRow({ text, agentName }: { text: string; agentName?: string }) {
  return (
    <div className="tx2-msg assistant">
      <div className="tx2-avatar">
        <Sparkles size={13} />
      </div>
      <div className="tx2-msg-body">
        <div className="tx2-msg-who">{agentName || 'Agent'}</div>
        <Md source={text} className="tx-md" />
      </div>
    </div>
  )
}

function UserRow({ text }: { text: string }) {
  return (
    <div className="tx2-msg user">
      <div className="tx2-user-card">
        <div className="tx2-msg-who">User</div>
        <Md source={text} className="tx-md" />
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Top-level transcript
// ---------------------------------------------------------------------------

export const Transcript = memo(function Transcript({
  entries,
  live,
  agentName,
  scrollSelf = true,
}: {
  entries: TranscriptEntry[]
  live?: boolean
  agentName?: string
  scrollSelf?: boolean
}) {
  const items = useMemo(() => buildItems(entries), [entries])
  const scrollRef = useRef<HTMLDivElement>(null)
  const stickRef = useRef(true)
  const [showJump, setShowJump] = useState(false)

  // Index of the most recent thinking item — auto-expanded while the session
  // is live so you can watch the reasoning stream.
  const lastThinkingKey = useMemo(() => {
    for (let i = items.length - 1; i >= 0; i--) if (items[i].kind === 'thinking') return items[i].key
    return null
  }, [items])

  // A cheap signature that also changes when the tail message grows (streaming
  // text), not just when the item count changes.
  const tailSig = useMemo(() => {
    const last = items[items.length - 1]
    const lastLen = last ? ('text' in last ? last.text.length : last.kind === 'tool' ? (last.result?.tool_result_text?.length ?? 0) : 0) : 0
    return `${items.length}:${lastLen}`
  }, [items])

  const scrollToBottom = () => {
    const el = scrollRef.current
    if (!el) return
    el.scrollTop = el.scrollHeight
    stickRef.current = true
    setShowJump(false)
  }

  const onScroll = () => {
    const el = scrollRef.current
    if (!el) return
    const dist = el.scrollHeight - el.scrollTop - el.clientHeight
    const atBottom = dist < 80
    stickRef.current = atBottom
    setShowJump(!atBottom)
  }

  // Mount: jump straight to the latest turn.
  useEffect(() => {
    if (scrollSelf) scrollToBottom()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // New content: stay pinned to the bottom only if the user was already there.
  useLayoutEffect(() => {
    if (scrollSelf && stickRef.current) scrollToBottom()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tailSig])

  if (items.length === 0) return <div className="faint">No transcript captured yet.</div>

  const feed = (
    <div className="tx2-feed">
      {items.map((it) => {
        switch (it.kind) {
          case 'message':
            return it.role === 'assistant' ? (
              <AssistantRow key={it.key} text={it.text} agentName={agentName} />
            ) : (
              <UserRow key={it.key} text={it.text} />
            )
          case 'thinking':
            return <ThinkingCard key={it.key} text={it.text} defaultOpen={!!live && it.key === lastThinkingKey} />
          case 'tool':
            return <ToolCard key={it.key} call={it.call} result={it.result} live={live} />
          case 'result':
            return (
              <div key={it.key} className="tx2-tool">
                <div className="tx2-tool-body">
                  <ResultPre text={(it.result.tool_result_text || '').trim()} error={it.result.is_error} />
                </div>
              </div>
            )
        }
      })}
      {live && (
        <div className="tx2-working">
          <Loader2 size={13} className="tx2-spin" />
          <span>{agentName || 'Agent'} is working…</span>
        </div>
      )}
    </div>
  )

  if (!scrollSelf) return feed

  return (
    <div className="tx2">
      <div className="tx2-scroll" ref={scrollRef} onScroll={onScroll}>
        {feed}
      </div>
      {showJump && (
        <button className="tx2-jump" onClick={scrollToBottom}>
          <ArrowDown size={13} /> Jump to latest
        </button>
      )}
    </div>
  )
})
