import { useState } from 'react'
import { SendHorizontal, Loader2 } from 'lucide-react'
import { useAction } from '../lib/query'

// A one-line composer to nudge a running agent without opening the terminal.
// Posts a `nudge` action, which injects the text into the session's PTY via the
// same path the inbox monitor uses to auto-inject on new messages. Paused
// sessions queue the text until resume. Stops click/key propagation so typing
// here never navigates the card.
export function NudgeComposer({ slug, compact = false }: { slug: string; compact?: boolean }) {
  const [text, setText] = useState('')
  const action = useAction()
  const send = () => {
    const t = text.trim()
    if (!t || action.isPending) return
    action.mutate({ kind: 'nudge', target: slug, prompt: t }, { onSuccess: () => setText('') })
  }
  return (
    <div
      className={`nudge${compact ? ' compact' : ''}`}
      onClick={(e) => e.stopPropagation()}
    >
      <input
        className="nudge-input"
        aria-label="Instruction"
        placeholder="Send an instruction…"
        value={text}
        disabled={action.isPending}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          e.stopPropagation()
          if (e.key === 'Enter') {
            e.preventDefault()
            send()
          }
        }}
      />
      <button
        type="button"
        className="nudge-send"
        disabled={!text.trim() || action.isPending}
        title="Send instruction to this session"
        aria-label="Send instruction"
        onClick={send}
      >
        {action.isPending ? <Loader2 size={14} className="spin" /> : <SendHorizontal size={14} />}
      </button>
    </div>
  )
}
