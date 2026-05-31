import { useEffect, useRef, type ReactNode } from 'react'
import { ChevronDown, Check } from 'lucide-react'

export interface SelectOption {
  value: string
  label: string
  icon?: ReactNode
}

// Custom, theme-consistent dropdown — replaces the OS-native <select> so menus
// match the app on every platform. Built on <details> with an outside-click
// close, mirroring the rest of the app's menu pattern.
export function Select({
  value,
  onChange,
  options,
  placeholder = 'Select…',
}: {
  value: string
  onChange: (value: string) => void
  options: SelectOption[]
  placeholder?: string
}) {
  const ref = useRef<HTMLDetailsElement>(null)
  const current = options.find((o) => o.value === value)

  useEffect(() => {
    const onDown = (e: globalThis.MouseEvent) => {
      if (ref.current?.open && !ref.current.contains(e.target as Node)) {
        ref.current.removeAttribute('open')
      }
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [])

  const pick = (e: { currentTarget: EventTarget & HTMLElement }, v: string) => {
    e.currentTarget.closest('details')?.removeAttribute('open')
    onChange(v)
  }

  return (
    <details className="menu select" ref={ref}>
      <summary className="input select-trigger">
        {current?.icon && <span className="select-ico">{current.icon}</span>}
        <span className="clip">{current ? current.label : placeholder}</span>
        <ChevronDown size={14} className="dim select-caret" />
      </summary>
      <div className="menu-pop select-pop">
        {options.map((o) => (
          <button
            key={o.value}
            type="button"
            className={`menu-item${o.value === value ? ' active' : ''}`}
            onClick={(e) => pick(e, o.value)}
          >
            {o.icon && <span className="select-ico">{o.icon}</span>}
            <span className="clip" style={{ flex: 1 }}>{o.label}</span>
            {o.value === value && <Check size={13} className="dim" />}
          </button>
        ))}
      </div>
    </details>
  )
}
