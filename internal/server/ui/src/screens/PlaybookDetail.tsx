import { useLocation } from 'wouter'
import { ArrowLeft, Play } from 'lucide-react'
import { usePlaybook, useAction } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { BriefPanel } from '../components/BriefPanel'
import { ErrorNote, Loading } from '../components/ui'
import { useFloatTip } from '../components/FloatTip'
import { ago } from '../lib/format'

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']

// A readable 30-day run-history bar chart. run_days_30[i] is the run count for
// (today − (29 − i)) days, so index 29 is today. Hover shows the day + count.
function RunBars({ days }: { days: number[] }) {
  const { show, hide, portal } = useFloatTip()
  const data = days.length ? days : new Array(30).fill(0)
  const max = Math.max(1, ...data)
  const dateFor = (i: number) => {
    const d = new Date()
    d.setHours(0, 0, 0, 0)
    d.setDate(d.getDate() - (data.length - 1 - i))
    return d
  }
  return (
    <div className="actbars pb-runbars">
      {portal}
      {data.map((v, i) => {
        const d = dateFor(i)
        return (
          <i
            key={i}
            className={v > 0 ? 'on' : ''}
            style={{ height: `${Math.max(4, Math.round((v / max) * 100))}%` }}
            onMouseEnter={(e) =>
              show(
                e.currentTarget,
                <div className="ftip-head">
                  <span className="ftip-count">{v === 0 ? 'No runs' : `${v} run${v === 1 ? '' : 's'}`}</span>
                  <span className="ftip-date">{`${WEEKDAYS[d.getDay()]}, ${MONTHS[d.getMonth()]} ${d.getDate()}`}</span>
                </div>,
              )
            }
            onMouseLeave={hide}
          />
        )
      })}
    </div>
  )
}

export function PlaybookDetail({ slug }: { slug: string }) {
  const [, navigate] = useLocation()
  const { data: pb, isLoading, error } = usePlaybook(slug)
  const action = useAction()
  useDocumentTitle(pb?.name)

  if (isLoading) return <div className="page"><Loading /></div>
  if (error) return <div className="page"><ErrorNote error={error} /></div>
  if (!pb) return null

  const runPlaybook = () => {
    action.mutate(
      { kind: 'spawn-run', target: slug },
      { onSuccess: (data) => data.agent && navigate(`/session/${data.agent.slug}`) },
    )
  }

  return (
    <div className="page">
      <button className="btn ghost sm" style={{ marginBottom: 14 }} onClick={() => navigate('/playbooks')}>
        <ArrowLeft size={14} /> Playbooks
      </button>

      <div className="detail-head">
        <div style={{ flex: 1 }}>
          <div className="eyebrow">playbook</div>
          <div className="detail-title">{pb.name}</div>
          <div className="detail-ref">{pb.slug}{pb.project_slug ? ` · ${pb.project_slug}` : ''}</div>
        </div>
        <button className="btn primary" disabled={action.isPending} onClick={runPlaybook}>
          <Play size={15} /> Run playbook
        </button>
      </div>

      <div className="meta-grid" style={{ marginBottom: 16 }}>
        <div className="meta-cell"><div className="meta-k">working dir</div><div className="meta-v mono clip" title={pb.work_dir}>{pb.work_dir}</div></div>
        <div className="meta-cell"><div className="meta-k">runs · 7d</div><div className="meta-v num">{pb.run_count_7d}</div></div>
        <div className="meta-cell">
          <div className="meta-k">runs · 30d</div>
          <div className="meta-v num">{(pb.run_days_30 ?? []).reduce((a, b) => a + b, 0)}</div>
        </div>
      </div>

      <section className="card" style={{ padding: '16px 18px', marginBottom: 24 }}>
        <div className="bento-head" style={{ marginBottom: 12 }}>
          <span className="eyebrow">Run activity</span>
          <div className="spacer" />
          <span className="faint mono" style={{ fontSize: 10 }}>last 30 days</span>
        </div>
        <RunBars days={pb.run_days_30 ?? []} />
      </section>

      <div className="grid two">
        <section className="section">
          <div className="section-head"><span className="eyebrow">Definition</span></div>
          <div className="card" style={{ padding: 18 }}>
            <BriefPanel
              getPath={`/api/playbooks/${encodeURIComponent(slug)}/brief`}
              putPath={`/api/playbooks/${encodeURIComponent(slug)}/brief`}
              empty="No playbook definition yet."
            />
          </div>
        </section>

        <section className="section">
          <div className="section-head">
            <span className="eyebrow">Recent runs</span>
            <span className="section-count">{pb.recent_runs?.length ?? 0}</span>
          </div>
          <div className="rows">
            {(pb.recent_runs ?? []).map((r) => (
              <div key={r.slug} className="lrow" onClick={() => navigate(`/session/${r.slug}`)}>
                <div className="lrow-main">
                  <div className="lrow-title clip">{r.name}</div>
                  <div className="lrow-sub clip">{r.status} · {ago(r.created_at)}</div>
                </div>
                <span className={`prio ${r.priority}`} />
              </div>
            ))}
            {(!pb.recent_runs || pb.recent_runs.length === 0) && (
              <div className="lrow"><span className="faint">No runs yet. Hit “Run playbook”.</span></div>
            )}
          </div>
        </section>
      </div>
    </div>
  )
}
