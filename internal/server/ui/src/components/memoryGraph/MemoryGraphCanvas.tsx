import { useEffect, useMemo, useRef, useState, type ComponentType } from 'react'
import {
  Background,
  BackgroundVariant,
  Controls,
  MarkerType,
  MiniMap,
  ReactFlow,
  type Edge,
  type Node,
  type NodeMouseHandler,
  type NodeProps,
  type ReactFlowInstance,
} from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import dagre from '@dagrejs/dagre'
import { MemoryGraphNode } from './MemoryGraphNode'
import type { MemoryGraph, MemoryGraphNode as MemoryGraphNodeView } from '../../screens/memoryGraph'

type FlowNode = Node<MemoryGraphNodeView, 'memory'>
type FlowEdge = Edge

const NODE_W = 264
const NODE_H = 126
const RANK_SEP = 96
const NODE_SEP = 34

const nodeTypes = {
  memory: MemoryGraphNode as unknown as ComponentType<NodeProps>,
}

function layoutNodes(graph: MemoryGraph): FlowNode[] {
  const g = new dagre.graphlib.Graph({ multigraph: true })
  g.setGraph({ rankdir: 'LR', nodesep: NODE_SEP, ranksep: RANK_SEP, marginx: 24, marginy: 24 })
  g.setDefaultEdgeLabel(() => ({}))
  for (const node of graph.nodes) g.setNode(node.id, { width: NODE_W, height: NODE_H })
  for (const edge of graph.edges) g.setEdge(edge.source, edge.target, {}, edge.id)
  dagre.layout(g)

  return graph.nodes.map((node, index) => {
    const placed = g.node(node.id)
    const fallbackCol = index % 4
    const fallbackRow = Math.floor(index / 4)
    return {
      id: node.id,
      type: 'memory',
      data: node,
      position: placed
        ? { x: placed.x - NODE_W / 2, y: placed.y - NODE_H / 2 }
        : { x: fallbackCol * (NODE_W + 40), y: fallbackRow * (NODE_H + 32) },
      width: NODE_W,
      height: NODE_H,
    }
  })
}

function flowEdges(graph: MemoryGraph): FlowEdge[] {
  return graph.edges.map((edge) => ({
    id: edge.id,
    source: edge.source,
    target: edge.target,
    label: edge.label,
    markerEnd: { type: MarkerType.ArrowClosed, color: 'var(--accent-line)', width: 16, height: 16 },
    style: { stroke: 'var(--accent-line)', strokeWidth: 1.5 },
    labelStyle: { fill: 'var(--text-2)', fontSize: 10, fontFamily: 'var(--font-mono)' },
  }))
}

function miniMapColor(node: Node) {
  const data = node.data as MemoryGraphNodeView
  if (data.status === 'error') return 'var(--danger)'
  if (!data.available) return 'var(--warn)'
  if (data.provider === 'claude') return 'var(--info)'
  return 'var(--accent)'
}

export function MemoryGraphCanvas({
  graph,
  selectedId,
  onSelectNode,
  onClearSelection,
}: {
  graph: MemoryGraph
  selectedId?: string | null
  onSelectNode: (node: MemoryGraphNodeView) => void
  onClearSelection: () => void
}) {
  const [instance, setInstance] = useState<ReactFlowInstance<FlowNode, FlowEdge> | null>(null)
  const fittedRef = useRef(false)
  const baseNodes = useMemo(() => layoutNodes(graph), [graph])
  const nodes = useMemo(
    () => baseNodes.map((node) => (node.selected === (node.id === selectedId) ? node : { ...node, selected: node.id === selectedId })),
    [baseNodes, selectedId],
  )
  const edges = useMemo(() => flowEdges(graph), [graph])

  useEffect(() => {
    fittedRef.current = false
  }, [graph.nodes.length, graph.edges.length])

  useEffect(() => {
    if (!instance || nodes.length === 0 || fittedRef.current) return
    fittedRef.current = true
    const timer = window.setTimeout(() => instance.fitView({ padding: 0.18, duration: 220 }), 60)
    return () => window.clearTimeout(timer)
  }, [instance, nodes.length])

  const onNodeClick: NodeMouseHandler<FlowNode> = (_event, node) => onSelectNode(node.data as MemoryGraphNodeView)

  return (
    <div className="memory-graph-canvas">
      <ReactFlow<FlowNode, FlowEdge>
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        onInit={setInstance}
        onNodeClick={onNodeClick}
        onPaneClick={onClearSelection}
        fitView
        fitViewOptions={{ padding: 0.18 }}
        minZoom={0.2}
        maxZoom={1.4}
        nodesDraggable={false}
        nodesConnectable={false}
        elementsSelectable
        proOptions={{ hideAttribution: true }}
      >
        <Background variant={BackgroundVariant.Dots} gap={18} size={1} />
        <Controls showInteractive={false} />
        <MiniMap pannable zoomable nodeColor={miniMapColor} nodeStrokeWidth={2} maskColor="color-mix(in srgb, var(--bg) 72%, transparent)" />
      </ReactFlow>
    </div>
  )
}
