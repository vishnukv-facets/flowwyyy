import { useState } from 'react'
import { HardDrive, Plus, Trash2 } from 'lucide-react'
import { queryClient, useWorkdirs } from '../lib/query'
import { apiAction } from '../lib/api'
import { pushToast } from '../lib/toast'
import { confirmAction } from '../lib/confirm'
import { AddWorkdirModal } from '../components/modals'
import { EmptyState, ErrorNote, Loading } from '../components/ui'
import { ago } from '../lib/format'

export function Workdirs() {
  const { data, isLoading, error } = useWorkdirs()
  const [addOpen, setAddOpen] = useState(false)
  const [removing, setRemoving] = useState<string | null>(null)

  const remove = async (path: string) => {
    const ok = await confirmAction({
      title: 'Unregister workdir?',
      body: `${path}\n\nTasks already using it are unaffected — this only removes it from the registry.`,
      confirmLabel: 'Unregister',
      danger: true,
    })
    if (!ok) return
    setRemoving(path)
    try {
      await apiAction({ kind: 'workdir-remove', path })
      pushToast('ok', 'workdir unregistered')
      queryClient.invalidateQueries()
    } catch (e) {
      pushToast('error', e instanceof Error ? e.message : 'could not remove workdir')
    } finally {
      setRemoving(null)
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="eyebrow">registered repositories</div>
          <h1 className="h-xl">Workdirs</h1>
        </div>
        <div className="spacer" />
        <button className="btn" onClick={() => setAddOpen(true)}>
          <Plus size={15} /> Add workdir
        </button>
      </div>
      {isLoading ? (
        <Loading rows={4} />
      ) : error ? (
        <ErrorNote error={error} />
      ) : !data || data.length === 0 ? (
        <EmptyState
          icon={<HardDrive size={30} />}
          title="No workdirs"
          hint="Directories used by tasks register here automatically — or add one manually."
        />
      ) : (
        <div className="card" style={{ padding: '6px 14px 4px' }}>
          <table className="tbl fixed">
            <colgroup>
              <col />
              <col style={{ width: 280 }} />
              <col style={{ width: 70 }} />
              <col style={{ width: 96 }} />
              <col style={{ width: 44 }} />
            </colgroup>
            <thead>
              <tr>
                <th>Directory</th>
                <th>Remote</th>
                <th style={{ textAlign: 'right' }}>Tasks</th>
                <th style={{ textAlign: 'right' }}>Last used</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {data.map((w) => (
                <tr key={w.path} style={{ cursor: 'default' }}>
                  <td>
                    <div style={{ fontWeight: 500 }} className="clip">{w.name || w.path.split('/').pop()}</div>
                    <div className="mono faint clip" style={{ fontSize: 11 }}>{w.path}</div>
                  </td>
                  <td className="dim mono clip" style={{ fontSize: 11.5 }}>{w.git_remote || '—'}</td>
                  <td className="num" style={{ textAlign: 'right' }}>{w.tasks_using_this}</td>
                  <td className="dim mono" style={{ textAlign: 'right', fontSize: 11.5 }}>
                    {w.untouched_30d ? <span className="faint">untouched</span> : ago(w.last_used_at)}
                  </td>
                  <td style={{ textAlign: 'right' }}>
                    <button
                      className="btn icon ghost sm row-action"
                      title="Unregister workdir"
                      aria-label="Unregister workdir"
                      disabled={removing === w.path}
                      onClick={() => remove(w.path)}
                    >
                      <Trash2 size={13} />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <AddWorkdirModal open={addOpen} onClose={() => setAddOpen(false)} />
    </div>
  )
}
