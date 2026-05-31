import { CheckCircle2, Info, XCircle } from 'lucide-react'
import { dismissToast, useToasts } from '../lib/toast'

export function Toaster() {
  const toasts = useToasts()
  return (
    <div className="toaster">
      {toasts.map((t) => (
        <div key={t.id} className={`toast ${t.kind}`} onClick={() => dismissToast(t.id)}>
          <span className="toast-icon">
            {t.kind === 'ok' ? (
              <CheckCircle2 size={15} />
            ) : t.kind === 'error' ? (
              <XCircle size={15} />
            ) : (
              <Info size={15} />
            )}
          </span>
          <span className="toast-msg">{t.message}</span>
        </div>
      ))}
    </div>
  )
}
