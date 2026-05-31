import { Modal } from './Modal'
import { resolveConfirm, useConfirm } from '../lib/confirm'

// Single mounted host that renders the universal confirm dialog.
export function ConfirmHost() {
  const c = useConfirm()
  return (
    <Modal
      open={!!c}
      onClose={() => resolveConfirm(false)}
      title={c?.title ?? ''}
      width={440}
      footer={
        <>
          <div className="spacer" />
          <button className="btn" onClick={() => resolveConfirm(false)}>
            {c?.cancelLabel ?? 'Cancel'}
          </button>
          <button
            className={`btn ${c?.danger ? 'danger' : 'primary'}`}
            autoFocus
            onClick={() => resolveConfirm(true)}
          >
            {c?.confirmLabel ?? 'Confirm'}
          </button>
        </>
      }
    >
      {c?.body && <p className="confirm-body">{c.body}</p>}
    </Modal>
  )
}
