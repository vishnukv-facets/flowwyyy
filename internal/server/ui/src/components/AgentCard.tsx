import { useLocation } from 'wouter'
import type { MouseEvent } from 'react'
import { GitBranch, Clock3, Radar, Coins, AlertTriangle, ExternalLink, PictureInPicture2, GitFork, Loader2, RotateCcw } from 'lucide-react'
import type { UiAgent } from '../lib/types'
import { fromMinutes, fromSeconds, compact, cacheAwareTokens, fmtUSD, tokenUsageTitle } from '../lib/format'
import { ProviderIcon, Sparkline, StatusDot, TokenBar } from './ui'
import { NudgeComposer } from './NudgeComposer'
import { clickable } from '../lib/a11y'
import { useFloatingTerminals } from '../lib/floatingTerminals'
import { useAction } from '../lib/query'

const BADGE_TONE: Record<string, string> = {
  waiting: 'warn',
  running: 'ok',
  paused: 'warn',
  dead: 'danger',
  stale: 'danger',
  released: '',
}
const AUTO_TONE: Record<string, string> = { running: 'ok', completed: 'info', dead: 'danger' }
const AUTO_DOT: Record<string, string> = { running: 'running', completed: 'done', dead: 'stale' }
const STATUS_LABEL: Record<string, string> = {
  dead: 'crashed',
  stale: 'stalled',
  paused: 'paused',
}

export function AgentCard({
  agent,
  selectable = false,
  selected = false,
  onToggle,
}: {
  agent: UiAgent
  selectable?: boolean
  selected?: boolean
  onToggle?: () => void
}) {
  const [, navigate] = useLocation()
  const { popOut } = useFloatingTerminals()
  const action = useAction()
  const waiting = agent.status === 'waiting'
  // A finished task should read as "done", not as the residual runtime state
  // (its session is merely idle/released). task_status is the source of truth
  // for completion; the runtime status drives everything else.
  const isDone = agent.task_status === 'done'
  const badgeStatus = isDone ? 'done' : agent.status
  const canResume = !isDone && agent.status !== 'running' && agent.task_status === 'in-progress'
  const resume = (e: MouseEvent<HTMLButtonElement>) => {
    e.stopPropagation()
    if (!canResume || action.isPending) return
    action.mutate(
      { kind: 'resume', target: agent.slug },
      { onSuccess: () => popOut({ slug: agent.slug, provider: agent.provider, title: agent.name }) },
    )
  }
  return (
    <article
      className={`card acard${selected ? ' selected' : ''}`}
      aria-label={`Open session ${agent.name}`}
      {...clickable(() => navigate(`/session/${agent.slug}`))}
    >
      <div className="acard-top">
        {selectable && (
          <input
            type="checkbox"
            className="acard-check"
            checked={selected}
            aria-label={`Select ${agent.name}`}
            onClick={(e) => e.stopPropagation()}
            onChange={() => onToggle?.()}
          />
        )}
        <ProviderIcon provider={agent.provider} size={17} />
        <div style={{ flex: 1, minWidth: 0 }}>
          <div className="acard-title clip">{agent.name}</div>
          <div className="acard-ref clip">{agent.slug}</div>
        </div>
        {agent.hook_health && (
          <span
            className="acard-mon"
            style={{ color: 'var(--warn)' }}
            title={`${agent.hook_health.message}${agent.hook_health.action ? ' — ' + agent.hook_health.action : ''}`}
          >
            <AlertTriangle size={14} />
          </span>
        )}
        {agent.monitored && (
          <span className="acard-mon" title="Background monitor active">
            <Radar size={14} />
          </span>
        )}
        <span className={`badge ${isDone ? 'info' : BADGE_TONE[agent.status] ?? ''}`}>
          <StatusDot status={badgeStatus} />
          {isDone ? 'done' : STATUS_LABEL[agent.status] ?? agent.status}
        </span>
        {agent.auto_run_status && (
          <span
            className={`badge ${AUTO_TONE[agent.auto_run_status] ?? ''}`}
            title={agent.auto_run_status === 'dead' ? 'Autonomous run died — needs eyes' : 'Autonomous run'}
          >
            <StatusDot status={AUTO_DOT[agent.auto_run_status] ?? 'idle'} />
            auto: {agent.auto_run_status}
          </span>
        )}
        {canResume && (
          <button
            type="button"
            className="btn icon ghost sm acard-open"
            title="Resume session in a floating window"
            aria-label="Resume session in a floating window"
            disabled={action.isPending}
            onClick={resume}
          >
            {action.isPending ? <Loader2 size={13} className="spin" /> : <RotateCcw size={13} />}
          </button>
        )}
        <button
          type="button"
          className="btn icon ghost sm acard-open"
          title="Pop out as a floating window"
          aria-label="Pop out as a floating window"
          onClick={(e) => {
            e.stopPropagation()
            popOut({ slug: agent.slug, provider: agent.provider, title: agent.name })
          }}
        >
          <PictureInPicture2 size={13} />
        </button>
        <button
          type="button"
          className="btn icon ghost sm acard-open"
          title="Open session in a new tab"
          aria-label="Open session in a new tab"
          onClick={(e) => {
            e.stopPropagation()
            window.open(`/session/${agent.slug}`, '_blank', 'noopener,noreferrer')
          }}
        >
          <ExternalLink size={13} />
        </button>
      </div>

      {waiting && agent.waiting_for?.why && (
        <div className="badge warn" style={{ height: 'auto', padding: '5px 9px', whiteSpace: 'normal', textAlign: 'left' }}>
          {agent.waiting_for.why}
        </div>
      )}

      <div className="acard-summary">{agent.last_action || agent.summary}</div>

      <div className="acard-meta">
        {agent.project && <span className="tag">{agent.project}</span>}
        {agent.forked_from_slug && (
          <span className="tag fork-tag" title={`Forked from ${agent.forked_from?.name || agent.forked_from_slug}${agent.fork_reason ? ` · ${agent.fork_reason}` : ''}`}>
            <GitFork size={11} /> {agent.forked_from_slug}
          </span>
        )}
        {(agent.forks?.length ?? 0) > 0 && (
          <span className="tag fork-tag" title={`Forked into ${agent.forks?.map((f) => f.name).join(', ')}`}>
            <GitFork size={11} /> forks {agent.forks?.length}
          </span>
        )}
        <span className="row" style={{ gap: 5 }}>
          <GitBranch size={12} /> <span className="mono clip" style={{ maxWidth: 150 }}>{agent.branch}</span>
        </span>
        <span className="row" style={{ gap: 5 }}>
          <Clock3 size={12} /> {fromMinutes(agent.started_min)}
        </span>
        {agent.tokens_session > 0 && (
          <span
            className="tag tok-pill"
            title={tokenUsageTitle({
              tokens: agent.tokens_session,
              cacheReadTokens: agent.cache_read_tokens,
              cacheCreationTokens: agent.cache_creation_tokens,
              cost: agent.cost_session,
              costFresh: agent.cost_fresh,
              costCacheRead: agent.cost_cache_read,
              costCacheCreation: agent.cost_cache_creation,
              contextUsed: agent.tokens_used,
              contextMax: agent.tokens_max,
            })}
          >
            <Coins size={11} /> {cacheAwareTokens(agent.tokens_session, agent.cache_read_tokens)} tok
            {agent.cost_session ? <span className="tok-pill-cost"> · ~{fmtUSD(agent.cost_session)}</span> : null}
          </span>
        )}
      </div>

      <div className="acard-foot">
        <span className="acard-idle mono">
          {isDone
            ? `done ${fromSeconds(agent.last_activity_sec)} ago`
            : agent.status === 'running' && agent.last_activity_sec < 120
            ? 'active'
            : `idle ${fromSeconds(agent.last_activity_sec)}`}
        </span>
        <div className="spacer" />
        {(agent.diff.add > 0 || agent.diff.rem > 0) && (
          <span className="diffstat">
            <span className="add">+{agent.diff.add}</span>
            <span className="rem">−{agent.diff.rem}</span>
          </span>
        )}
      </div>

      <div className="acard-spark">
        <Sparkline data={agent.activity ?? []} flex />
      </div>

      <div className="row gap" style={{ gap: 9 }}>
        <TokenBar used={agent.tokens_used} max={agent.tokens_max} />
        <span className="faint mono" style={{ fontSize: 12 }}>
          {compact(agent.tokens_used)}/{compact(agent.tokens_max)}
        </span>
      </div>

      {!isDone && <NudgeComposer slug={agent.slug} compact />}
    </article>
  )
}
