import { RotateCcw, Trash2, Flame } from 'lucide-react'
import { useUiData, useAction } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { EmptyState, Loading } from '../components/ui'
import { confirmAction } from '../lib/confirm'
import type { TrashItem } from '../lib/types'

export function Trash() {
  useDocumentTitle('Trash')
  const { data: ui, isLoading } = useUiData()
  const action = useAction()

  if (isLoading) return <div className="page"><Loading rows={4} /></div>
  const trash = ui?.TRASH
  // Go marshals empty slices as null, so guard every group before spreading.
  const items: TrashItem[] = trash
    ? [...(trash.tasks ?? []), ...(trash.projects ?? []), ...(trash.playbooks ?? [])]
    : []

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="eyebrow">soft-deleted</div>
          <h1 className="h-xl">Trash</h1>
        </div>
        <div className="spacer" />
        {items.length > 0 && (
          <button
            className="btn danger"
            disabled={action.isPending}
            onClick={async () => {
              const ok = await confirmAction({
                title: 'Empty trash?',
                body: `Permanently delete all ${items.length} item${items.length === 1 ? '' : 's'} in trash. This cannot be undone. Items still referenced by active tasks are kept.`,
                confirmLabel: 'Empty Trash',
                danger: true,
              })
              if (ok) action.mutate({ kind: 'empty-trash' })
            }}
          >
            <Trash2 size={14} /> Empty Trash
          </button>
        )}
      </div>

      {items.length === 0 ? (
        <EmptyState icon={<Trash2 size={30} />} title="Trash is empty" hint="Deleted tasks, projects, and playbooks rest here before permanent removal." />
      ) : (
        <div className="rows">
          {items.map((it) => (
            <div key={`${it.kind}-${it.slug}`} className="lrow" style={{ cursor: 'default' }}>
              <span className="badge">{it.kind}</span>
              <div className="lrow-main">
                <div className="lrow-title clip">{it.name}</div>
                <div className="lrow-sub clip">{it.slug}{it.project ? ` · ${it.project}` : ''}</div>
              </div>
              <button
                className="btn sm"
                disabled={action.isPending}
                onClick={async () => {
                  const ok = await confirmAction({
                    title: `Restore this ${it.kind}?`,
                    body: `“${it.name}” will be moved back into your active ${it.kind}s.`,
                    confirmLabel: 'Restore',
                  })
                  if (ok) action.mutate({ kind: 'restore', target: it.slug, entity_kind: it.kind })
                }}
              >
                <RotateCcw size={13} /> Restore
              </button>
              <button
                className="btn sm danger"
                disabled={action.isPending}
                onClick={async () => {
                  const ok = await confirmAction({
                    title: `Permanently delete this ${it.kind}?`,
                    body: `“${it.name}” will be permanently removed. This cannot be undone.`,
                    confirmLabel: 'Destroy',
                    danger: true,
                  })
                  if (ok) action.mutate({ kind: 'destroy', target: it.slug, entity_kind: it.kind })
                }}
              >
                <Flame size={13} /> Destroy
              </button>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
