import { AlertTriangle, FileText } from 'lucide-react'
import { Handle, Position } from '@xyflow/react'
import { ProviderIcon, StatusDot } from '../ui'
import type { MemoryGraphNode as MemoryGraphNodeView } from '../../screens/memoryGraph'

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

export function MemoryGraphNode({ data, selected }: { data: MemoryGraphNodeView; selected?: boolean }) {
  return (
    <div className={`memory-node${selected ? ' selected' : ''}${data.available ? '' : ' muted'}`}>
      <Handle type="target" position={Position.Left} className="memory-node-handle" />
      <Handle type="source" position={Position.Right} className="memory-node-handle" />
      <div className="memory-node-top">
        <span className={`badge ${statusTone(data.status)}`}>
          <StatusDot status={data.status} />
          {data.status}
        </span>
        {data.error ? <AlertTriangle size={14} className="memory-node-warn" /> : <FileText size={14} />}
      </div>
      <div className="memory-node-title" title={data.label}>{data.label}</div>
      <div className="memory-node-path" title={data.path}>{data.path}</div>
      <div className="memory-node-meta">
        {data.provider ? (
          <span className="memory-node-chip">
            <ProviderIcon provider={data.provider} size={13} />
            {data.provider}
          </span>
        ) : null}
        {data.badges.slice(0, 2).map((badge) => (
          <span className="memory-node-chip" key={badge}>{badge}</span>
        ))}
      </div>
    </div>
  )
}
