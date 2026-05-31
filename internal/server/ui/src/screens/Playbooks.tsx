import { useLocation } from 'wouter'
import { Archive, Play, Repeat } from 'lucide-react'
import { usePlaybooks, useAction } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { confirmAction } from '../lib/confirm'
import { EmptyState, ErrorNote, Loading, Sparkline } from '../components/ui'
import { ago } from '../lib/format'

export function Playbooks() {
  useDocumentTitle('Playbooks')
  const [, navigate] = useLocation()
  const { data, isLoading, error } = usePlaybooks()
  const action = useAction()

  const run = (slug: string, e: React.MouseEvent) => {
    e.stopPropagation()
    action.mutate(
      { kind: 'spawn-run', target: slug },
      { onSuccess: (d) => d.agent && navigate(`/session/${d.agent.slug}`) },
    )
  }

  const archive = async (e: React.MouseEvent, slug: string, name: string) => {
    e.stopPropagation()
    const ok = await confirmAction({
      title: 'Archive this playbook?',
      body: `"${name}" will leave your active list. Past runs are unaffected and you can unarchive it later.`,
      confirmLabel: 'Archive',
      danger: true,
    })
    if (ok) action.mutate({ kind: 'archive', target: slug, entity_kind: 'playbook' })
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="eyebrow">repeatable workflows</div>
          <h1 className="h-xl">Playbooks</h1>
        </div>
      </div>

      {isLoading ? (
        <Loading rows={4} />
      ) : error ? (
        <ErrorNote error={error} />
      ) : !data || data.length === 0 ? (
        <EmptyState icon={<Repeat size={30} />} title="No playbooks" hint="Playbooks are reusable task templates an agent runs on demand." />
      ) : (
        <div className="grid cards stagger">
          {data.map((p) => (
            <article key={p.slug} className="card acard" onClick={() => navigate(`/playbook/${p.slug}`)}>
              <div className="acard-top">
                <Repeat size={16} className="dim" style={{ marginTop: 2 }} />
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div className="acard-title clip">{p.name}</div>
                  <div className="acard-ref clip">{p.project_slug || 'no project'}</div>
                </div>
                <button className="btn primary sm" onClick={(e) => run(p.slug, e)} disabled={action.isPending}>
                  <Play size={13} /> Run
                </button>
                <button
                  className="btn icon ghost sm row-action"
                  title="Archive playbook"
                  aria-label="Archive playbook"
                  onClick={(e) => archive(e, p.slug, p.name)}
                >
                  <Archive size={14} />
                </button>
              </div>
              <div className="acard-foot" style={{ borderTop: 'none', paddingTop: 0 }}>
                <span className="num" style={{ fontSize: 12.5 }}>
                  <b>{p.run_count_7d}</b> <span className="faint">runs · 7d</span>
                </span>
                <div className="spacer" />
                <Sparkline data={p.run_days_30?.slice(-14) ?? []} />
              </div>
              {p.recent_runs?.[0] && (
                <div className="faint" style={{ fontSize: 11.5 }}>last run {ago(p.recent_runs[0].created_at)}</div>
              )}
            </article>
          ))}
        </div>
      )}
    </div>
  )
}
