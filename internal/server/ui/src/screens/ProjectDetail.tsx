import { useLocation } from 'wouter'
import { ArrowLeft } from 'lucide-react'
import { useProject, useProjectTasks } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { BriefPanel } from '../components/BriefPanel'
import { ErrorNote, Loading, ProviderIcon, StatusDot } from '../components/ui'
import { ago } from '../lib/format'

export function ProjectDetail({ slug }: { slug: string }) {
  const [, navigate] = useLocation()
  const { data: project, isLoading, error } = useProject(slug)
  const { data: tasks } = useProjectTasks(slug)
  useDocumentTitle(project?.name)

  if (isLoading) return <div className="page"><Loading /></div>
  if (error) return <div className="page"><ErrorNote error={error} /></div>
  if (!project) return null

  return (
    <div className="page">
      <button className="btn ghost sm" style={{ marginBottom: 14 }} onClick={() => navigate('/projects')}>
        <ArrowLeft size={14} /> Projects
      </button>

      <div className="detail-head">
        <div style={{ flex: 1 }}>
          <div className="eyebrow">project</div>
          <div className="detail-title">{project.name}</div>
          <div className="detail-ref">{project.slug}</div>
        </div>
        <span className={`prio ${project.priority}`}>{project.priority}</span>
      </div>

      <div className="meta-grid" style={{ marginBottom: 24 }}>
        <div className="meta-cell"><div className="meta-k">working dir</div><div className="meta-v mono clip" title={project.work_dir}>{project.work_dir}</div></div>
        <div className="meta-cell"><div className="meta-k">active</div><div className="meta-v num">{project.task_counts.in_progress}</div></div>
        <div className="meta-cell"><div className="meta-k">backlog</div><div className="meta-v num">{project.task_counts.backlog}</div></div>
        <div className="meta-cell"><div className="meta-k">done</div><div className="meta-v num">{project.task_counts.done}</div></div>
      </div>

      <div className="grid two">
        <section className="section">
          <div className="section-head"><span className="eyebrow">Brief</span></div>
          <div className="card" style={{ padding: 18 }}>
            <BriefPanel
              getPath={`/api/projects/${encodeURIComponent(slug)}/brief`}
              putPath={`/api/projects/${encodeURIComponent(slug)}/brief`}
              empty="No project brief yet. Click Edit to add one — agents read this on startup."
            />
          </div>
        </section>

        <section className="section">
          <div className="section-head">
            <span className="eyebrow">Tasks</span>
            <span className="section-count">{tasks?.length ?? 0}</span>
          </div>
          <div className="rows">
            {(tasks ?? []).map((t) => (
              <div key={t.slug} className="lrow" onClick={() => navigate(`/session/${t.slug}`)}>
                <StatusDot status={t.live ? 'running' : t.status} />
                <ProviderIcon provider={t.session_provider} size={14} />
                <div className="lrow-main">
                  <div className="lrow-title clip">{t.name}</div>
                  <div className="lrow-sub clip">{t.status} · {ago(t.updated_at)}</div>
                </div>
                <span className={`prio ${t.priority}`} />
              </div>
            ))}
            {(!tasks || tasks.length === 0) && <div className="lrow"><span className="faint">No tasks in this project yet.</span></div>}
          </div>
        </section>
      </div>
    </div>
  )
}
