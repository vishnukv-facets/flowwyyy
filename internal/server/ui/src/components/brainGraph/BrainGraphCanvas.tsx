import { useEffect, useMemo, useRef, useState, type ComponentType } from 'react'
import {
  Background,
  BackgroundVariant,
  BaseEdge,
  Controls,
  EdgeLabelRenderer,
  MarkerType,
  MiniMap,
  ReactFlow,
  type Edge,
  type EdgeProps,
  type Node,
  type NodeMouseHandler,
  type NodeProps,
  type ReactFlowInstance,
} from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import dagre from '@dagrejs/dagre'
import { BrainGraphNode } from './BrainGraphNode'
import { OwnerGroupNode, type OwnerGroupData } from './OwnerBoundary'
import type { BrainGraphEdge, BrainGraphNode as BrainGraphNodeView, BrainGraphOwnerView } from '../../lib/types'

type Point = { x: number; y: number }
type BrainFlowNode = Node<BrainGraphNodeView, 'brain'>
type OwnerFlowNode = Node<OwnerGroupData, 'ownerGroup'>
type FlowNode = BrainFlowNode | OwnerFlowNode
type FlowEdge = Edge<{ edgeType: string; points?: Point[] }>

const NODE_W = 286
const NODE_H = 126
// dagre LR spacing: RANK_SEP is the horizontal gap between dependency ranks,
// NODE_SEP the vertical gap between siblings in the same rank.
const RANK_SEP = 96
const NODE_SEP = 30
const GRID_GAP = 28
const OWNER_GAP_X = 120
const OWNER_PAD_X = 26
const OWNER_PAD_TOP = 94
const OWNER_PAD_BOTTOM = 28
const OWNER_MIN_W = 340
const OWNER_MIN_H = 220
const CORNER_RADIUS = 10

const STATUS_RANK: Record<string, number> = {
  'approval_required': 0,
  'in-progress': 1,
  running: 1,
  backlog: 2,
  available: 3,
  linked: 3,
  done: 4,
  completed: 4,
  dead: 5,
  error: 5,
}
const TYPE_RANK: Record<string, number> = {
  task: 0,
  approval: 1,
  worker_run: 2,
  validator_run: 3,
  steward_run: 4,
  transcript_ref: 5,
  github_ref: 6,
}
const EDGE_COLOR: Record<string, string> = {
  parent: 'var(--accent-line)',
  depends_on: 'var(--warn)',
  run_of: 'var(--info)',
  external_ref: 'var(--text-3)',
  blocks: 'var(--danger)',
}
const EDGE_DASH: Record<string, string | undefined> = {
  run_of: '6 5',
  external_ref: '2 6',
}

function rankNode(node: BrainGraphNodeView) {
  return (STATUS_RANK[node.status] ?? 9) * 10 + (TYPE_RANK[node.type] ?? 8)
}

function taskSlugFromNode(node: BrainGraphNodeView) {
  if (node.type === 'task') return node.task_slug || node.id.replace(/^task:/, '')
  return node.task_slug || ''
}

function effectiveOwner(node: BrainGraphNodeView, taskOwners: Map<string, string>) {
  if (node.owner_slug) return node.owner_slug
  if (node.task_slug) return taskOwners.get(node.task_slug) || 'unowned'
  return 'unowned'
}

function ownerOrder(owners: BrainGraphOwnerView[], ownerSet: Set<string>) {
  const ordered: string[] = []
  const orderedSet = new Set<string>()
  for (const owner of owners) {
    if (ownerSet.has(owner.slug)) {
      ordered.push(owner.slug)
      orderedSet.add(owner.slug)
    }
  }
  for (const slug of ownerSet) {
    if (!orderedSet.has(slug)) ordered.push(slug)
  }
  // Keep "unowned" last so the meaningful owner families lead the canvas.
  ordered.sort((a, b) => Number(a === 'unowned') - Number(b === 'unowned'))
  return ordered.length ? ordered : ['unowned']
}

function ownerSummary(
  slug: string,
  ownerBySlug: Map<string, BrainGraphOwnerView>,
  effectiveOwners: Map<string, string>,
  nodes: BrainGraphNodeView[],
): BrainGraphOwnerView {
  const existing = ownerBySlug.get(slug)
  const summary: BrainGraphOwnerView = existing
    ? { ...existing }
    : {
        id: `owner:${slug}`,
        slug,
        name: slug === 'unowned' ? 'Unowned' : slug,
        status: slug === 'unowned' ? 'active' : 'missing',
        task_count: 0,
        running_count: 0,
      }

  if (!existing) {
    for (const node of nodes) {
      if (effectiveOwners.get(node.id) !== slug) continue
      if (node.type === 'task') summary.task_count++
      if (node.type === 'task' && (node.status === 'running' || node.status === 'in-progress')) summary.running_count++
    }
  }

  return summary
}

function appendToBucket<K, V>(map: Map<K, V[]>, key: K, value: V) {
  const existing = map.get(key)
  if (existing) {
    existing.push(value)
    return
  }
  map.set(key, [value])
}

interface OwnerLayout {
  positions: Map<string, Point>
  edgePoints: Map<string, Point[]>
  width: number
  height: number
}

// layoutOwnerSubgraph positions one owner's family. Connected tasks run through
// dagre (left→right) so dependency / parent / run / gate edges read as a real
// DAG; tasks with no intra-owner edge drop into a compact grid below it instead
// of stacking into one tall column. dagre's routed polyline for each edge is
// captured so the rendered line threads through the rank gaps rather than
// cutting under the cards. All coordinates are normalized to a 0,0 origin.
function layoutOwnerSubgraph(ownerNodes: BrainGraphNodeView[], ownerEdges: BrainGraphEdge[]): OwnerLayout {
  const positions = new Map<string, Point>()
  const edgePoints = new Map<string, Point[]>()

  const ids = new Set(ownerNodes.map((node) => node.id))
  const validEdges = ownerEdges.filter((edge) => edge.source !== edge.target && ids.has(edge.source) && ids.has(edge.target))
  const connectedIds = new Set<string>()
  for (const edge of validEdges) {
    connectedIds.add(edge.source)
    connectedIds.add(edge.target)
  }
  const connected = ownerNodes.filter((node) => connectedIds.has(node.id))
  const isolated = ownerNodes.filter((node) => !connectedIds.has(node.id))

  let dagreWidth = 0
  let dagreHeight = 0
  if (connected.length > 0) {
    const g = new dagre.graphlib.Graph({ multigraph: true })
    g.setGraph({ rankdir: 'LR', nodesep: NODE_SEP, ranksep: RANK_SEP, marginx: 4, marginy: 4 })
    g.setDefaultEdgeLabel(() => ({}))
    for (const node of connected) {
      g.setNode(node.id, { width: NODE_W, height: NODE_H })
    }
    const edgeNames = new Map<string, string>()
    let seq = 0
    for (const edge of validEdges) {
      const name = `e${seq++}`
      edgeNames.set(name, edge.id)
      g.setEdge(edge.source, edge.target, {}, name)
    }
    dagre.layout(g)

    let minX = Infinity
    let minY = Infinity
    let maxX = -Infinity
    let maxY = -Infinity
    for (const node of connected) {
      const placed = g.node(node.id)
      const x = (placed?.x ?? NODE_W / 2) - NODE_W / 2
      const y = (placed?.y ?? NODE_H / 2) - NODE_H / 2
      positions.set(node.id, { x, y })
      minX = Math.min(minX, x)
      minY = Math.min(minY, y)
      maxX = Math.max(maxX, x + NODE_W)
      maxY = Math.max(maxY, y + NODE_H)
    }
    if (!Number.isFinite(minX)) {
      minX = 0
      minY = 0
    }
    for (const [id, pos] of positions) {
      positions.set(id, { x: pos.x - minX, y: pos.y - minY })
    }
    for (const edgeObj of g.edges()) {
      const edgeId = edgeObj.name ? edgeNames.get(edgeObj.name) : undefined
      if (!edgeId) continue
      const label = g.edge(edgeObj) as { points?: Point[] } | undefined
      const pts = (label?.points ?? []).map((p) => ({ x: p.x - minX, y: p.y - minY }))
      if (pts.length >= 2) edgePoints.set(edgeId, pts)
    }
    dagreWidth = maxX - minX
    dagreHeight = maxY - minY
  }

  let gridWidth = 0
  let gridHeight = 0
  if (isolated.length > 0) {
    const cols = Math.max(2, Math.min(5, Math.round(Math.sqrt(isolated.length))))
    const startY = dagreHeight > 0 ? dagreHeight + GRID_GAP + 18 : 0
    isolated.forEach((node, index) => {
      const col = index % cols
      const row = Math.floor(index / cols)
      positions.set(node.id, { x: col * (NODE_W + GRID_GAP), y: startY + row * (NODE_H + GRID_GAP) })
    })
    const rows = Math.ceil(isolated.length / cols)
    gridWidth = cols * (NODE_W + GRID_GAP) - GRID_GAP
    gridHeight = startY + rows * (NODE_H + GRID_GAP) - GRID_GAP
  }

  return {
    positions,
    edgePoints,
    width: Math.max(dagreWidth, gridWidth),
    height: Math.max(dagreHeight, gridHeight),
  }
}

interface GraphLayout {
  nodes: FlowNode[]
  routes: Map<string, Point[]>
}

function layoutNodes(
  nodes: BrainGraphNodeView[],
  edges: BrainGraphEdge[],
  owners: BrainGraphOwnerView[],
): GraphLayout {
  const taskOwners = new Map<string, string>()
  for (const node of nodes) {
    if (node.type !== 'task') continue
    taskOwners.set(taskSlugFromNode(node), node.owner_slug || 'unowned')
  }

  const effectiveOwners = new Map<string, string>()
  const visibleOwners = new Set<string>()
  for (const node of nodes) {
    const owner = effectiveOwner(node, taskOwners)
    effectiveOwners.set(node.id, owner)
    visibleOwners.add(owner)
  }

  const ownerBySlug = new Map(owners.map((owner) => [owner.slug, owner]))
  const orderedOwners = ownerOrder(owners, visibleOwners)

  const nodesByOwner = new Map<string, BrainGraphNodeView[]>()
  for (const node of nodes) {
    appendToBucket(nodesByOwner, effectiveOwners.get(node.id) || 'unowned', node)
  }
  for (const list of nodesByOwner.values()) {
    list.sort((a, b) => rankNode(a) - rankNode(b) || a.label.localeCompare(b.label))
  }

  const placed = new Map<string, Point>()
  const routes = new Map<string, Point[]>()
  const groupPositions = new Map<string, { x: number; y: number; width: number; height: number }>()
  let cursorX = 0
  for (const owner of orderedOwners) {
    const ownerNodes = nodesByOwner.get(owner) ?? []
    const ownerEdges = edges.filter(
      (edge) => effectiveOwners.get(edge.source) === owner && effectiveOwners.get(edge.target) === owner,
    )
    const layout = layoutOwnerSubgraph(ownerNodes, ownerEdges)
    const boxWidth = Math.max(OWNER_MIN_W, layout.width + OWNER_PAD_X * 2)
    const boxHeight = Math.max(OWNER_MIN_H, layout.height + OWNER_PAD_TOP + OWNER_PAD_BOTTOM)
    groupPositions.set(owner, { x: cursorX, y: 0, width: boxWidth, height: boxHeight })

    const offsetX = cursorX + OWNER_PAD_X
    const offsetY = OWNER_PAD_TOP
    for (const [id, pos] of layout.positions) {
      placed.set(id, { x: offsetX + pos.x, y: offsetY + pos.y })
    }
    for (const [edgeId, pts] of layout.edgePoints) {
      routes.set(edgeId, pts.map((p) => ({ x: offsetX + p.x, y: offsetY + p.y })))
    }
    cursorX += boxWidth + OWNER_GAP_X
  }

  const ownerNodes: OwnerFlowNode[] = orderedOwners.map((slug) => {
    const group = groupPositions.get(slug) ?? { x: 0, y: 0, width: OWNER_MIN_W, height: OWNER_MIN_H }
    return {
      id: `owner-boundary:${slug}`,
      type: 'ownerGroup',
      data: { owner: ownerSummary(slug, ownerBySlug, effectiveOwners, nodes) },
      position: { x: group.x, y: group.y },
      style: { width: group.width, height: group.height },
      selectable: true,
      draggable: false,
      zIndex: 0,
    }
  })

  const graphNodes: BrainFlowNode[] = nodes.map((node) => {
    const position = placed.get(node.id) ?? { x: OWNER_PAD_X, y: OWNER_PAD_TOP }
    return {
      id: node.id,
      type: 'brain',
      data: node,
      position,
      width: NODE_W,
      height: NODE_H,
      zIndex: 2,
    }
  })

  return { nodes: [...ownerNodes, ...graphNodes], routes }
}

// roundedPath draws an SVG path through the routed waypoints with rounded
// corners, skipping any duplicate points dagre may emit.
function roundedPath(points: Point[], radius: number): string {
  const pts: Point[] = []
  for (const p of points) {
    const last = pts[pts.length - 1]
    if (!last || Math.abs(last.x - p.x) > 0.5 || Math.abs(last.y - p.y) > 0.5) pts.push(p)
  }
  if (pts.length < 2) return ''
  let d = `M ${pts[0].x},${pts[0].y}`
  for (let i = 1; i < pts.length - 1; i++) {
    const prev = pts[i - 1]
    const cur = pts[i]
    const next = pts[i + 1]
    const dIn = Math.hypot(cur.x - prev.x, cur.y - prev.y)
    const dOut = Math.hypot(next.x - cur.x, next.y - cur.y)
    const r = Math.min(radius, dIn / 2, dOut / 2)
    const p1 = { x: cur.x + ((prev.x - cur.x) / (dIn || 1)) * r, y: cur.y + ((prev.y - cur.y) / (dIn || 1)) * r }
    const p2 = { x: cur.x + ((next.x - cur.x) / (dOut || 1)) * r, y: cur.y + ((next.y - cur.y) / (dOut || 1)) * r }
    d += ` L ${p1.x},${p1.y} Q ${cur.x},${cur.y} ${p2.x},${p2.y}`
  }
  const last = pts[pts.length - 1]
  d += ` L ${last.x},${last.y}`
  return d
}

function RoutedEdge({ sourceX, sourceY, targetX, targetY, markerEnd, style, data, label }: EdgeProps) {
  const waypoints = (data as { points?: Point[] } | undefined)?.points
  const source = { x: sourceX, y: sourceY }
  const target = { x: targetX, y: targetY }
  const path =
    waypoints && waypoints.length >= 2
      ? roundedPath([source, ...waypoints.slice(1, -1), target], CORNER_RADIUS)
      : `M ${sourceX},${sourceY} L ${targetX},${targetY}`
  const mid = waypoints && waypoints.length ? waypoints[Math.floor(waypoints.length / 2)] : { x: (sourceX + targetX) / 2, y: (sourceY + targetY) / 2 }
  return (
    <>
      <BaseEdge path={path} markerEnd={markerEnd} style={style} />
      {label ? (
        <EdgeLabelRenderer>
          <div
            className="brain-edge-label nodrag nopan"
            style={{ transform: `translate(-50%, -50%) translate(${mid.x}px, ${mid.y}px)`, opacity: style?.opacity as number | undefined }}
          >
            {label}
          </div>
        </EdgeLabelRenderer>
      ) : null}
    </>
  )
}

const nodeTypes = {
  brain: BrainGraphNode as unknown as ComponentType<NodeProps>,
  ownerGroup: OwnerGroupNode as unknown as ComponentType<NodeProps>,
}
const edgeTypes = { routed: RoutedEdge }

function flowEdges(edges: BrainGraphEdge[], routes: Map<string, Point[]>, incident: Set<string> | null): FlowEdge[] {
  return edges.map((edge) => {
    const color = EDGE_COLOR[edge.type] ?? 'var(--border-strong)'
    const blocked = edge.status === 'blocked' || edge.type === 'blocks'
    const onPath = !incident || incident.has(edge.source) === true && incident.has(edge.target) === true
    const dimmed = Boolean(incident) && !onPath
    const baseWidth = blocked ? 2.2 : edge.type === 'depends_on' ? 1.8 : 1.4
    return {
      id: edge.id,
      source: edge.source,
      target: edge.target,
      label: edge.label || undefined,
      type: 'routed',
      data: { edgeType: edge.type, points: routes.get(edge.id) },
      // Edges sit above the (z=0) owner rects but below the (z=2) node cards.
      zIndex: blocked ? 3 : 1,
      markerEnd: { type: MarkerType.ArrowClosed, color, width: 16, height: 16 },
      style: {
        stroke: color,
        strokeWidth: onPath && incident ? baseWidth + 0.8 : baseWidth,
        strokeDasharray: EDGE_DASH[edge.type],
        opacity: dimmed ? 0.1 : edge.type === 'external_ref' ? 0.7 : 0.92,
      },
      labelStyle: { fill: 'var(--text-2)', fontSize: 10, fontFamily: 'var(--font-mono)' },
    }
  })
}

function miniMapColor(node: Node) {
  if (node.type === 'ownerGroup') return 'color-mix(in srgb, var(--accent) 26%, var(--bg-3))'
  const data = node.data as unknown as BrainGraphNodeView
  if (data.type === 'approval' || data.status === 'approval_required') return 'var(--warn)'
  if (data.status === 'dead' || data.status === 'error') return 'var(--danger)'
  if (data.status === 'running' || data.status === 'in-progress') return 'var(--ok)'
  if (data.type === 'task') return 'var(--accent)'
  return 'var(--info)'
}

export function BrainGraphCanvas({
  nodes,
  edges,
  owners,
  selectedId,
  selectedOwner,
  onSelectNode,
  onSelectOwner,
  onClearSelection,
}: {
  nodes: BrainGraphNodeView[]
  edges: BrainGraphEdge[]
  owners: BrainGraphOwnerView[]
  selectedId?: string | null
  selectedOwner?: string | null
  onSelectNode: (node: BrainGraphNodeView) => void
  onSelectOwner: (ownerSlug: string) => void
  onClearSelection: () => void
}) {
  const [instance, setInstance] = useState<ReactFlowInstance<FlowNode, FlowEdge> | null>(null)
  const fittedRef = useRef(false)

  // The dagre layout depends only on graph shape, never on selection — so
  // clicking a node neither relayouts nor moves the viewport.
  const baseLayout = useMemo(() => layoutNodes(nodes, edges, owners), [nodes, edges, owners])

  const selectedOwnerId = selectedOwner ? `owner-boundary:${selectedOwner}` : null
  // Selecting a node only marks it selected (highlight ring) — it never dims the
  // other nodes nor reframes the viewport. Clicking a task must keep the whole
  // graph in view; the detail opens in the drawer, the canvas stays put.
  const flowNodes = useMemo(
    () =>
      baseLayout.nodes.map((node) => {
        if (node.type === 'ownerGroup') {
          const selected = node.id === selectedOwnerId
          return node.selected === selected ? node : { ...node, selected }
        }
        const selected = node.id === selectedId
        if (node.selected === selected && !node.className) return node
        return { ...node, selected, className: undefined }
      }),
    [baseLayout.nodes, selectedId, selectedOwnerId],
  )
  const flowEdgeList = useMemo(() => flowEdges(edges, baseLayout.routes, null), [edges, baseLayout.routes])

  useEffect(() => {
    // Fit once when the graph first populates. Never auto-refit afterwards —
    // expanding a node or selecting must not move the viewport out from under
    // the operator. The Controls "fit view" button is the manual reset.
    if (!instance || baseLayout.nodes.length === 0 || fittedRef.current) return
    fittedRef.current = true
    const timer = window.setTimeout(() => instance.fitView({ padding: 0.16, duration: 240 }), 60)
    return () => window.clearTimeout(timer)
  }, [instance, baseLayout.nodes.length])

  const onNodeClick: NodeMouseHandler<FlowNode> = (_event, node) => {
    if (node.type === 'ownerGroup') {
      onSelectOwner((node.data as OwnerGroupData).owner.slug)
      return
    }
    onSelectNode(node.data as BrainGraphNodeView)
  }

  return (
    <div className="brain-canvas">
      <ReactFlow<FlowNode, FlowEdge>
        nodes={flowNodes}
        edges={flowEdgeList}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        onInit={setInstance}
        onNodeClick={onNodeClick}
        onPaneClick={onClearSelection}
        fitView
        fitViewOptions={{ padding: 0.16 }}
        minZoom={0.18}
        maxZoom={1.4}
        nodesDraggable={false}
        nodesConnectable={false}
        elementsSelectable
        proOptions={{ hideAttribution: true }}
      >
        <Background variant={BackgroundVariant.Dots} gap={18} size={1} />
        <Controls showInteractive={false} />
        <MiniMap
          pannable
          zoomable
          nodeColor={miniMapColor}
          nodeStrokeWidth={2}
          maskColor="color-mix(in srgb, var(--bg) 72%, transparent)"
        />
      </ReactFlow>
    </div>
  )
}
