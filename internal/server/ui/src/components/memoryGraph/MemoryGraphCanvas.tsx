import { useEffect, useMemo, useRef, useState, type ComponentType } from 'react'
import {
  Background,
  BackgroundVariant,
  Controls,
  MarkerType,
  MiniMap,
  ReactFlow,
  useNodesState,
  type Edge,
  type Node,
  type NodeMouseHandler,
  type NodeProps,
  type ReactFlowInstance,
} from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { MemoryGraphNode } from './MemoryGraphNode'
import type { MemoryGraph, MemoryGraphNode as MemoryGraphNodeView } from '../../screens/memoryGraph'

type FlowNodeData = MemoryGraphNodeView & {
  actionVisible?: boolean
  onOpenDetails?: (node: MemoryGraphNodeView) => void
}
type FlowNode = Node<FlowNodeData, 'memory'>
type FlowEdge = Edge

const NODE_W = 264
const NODE_H = 126
const CLUSTER_GAP = 420
const RING_GAP = 220
const FIRST_RING_RADIUS = 290
const NODE_ARC = NODE_W + 84

const nodeTypes = {
  memory: MemoryGraphNode as unknown as ComponentType<NodeProps>,
}

interface Cluster {
  key: string
  nodes: MemoryGraphNodeView[]
  rootId?: string
  radius: number
}

function clusterFallbackKey(node: MemoryGraphNodeView) {
  return `${node.provider}:${node.scope}:${node.kind}`
}

function buildClusters(graph: MemoryGraph): Cluster[] {
  const rootByChild = new Map<string, string>()
  const rootIds = new Set<string>()
  for (const edge of graph.edges) {
    if (edge.kind !== 'contains') continue
    rootByChild.set(edge.target, edge.source)
    rootIds.add(edge.source)
  }

  const clusters = new Map<string, Cluster>()
  for (const node of graph.nodes) {
    const key = rootIds.has(node.id) ? node.id : rootByChild.get(node.id) ?? clusterFallbackKey(node)
    const cluster = clusters.get(key) ?? { key, nodes: [], rootId: rootIds.has(key) ? key : undefined, radius: 0 }
    cluster.nodes.push(node)
    clusters.set(key, cluster)
  }

  for (const cluster of clusters.values()) cluster.radius = clusterRadius(cluster)
  return Array.from(clusters.values()).sort((a, b) => b.nodes.length - a.nodes.length || a.key.localeCompare(b.key))
}

function clusterRadius(cluster: Cluster) {
  const outerNodes = cluster.rootId ? cluster.nodes.length - 1 : cluster.nodes.length
  if (outerNodes <= 0) return 220

  let remaining = outerNodes
  let ring = 0
  let radius = FIRST_RING_RADIUS
  while (remaining > 0) {
    radius = FIRST_RING_RADIUS + ring * RING_GAP
    const capacity = Math.max(6, Math.floor((2 * Math.PI * radius) / NODE_ARC))
    remaining -= capacity
    ring += 1
  }
  return radius + NODE_W / 2 + 110
}

function clusterCenters(clusters: Cluster[]) {
  const centers = new Map<string, { x: number; y: number }>()
  if (clusters.length === 0) return centers

  const totalNodes = clusters.reduce((sum, cluster) => sum + cluster.nodes.length, 0)
  const maxRowWidth = Math.max(5200, Math.sqrt(totalNodes) * 620)
  let x = 0
  let y = 0
  let rowHeight = 0

  for (const cluster of clusters) {
    const width = cluster.radius * 2 + CLUSTER_GAP
    const height = cluster.radius * 2 + CLUSTER_GAP
    if (x > 0 && x + width > maxRowWidth) {
      x = 0
      y += rowHeight
      rowHeight = 0
    }
    centers.set(cluster.key, { x: x + width / 2, y: y + height / 2 })
    x += width
    rowHeight = Math.max(rowHeight, height)
  }

  const placed = Array.from(centers.values())
  const minX = Math.min(...placed.map((center) => center.x))
  const maxX = Math.max(...placed.map((center) => center.x))
  const minY = Math.min(...placed.map((center) => center.y))
  const maxY = Math.max(...placed.map((center) => center.y))
  const offsetX = (minX + maxX) / 2
  const offsetY = (minY + maxY) / 2
  for (const center of placed) {
    center.x -= offsetX
    center.y -= offsetY
  }
  return centers
}

function placeCluster(cluster: Cluster, center: { x: number; y: number }) {
  const root = cluster.rootId ? cluster.nodes.find((node) => node.id === cluster.rootId) : undefined
  const placed = new Map<string, { x: number; y: number }>()
  if (root) placed.set(root.id, { x: center.x - NODE_W / 2, y: center.y - NODE_H / 2 })

  const outerNodes = cluster.nodes
    .filter((node) => node.id !== root?.id)
    .sort((a, b) => a.label.localeCompare(b.label) || a.id.localeCompare(b.id))
  if (!root && outerNodes.length === 1) {
    placed.set(outerNodes[0].id, { x: center.x - NODE_W / 2, y: center.y - NODE_H / 2 })
    return placed
  }

  let cursor = 0
  let ring = 0
  while (cursor < outerNodes.length) {
    const radius = FIRST_RING_RADIUS + ring * RING_GAP
    const capacity = Math.max(6, Math.floor((2 * Math.PI * radius) / NODE_ARC))
    const count = Math.min(capacity, outerNodes.length - cursor)
    const offset = (ring % 2) * (Math.PI / Math.max(count, 1)) + (cluster.key.length % 13) * 0.021

    for (let i = 0; i < count; i += 1) {
      const node = outerNodes[cursor + i]
      const angle = offset + (2 * Math.PI * i) / count
      placed.set(node.id, {
        x: center.x + Math.cos(angle) * radius - NODE_W / 2,
        y: center.y + Math.sin(angle) * radius - NODE_H / 2,
      })
    }

    cursor += count
    ring += 1
  }
  return placed
}

function resolveNodeCollisions(positions: Map<string, { x: number; y: number }>) {
  const ids = Array.from(positions.keys()).sort()
  const minDx = NODE_W + 56
  const minDy = NODE_H + 34

  for (let pass = 0; pass < 36; pass += 1) {
    let moved = false
    for (let i = 0; i < ids.length; i += 1) {
      const a = positions.get(ids[i])
      if (!a) continue
      for (let j = i + 1; j < ids.length; j += 1) {
        const b = positions.get(ids[j])
        if (!b) continue

        const ax = a.x + NODE_W / 2
        const ay = a.y + NODE_H / 2
        const bx = b.x + NODE_W / 2
        const by = b.y + NODE_H / 2
        const dx = ax - bx
        const dy = ay - by
        const overlapX = minDx - Math.abs(dx)
        const overlapY = minDy - Math.abs(dy)

        if (overlapX <= 0 || overlapY <= 0) continue
        moved = true

        if (overlapX < overlapY) {
          const direction = dx === 0 ? (i % 2 === 0 ? 1 : -1) : Math.sign(dx)
          const shift = overlapX / 2 + 1
          a.x += direction * shift
          b.x -= direction * shift
        } else {
          const direction = dy === 0 ? (j % 2 === 0 ? 1 : -1) : Math.sign(dy)
          const shift = overlapY / 2 + 1
          a.y += direction * shift
          b.y -= direction * shift
        }
      }
    }
    if (!moved) return positions
  }
  return positions
}

function layoutNodes(graph: MemoryGraph): FlowNode[] {
  const clusters = buildClusters(graph)
  const centers = clusterCenters(clusters)
  const positions = new Map<string, { x: number; y: number }>()
  for (const cluster of clusters) {
    const center = centers.get(cluster.key)
    if (!center) continue
    for (const [id, position] of placeCluster(cluster, center)) positions.set(id, position)
  }
  resolveNodeCollisions(positions)

  return graph.nodes.map((node, index) => {
    const placed = positions.get(node.id)
    const fallbackCol = index % 4
    const fallbackRow = Math.floor(index / 4)
    return {
      id: node.id,
      type: 'memory',
      data: node,
      position: placed ?? { x: fallbackCol * (NODE_W + 40), y: fallbackRow * (NODE_H + 32) },
      width: NODE_W,
      height: NODE_H,
    }
  })
}

function focusSets(graph: MemoryGraph, selectedId?: string | null) {
  const connectedNodeIds = new Set<string>()
  const focusedEdgeIds = new Set<string>()
  if (!selectedId) return { connectedNodeIds, focusedEdgeIds }

  for (const edge of graph.edges) {
    if (edge.source !== selectedId && edge.target !== selectedId) continue
    focusedEdgeIds.add(edge.id)
    connectedNodeIds.add(edge.source)
    connectedNodeIds.add(edge.target)
  }
  connectedNodeIds.delete(selectedId)
  return { connectedNodeIds, focusedEdgeIds }
}

function flowEdges(graph: MemoryGraph, selectedId?: string | null): FlowEdge[] {
  const showLabels = graph.edges.length <= 140
  const { focusedEdgeIds } = focusSets(graph, selectedId)
  return graph.edges.map((edge) => ({
    id: edge.id,
    source: edge.source,
    target: edge.target,
    className: selectedId ? (focusedEdgeIds.has(edge.id) ? 'focused' : 'dimmed') : undefined,
    label: showLabels ? edge.label : undefined,
    markerEnd: {
      type: MarkerType.ArrowClosed,
      color: edge.kind === 'references' ? 'var(--accent-line)' : 'var(--text-3)',
      width: 16,
      height: 16,
    },
    style: {
      stroke: edge.kind === 'references' ? 'var(--accent-line)' : 'var(--text-3)',
      strokeWidth: focusedEdgeIds.has(edge.id) ? 2.6 : edge.kind === 'references' ? 1.6 : 1.2,
      strokeDasharray: edge.kind === 'contains' ? '5 5' : undefined,
      opacity: selectedId ? (focusedEdgeIds.has(edge.id) ? 0.96 : 0.08) : edge.kind === 'contains' ? 0.55 : 0.82,
    },
    labelStyle: { fill: 'var(--text-2)', fontSize: 10, fontFamily: 'var(--font-mono)' },
    zIndex: focusedEdgeIds.has(edge.id) ? 20 : 0,
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
  onOpenDetails,
  onClearSelection,
}: {
  graph: MemoryGraph
  selectedId?: string | null
  onSelectNode: (node: MemoryGraphNodeView) => void
  onOpenDetails: (node: MemoryGraphNodeView) => void
  onClearSelection: () => void
}) {
  const [instance, setInstance] = useState<ReactFlowInstance<FlowNode, FlowEdge> | null>(null)
  const fittedRef = useRef(false)
  const baseNodes = useMemo(() => layoutNodes(graph), [graph])
  const [graphNodes, setGraphNodes, onNodesChange] = useNodesState<FlowNode>(baseNodes)

  useEffect(() => {
    setGraphNodes(baseNodes)
  }, [baseNodes, setGraphNodes])

  const { connectedNodeIds } = useMemo(() => focusSets(graph, selectedId), [graph, selectedId])
  const nodes = useMemo(
    () =>
      graphNodes.map((node) => {
        const isSelected = node.id === selectedId
        const focus: MemoryGraphNodeView['focus'] = selectedId ? (isSelected ? 'selected' : connectedNodeIds.has(node.id) ? 'connected' : 'dimmed') : undefined
        return {
          ...node,
          selected: isSelected,
          data: {
            ...node.data,
            focus,
            actionVisible: isSelected,
            onOpenDetails,
          },
        }
      }),
    [connectedNodeIds, graphNodes, onOpenDetails, selectedId],
  )
  const edges = useMemo(() => flowEdges(graph, selectedId), [graph, selectedId])

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
        onNodesChange={onNodesChange}
        onPaneClick={onClearSelection}
        fitView
        fitViewOptions={{ padding: 0.18 }}
        minZoom={0.03}
        maxZoom={1.4}
        nodesDraggable
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
