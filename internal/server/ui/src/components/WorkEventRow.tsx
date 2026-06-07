import { ArrowRight, ExternalLink } from 'lucide-react'
import type { WorkEvent, WorkEventBucket, WorkEventLink } from '../lib/types'
import { workEventLinkHref } from '../lib/workEventLinks'

const BUCKET_LABEL: Record<WorkEventBucket, string> = {
  needs_action: 'needs action',
  closeout: 'closeout',
  waiting: 'waiting',
  next_up: 'next up',
  fyi: 'FYI',
  handled: 'handled',
  ignored: 'ignored',
}

const BUCKET_TONE: Record<WorkEventBucket, string> = {
  needs_action: 'warn',
  closeout: 'accent',
  waiting: 'warn',
  next_up: 'ok',
  fyi: '',
  handled: 'ok',
  ignored: '',
}

const LINK_LABEL: Record<string, string> = {
  attention: 'Attention',
  project: 'Project',
  source: 'Source',
  task: 'Task',
  trace: 'Trace',
}

export function WorkEventRow({
  event,
  compact = false,
  onOpen,
}: {
  event?: WorkEvent | null
  compact?: boolean
  onOpen?: (href: string) => void
}) {
  if (!event) return null
  const reason = event.reason_text || event.summary || event.title
  const links = (event.links ?? [])
    .map((link) => ({ link, href: workEventLinkHref(link) }))
    .filter((item) => item.href)

  return (
    <div className={`workevent-row ${event.bucket}${compact ? ' compact' : ''}`}>
      <span className={`badge ${BUCKET_TONE[event.bucket]}`}>{BUCKET_LABEL[event.bucket] ?? event.bucket}</span>
      {event.urgency ? <span className="workevent-urgency mono">{event.urgency}</span> : null}
      <span className="workevent-reason clip" title={reason}>{reason}</span>
      {!compact && links.length > 0 ? (
        <div className="workevent-links">
          {links.slice(0, 3).map(({ link, href }) => (
            <WorkEventLinkButton key={`${link.kind}:${link.target}`} link={link} href={href!} onOpen={onOpen} />
          ))}
        </div>
      ) : null}
    </div>
  )
}

function WorkEventLinkButton({
  link,
  href,
  onOpen,
}: {
  link: WorkEventLink
  href: string
  onOpen?: (href: string) => void
}) {
  const label = link.label || LINK_LABEL[link.kind] || link.kind
  const external = /^https?:\/\//i.test(href)
  if (external) {
    return (
      <a className="workevent-link" href={href} target="_blank" rel="noreferrer" onClick={(e) => e.stopPropagation()}>
        <ExternalLink size={12} /> {label}
      </a>
    )
  }
  return (
    <button
      type="button"
      className="workevent-link"
      onClick={(e) => {
        e.stopPropagation()
        onOpen?.(href)
      }}
    >
      <ArrowRight size={12} /> {label}
    </button>
  )
}

export function strongerWorkEvent(current: WorkEvent | undefined, next: WorkEvent): WorkEvent {
  if (!current) return next
  const currentRank = workEventBucketRank(current.bucket)
  const nextRank = workEventBucketRank(next.bucket)
  if (nextRank < currentRank) return next
  if (nextRank > currentRank) return current
  const currentTime = safeEventTime(current)
  const nextTime = safeEventTime(next)
  return nextTime > currentTime ? next : current
}

function safeEventTime(event: WorkEvent): number {
  const parsed = Date.parse(event.observed_at || event.occurred_at || '')
  return Number.isFinite(parsed) ? parsed : 0
}

function workEventBucketRank(bucket: WorkEventBucket): number {
  switch (bucket) {
    case 'needs_action':
      return 0
    case 'closeout':
      return 1
    case 'waiting':
      return 2
    case 'next_up':
      return 3
    case 'fyi':
      return 4
    case 'handled':
      return 5
    case 'ignored':
      return 6
    default:
      return 7
  }
}
