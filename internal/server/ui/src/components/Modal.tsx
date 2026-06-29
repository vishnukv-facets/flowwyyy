import { useEffect, useId, type ReactNode } from 'react'
import { FlowMark } from './FlowMark'

// Lightweight modal: scrim + centered panel, Esc / scrim-click to close.
export function Modal({
  open,
  onClose,
  title,
  children,
  footer,
  className = '',
  scrimClassName = '',
  bodyClassName = '',
  width = 560,
}: {
  open: boolean
  onClose: () => void
  title: ReactNode
  children: ReactNode
  footer?: ReactNode
  className?: string
  scrimClassName?: string
  bodyClassName?: string
  width?: number
}) {
  const titleID = useId()
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  if (!open) return null
  return (
    <div
      className={`scrim${scrimClassName ? ` ${scrimClassName}` : ''}`}
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
      role="presentation"
    >
      <div
        className={`modal${className ? ` ${className}` : ''}`}
        style={{ width }}
        onMouseDown={(e) => e.stopPropagation()}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleID}
      >
        <div className="modal-head">
          <div className="modal-title">
            <FlowMark size={22} className="modal-mark" animated={false} />
            <span id={titleID} className="h-lg">{title}</span>
          </div>
          <button type="button" className="btn icon ghost sm" onClick={onClose} aria-label="Close">
            ✕
          </button>
        </div>
        <div className={`modal-body${bodyClassName ? ` ${bodyClassName}` : ''}`}>{children}</div>
        {footer && <div className="modal-foot">{footer}</div>}
      </div>
    </div>
  )
}
