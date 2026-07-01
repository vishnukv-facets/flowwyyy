import { AlertTriangle, PanelRightOpen } from 'lucide-react'
import { Handle, NodeToolbar, Position } from '@xyflow/react'
import { ProviderIcon, StatusDot } from '../ui'
import type { MemoryGraphNode as MemoryGraphNodeView } from '../../screens/memoryGraph'

type MemoryGraphNodeData = MemoryGraphNodeView & {
  actionVisible?: boolean
  onOpenDetails?: (node: MemoryGraphNodeView) => void
}

function statusTone(status: string) {
  switch (status) {
    case 'available':
      return 'ok'
    case 'missing':
    case 'unavailable':
      return 'warn'
    case 'error':
      return 'danger'
    default:
      return ''
  }
}

export function MemoryGraphNode({ data, selected }: { data: MemoryGraphNodeData; selected?: boolean }) {
  const focusClass = data.focus ? ` focus-${data.focus}` : ''
  return (
    <div className={`memory-node${selected ? ' selected' : ''}${data.available ? '' : ' muted'}${focusClass}`}>
      <NodeToolbar isVisible={data.actionVisible} position={Position.Top} align="center" offset={10} className="memory-node-toolbar">
        <button
          type="button"
          className="memory-node-toolbar-button"
          onClick={(event) => {
            event.stopPropagation()
            data.onOpenDetails?.(data)
          }}
        >
          <PanelRightOpen size={13} />
          Details
        </button>
      </NodeToolbar>
      <Handle type="target" position={Position.Left} className="memory-node-handle" />
      <Handle type="source" position={Position.Right} className="memory-node-handle" />
      <div className="memory-node-top">
        {data.provider ? (
          <span className="memory-node-provider">
            <ProviderIcon provider={data.provider} size={13} />
            {data.provider}
          </span>
        ) : null}
        {data.status !== 'available' || data.error ? (
          <span className={`badge ${statusTone(data.status)}`}>
            <StatusDot status={data.status} />
            {data.status}
          </span>
        ) : null}
        {data.error ? <AlertTriangle size={14} className="memory-node-warn" /> : null}
      </div>
      <div className="memory-node-title" title={data.label}>{data.label}</div>
      <div className="memory-node-path" title={data.path}>{data.path}</div>
      <div className="memory-node-meta">
        {data.badges.slice(0, 2).map((badge) => (
          <span className="memory-node-chip" key={badge}>{badge}</span>
        ))}
      </div>
    </div>
  )
}
