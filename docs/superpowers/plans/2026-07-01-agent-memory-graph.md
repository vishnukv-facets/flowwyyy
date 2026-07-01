# Agent Memory Graph Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the Agent Memories flat list/detail page with a graph-first agent memory visualizer while preserving the existing editor and save flow.

**Architecture:** Keep `/api/memory/sources` as the only data source and shape the graph in the UI. Use a pure graph builder for filtering and wiki-link edges, a small React Flow canvas for rendering, and the existing `DocEditor` for edits.

**Tech Stack:** React 18, TypeScript, Vite, `@xyflow/react`, `@dagrejs/dagre`, existing `DocEditor`, existing `useMemorySources`, existing `/api/memory` save API.

## Global Constraints

- These are agent memories, not Flow memories.
- Do not include `memorysrc.FlowKBSources(...)` output in this screen.
- Do not add a new memory API or storage layer.
- Do not add true 3D rendering in this slice.
- Do not infer semantic links with embeddings or LLM calls.
- Do not rewrite `DocEditor`.
- Do not add a graph/rendering dependency; `@xyflow/react` and `@dagrejs/dagre` are already installed.

---

## File Structure

- Create `internal/server/ui/src/lib/wikiRefs.ts`
  - Owns `[[wiki-link]]` extraction as a pure helper.
- Modify `internal/server/ui/src/components/DocEditor.tsx`
  - Re-export `wikiRefs` from the pure helper so existing imports keep working.
- Create `internal/server/ui/src/screens/memoryGraph.ts`
  - Pure graph model: filters agent memory sources and builds nodes/edges.
- Create `internal/server/ui/src/screens/memoryGraph.selfcheck.ts`
  - Runnable self-check for graph shaping without adding a test framework.
- Create `internal/server/ui/src/components/memoryGraph/MemoryGraphNode.tsx`
  - React Flow node card for one agent memory source.
- Create `internal/server/ui/src/components/memoryGraph/MemoryGraphCanvas.tsx`
  - React Flow + dagre canvas for the graph.
- Modify `internal/server/ui/src/screens/Memories.tsx`
  - Replace list/detail layout with graph canvas + inspector/editor.
- Modify `internal/server/ui/src/styles/app.css`
  - Add memory graph layout and node styles.

---

### Task 1: Pure Agent Memory Graph Model

**Files:**
- Create: `internal/server/ui/src/lib/wikiRefs.ts`
- Modify: `internal/server/ui/src/components/DocEditor.tsx`
- Create: `internal/server/ui/src/screens/memoryGraph.ts`
- Create: `internal/server/ui/src/screens/memoryGraph.selfcheck.ts`

**Interfaces:**
- Consumes: `MemorySource` from `internal/server/ui/src/lib/types.ts`.
- Produces:
  - `wikiRefs(content: string): string[]`
  - `buildMemoryGraph(sources: MemorySource[], filters: MemoryGraphFilters): MemoryGraph`
  - `filterMemorySources(sources: MemorySource[], filters: MemoryGraphFilters): MemorySource[]`

- [ ] **Step 1: Move wiki-link extraction into a pure helper**

Create `internal/server/ui/src/lib/wikiRefs.ts`:

```ts
// Extract the [[wiki-link]] target names referenced in a markdown body.
export function wikiRefs(content: string): string[] {
  const out: string[] = []
  const re = /\[\[([^\]\n]+)\]\]/g
  let m: RegExpExecArray | null
  while ((m = re.exec(content || '')) !== null) out.push(m[1].trim().toLowerCase())
  return out
}
```

In `internal/server/ui/src/components/DocEditor.tsx`, replace the existing bottom `wikiRefs` function with:

```ts
export { wikiRefs } from '../lib/wikiRefs'
```

- [ ] **Step 2: Write the graph model**

Create `internal/server/ui/src/screens/memoryGraph.ts`:

```ts
import type { MemorySource } from '../lib/types.js'
import { wikiRefs } from '../lib/wikiRefs.js'

export interface MemoryGraphFilters {
  query: string
  provider: string
}

export interface MemoryGraphNode {
  id: string
  label: string
  provider: string
  scope: string
  kind: string
  path: string
  status: string
  available: boolean
  error?: string
  summary: string
  badges: string[]
}

export interface MemoryGraphEdge {
  id: string
  source: string
  target: string
  label: string
}

export interface MemoryGraph {
  nodes: MemoryGraphNode[]
  edges: MemoryGraphEdge[]
  visibleSources: MemorySource[]
}

function normalized(value: string | undefined) {
  return (value ?? '').trim().toLowerCase()
}

export function filterMemorySources(sources: MemorySource[], filters: MemoryGraphFilters): MemorySource[] {
  const needle = normalized(filters.query)
  return sources.filter((source) => {
    if (filters.provider && source.provider !== filters.provider) return false
    if (!needle) return true
    return (
      normalized(source.label).includes(needle) ||
      normalized(source.path).includes(needle) ||
      normalized(source.scope).includes(needle)
    )
  })
}

export function buildMemoryGraph(sources: MemorySource[], filters: MemoryGraphFilters): MemoryGraph {
  const visibleSources = filterMemorySources(sources, filters)
  const byLabel = new Map<string, MemorySource>()
  for (const source of visibleSources) {
    const key = normalized(source.label)
    if (key && !byLabel.has(key)) byLabel.set(key, source)
  }

  const edges: MemoryGraphEdge[] = []
  const seenEdges = new Set<string>()
  for (const source of visibleSources) {
    for (const ref of wikiRefs(source.content ?? '')) {
      const target = byLabel.get(ref)
      if (!target || target.id === source.id) continue
      const id = `memory-edge:${source.id}->${target.id}:${ref}`
      if (seenEdges.has(id)) continue
      seenEdges.add(id)
      edges.push({ id, source: source.id, target: target.id, label: 'links to' })
    }
  }

  return {
    visibleSources,
    edges,
    nodes: visibleSources.map((source) => ({
      id: source.id,
      label: source.label,
      provider: source.provider,
      scope: source.scope,
      kind: source.kind,
      path: source.path,
      status: source.available ? 'available' : source.error ? 'error' : source.status || 'missing',
      available: source.available,
      error: source.error,
      summary: source.path,
      badges: [source.scope, source.kind].filter(Boolean),
    })),
  }
}
```

- [ ] **Step 3: Add the self-check**

Create `internal/server/ui/src/screens/memoryGraph.selfcheck.ts`:

```ts
import { buildMemoryGraph } from './memoryGraph.js'
import type { MemorySource } from '../lib/types.js'

function source(input: Partial<MemorySource> & Pick<MemorySource, 'id' | 'label'>): MemorySource {
  return {
    id: input.id,
    provider: input.provider ?? 'codex',
    scope: input.scope ?? 'user',
    kind: input.kind ?? 'instructions',
    label: input.label,
    path: input.path ?? `/tmp/${input.id}.md`,
    status: input.status ?? 'available',
    available: input.available ?? true,
    content: input.content ?? '',
    error: input.error,
  }
}

function expect(condition: boolean, message: string) {
  if (!condition) throw new Error(message)
}

const graph = buildMemoryGraph(
  [
    source({ id: 'global', label: 'Global', content: 'See [[Project]] and [[Missing]].' }),
    source({ id: 'project', label: 'Project', scope: 'project', content: 'Back to [[Global]].' }),
    source({ id: 'claude', label: 'Claude', provider: 'claude', available: false, status: 'missing' }),
  ],
  { query: '', provider: '' },
)

expect(graph.nodes.length === 3, 'expected all sources as nodes')
expect(graph.edges.length === 2, 'expected only resolved wiki links as edges')
expect(graph.edges.some((edge) => edge.source === 'global' && edge.target === 'project'), 'expected global -> project edge')
expect(graph.edges.some((edge) => edge.source === 'project' && edge.target === 'global'), 'expected project -> global edge')
expect(graph.nodes.find((node) => node.id === 'claude')?.status === 'missing', 'expected unavailable source status')

const filtered = buildMemoryGraph(graph.visibleSources, { query: 'global', provider: '' })
expect(filtered.nodes.length === 1, 'expected query to filter nodes')
expect(filtered.edges.length === 0, 'expected hidden targets to omit edges')

const providerFiltered = buildMemoryGraph(graph.visibleSources, { query: '', provider: 'claude' })
expect(providerFiltered.nodes.length === 1, 'expected provider filter to keep one node')
expect(providerFiltered.nodes[0]?.id === 'claude', 'expected claude provider node')

console.log('memoryGraph self-check passed')
```

- [ ] **Step 4: Run the self-check and typecheck**

Run:

```bash
cd internal/server/ui
pnpm exec tsc --module NodeNext --moduleResolution NodeNext --target ES2022 --skipLibCheck --outDir /tmp/flow-ui-memory-graph-check src/screens/memoryGraph.ts src/screens/memoryGraph.selfcheck.ts
node /tmp/flow-ui-memory-graph-check/screens/memoryGraph.selfcheck.js
pnpm typecheck
```

Expected:

```text
memoryGraph self-check passed
```

`pnpm typecheck` exits `0`.

- [ ] **Step 5: Commit**

```bash
git add internal/server/ui/src/lib/wikiRefs.ts internal/server/ui/src/components/DocEditor.tsx internal/server/ui/src/screens/memoryGraph.ts internal/server/ui/src/screens/memoryGraph.selfcheck.ts
git commit -m "feat: add agent memory graph model"
```

---

### Task 2: React Flow Canvas

**Files:**
- Create: `internal/server/ui/src/components/memoryGraph/MemoryGraphNode.tsx`
- Create: `internal/server/ui/src/components/memoryGraph/MemoryGraphCanvas.tsx`

**Interfaces:**
- Consumes:
  - `MemoryGraph`
  - `MemoryGraphNode`
- Produces:
  - `MemoryGraphCanvas({ graph, selectedId, onSelectNode, onClearSelection })`

- [ ] **Step 1: Create the node card**

Create `internal/server/ui/src/components/memoryGraph/MemoryGraphNode.tsx`:

```tsx
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
```

- [ ] **Step 2: Create the canvas**

Create `internal/server/ui/src/components/memoryGraph/MemoryGraphCanvas.tsx`:

```tsx
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
```

- [ ] **Step 3: Run typecheck**

Run:

```bash
cd internal/server/ui
pnpm typecheck
```

Expected: exits `0`.

- [ ] **Step 4: Commit**

```bash
git add internal/server/ui/src/components/memoryGraph/MemoryGraphNode.tsx internal/server/ui/src/components/memoryGraph/MemoryGraphCanvas.tsx
git commit -m "feat: add agent memory graph canvas"
```

---

### Task 3: Replace the Memories Page

**Files:**
- Modify: `internal/server/ui/src/screens/Memories.tsx`

**Interfaces:**
- Consumes:
  - `buildMemoryGraph(sources, { query, provider })`
  - `MemoryGraphCanvas`
  - existing `DocEditor`
- Produces:
  - Graph-first Agent Memories page.

- [ ] **Step 1: Replace imports**

Use these imports at the top of `internal/server/ui/src/screens/Memories.tsx`:

```tsx
import { useMemo, useState } from 'react'
import { Brain, Search } from 'lucide-react'
import { useDocumentTitle } from '../lib/title'
import { useMemorySources } from '../lib/query'
import { EMPTY_MEMORY_SOURCES, queryClient, apiPost } from '../lib/api'
import { EmptyState, Loading } from '../components/ui'
import { DocEditor, type Backlink } from '../components/DocEditor'
import { wikiRefs } from '../lib/wikiRefs'
import { MemoryGraphCanvas } from '../components/memoryGraph/MemoryGraphCanvas'
import { buildMemoryGraph } from './memoryGraph'
```

- [ ] **Step 2: Replace the component body**

Replace the `Memories` component with:

```tsx
export function Memories() {
  useDocumentTitle('Memories')
  const { data, isLoading } = useMemorySources()
  const sources = data ?? EMPTY_MEMORY_SOURCES
  const [selected, setSelected] = useState<string | null>(null)
  const [q, setQ] = useState('')
  const [provider, setProvider] = useState('')

  const providers = useMemo(() => {
    const seen = new Set<string>()
    for (const source of sources) {
      if (source.provider) seen.add(source.provider)
    }
    return Array.from(seen).sort()
  }, [sources])

  const graph = useMemo(() => buildMemoryGraph(sources, { query: q, provider }), [sources, q, provider])
  const selectedId = selected && graph.nodes.some((node) => node.id === selected) ? selected : graph.nodes[0]?.id ?? null
  const active = selectedId ? sources.find((source) => source.id === selectedId) : undefined

  const backlinks = useMemo<Backlink[]>(() => {
    if (!active) return []
    const name = active.label.trim().toLowerCase()
    return sources.flatMap((source) =>
      source.id !== active.id && wikiRefs(source.content ?? '').includes(name)
        ? [{ name: source.label, onOpen: () => setSelected(source.id) }]
        : [],
    )
  }, [sources, active])

  if (isLoading) return <div className="page"><Loading rows={5} /></div>
  if (sources.length === 0) {
    return (
      <div className="page">
        <EmptyState icon={<Brain size={30} />} title="No agent memory sources" hint="Agent instruction files surface here." />
      </div>
    )
  }

  const saveActive = async (text: string, version?: string) => {
    if (!active) return
    await apiPost('/api/memory', { path: active.path, text, mtime: version })
    await queryClient.invalidateQueries({ queryKey: ['memory-sources'] })
  }

  const onWikiLink = (target: string) => {
    const t = target.toLowerCase()
    const hit = sources.find((source) => source.label.trim().toLowerCase() === t)
    if (hit) setSelected(hit.id)
  }

  return (
    <div className="page flush">
      <div className="memory-graph-page">
        <div className="memory-graph-toolbar">
          <div>
            <div className="eyebrow">agent memory</div>
            <div className="h-lg">
              {graph.nodes.length}
              {graph.nodes.length !== sources.length ? ` / ${sources.length}` : ''} sources
            </div>
          </div>
          <div className="memory-graph-filters">
            <div className="input-icon">
              <Search size={14} className="dim" />
              <input
                className="input"
                aria-label="Filter agent memory sources"
                placeholder="Filter by name, path, or scope..."
                value={q}
                onChange={(event) => setQ(event.target.value)}
              />
            </div>
            {providers.length > 1 ? (
              <div className="chips">
                <button type="button" className={`chip${provider === '' ? ' active' : ''}`} onClick={() => setProvider('')}>
                  all
                </button>
                {providers.map((p) => (
                  <button
                    type="button"
                    key={p}
                    className={`chip${provider === p ? ' active' : ''}`}
                    onClick={() => setProvider((cur) => (cur === p ? '' : p))}
                  >
                    {p}
                  </button>
                ))}
              </div>
            ) : null}
          </div>
        </div>

        <div className="memory-graph-shell">
          <div className="memory-graph-main">
            {graph.nodes.length === 0 ? (
              <div className="memory-graph-empty">No agent memory sources match.</div>
            ) : (
              <MemoryGraphCanvas
                graph={graph}
                selectedId={selectedId}
                onSelectNode={(node) => setSelected(node.id)}
                onClearSelection={() => setSelected(null)}
              />
            )}
          </div>
          <aside className="memory-graph-inspector">
            {active ? (
              <div className="memory-graph-inspector-inner">
                <div className="eyebrow" style={{ marginBottom: 4 }}>{active.scope} · {active.kind}</div>
                <div className="detail-title" style={{ fontSize: 19 }}>{active.label}</div>
                <div className="detail-ref" style={{ marginBottom: 16 }}>{active.path}</div>
                {active.error ? (
                  <div className="error-note">{active.error}</div>
                ) : active.available ? (
                  <DocEditor
                    key={active.id}
                    content={active.content ?? ''}
                    version={active.mtime}
                    onSave={saveActive}
                    onWikiLink={onWikiLink}
                    backlinks={backlinks}
                  />
                ) : (
                  <div className="faint">This agent memory source is unavailable for editing.</div>
                )}
              </div>
            ) : (
              <div className="memory-graph-inspector-empty">Select an agent memory source.</div>
            )}
          </aside>
        </div>
      </div>
    </div>
  )
}
```

- [ ] **Step 3: Run typecheck**

Run:

```bash
cd internal/server/ui
pnpm typecheck
```

Expected: exits `0`.

- [ ] **Step 4: Commit**

```bash
git add internal/server/ui/src/screens/Memories.tsx
git commit -m "feat: replace memories list with graph view"
```

---

### Task 4: Styles and Responsive Layout

**Files:**
- Modify: `internal/server/ui/src/styles/app.css`

**Interfaces:**
- Consumes: class names from Task 2 and Task 3.
- Produces: stable desktop and mobile layout for the agent memory graph page.

- [ ] **Step 1: Add styles**

Append this near the existing graph styles in `internal/server/ui/src/styles/app.css`:

```css
.memory-graph-page {
  height: calc(100vh - var(--topbar-h, 0px));
  min-height: 0;
  display: grid;
  grid-template-rows: auto 1fr;
  background: var(--bg);
}
.memory-graph-toolbar {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  padding: 16px 20px;
  border-bottom: 1px solid var(--border);
  background: color-mix(in srgb, var(--bg-2) 82%, var(--bg));
}
.memory-graph-filters {
  width: min(520px, 48vw);
  display: grid;
  gap: 8px;
}
.memory-graph-shell {
  min-height: 0;
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(340px, 420px);
}
.memory-graph-main {
  min-width: 0;
  min-height: 0;
  position: relative;
  border-right: 1px solid var(--border);
}
.memory-graph-canvas {
  width: 100%;
  height: 100%;
  min-height: 0;
  background: var(--bg);
}
.memory-graph-canvas .react-flow {
  font-family: var(--font-sans);
}
.memory-graph-canvas .react-flow__controls,
.memory-graph-canvas .react-flow__minimap {
  border: 1px solid var(--border);
  border-radius: var(--r-sm);
  overflow: hidden;
  background: var(--bg-2);
  box-shadow: var(--shadow-card);
}
.memory-graph-canvas .react-flow__controls-button {
  width: 28px;
  height: 28px;
  color: var(--text-2);
  background: var(--bg-2);
  border-bottom: 1px solid var(--border);
}
.memory-graph-canvas .react-flow__controls-button:hover {
  color: var(--text);
  background: var(--surface-hover);
}
.memory-graph-canvas .react-flow__controls-button svg {
  fill: currentColor;
}
.memory-graph-canvas .react-flow__minimap {
  width: 138px;
  height: 94px;
  background: color-mix(in srgb, var(--bg-2) 88%, transparent);
}
.memory-node {
  width: 264px;
  min-height: 126px;
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 10px 11px;
  border: 1px solid var(--border);
  border-radius: var(--r-sm);
  background: color-mix(in srgb, var(--bg-2) 94%, var(--bg));
  color: var(--text);
  box-shadow: var(--shadow-card);
  overflow: hidden;
}
.memory-node.selected {
  border-color: var(--accent-line);
  box-shadow:
    0 0 0 2px var(--accent-soft),
    var(--shadow-card);
}
.memory-node.muted {
  opacity: 0.72;
  background: color-mix(in srgb, var(--bg-3) 58%, var(--bg-2));
}
.memory-node-handle {
  width: 7px;
  height: 7px;
  border: 1px solid var(--border-strong);
  background: var(--bg-3);
  opacity: 0;
}
.memory-node-top,
.memory-node-meta {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
}
.memory-node-top {
  justify-content: space-between;
}
.memory-node-title {
  min-width: 0;
  font-size: 13.5px;
  font-weight: 650;
  line-height: 1.25;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.memory-node-path {
  min-height: 32px;
  color: var(--text-3);
  font-family: var(--font-mono);
  font-size: 10.5px;
  line-height: 1.35;
  overflow: hidden;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
}
.memory-node-chip {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  max-width: 118px;
  height: 22px;
  padding: 0 7px;
  border: 1px solid var(--border-faint);
  border-radius: var(--r-pill);
  background: var(--bg-3);
  color: var(--text-2);
  font-family: var(--font-mono);
  font-size: 10.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.memory-node-warn {
  color: var(--danger);
}
.memory-graph-inspector {
  min-width: 0;
  min-height: 0;
  overflow: auto;
  background: var(--bg);
}
.memory-graph-inspector-inner {
  padding: 24px 28px;
  max-width: 820px;
}
.memory-graph-empty,
.memory-graph-inspector-empty {
  height: 100%;
  display: grid;
  place-items: center;
  padding: 24px;
  color: var(--text-3);
  font-size: 13px;
}

@media (max-width: 900px) {
  .memory-graph-page {
    height: auto;
    min-height: calc(100vh - var(--topbar-h, 0px));
  }
  .memory-graph-toolbar {
    display: grid;
  }
  .memory-graph-filters {
    width: 100%;
  }
  .memory-graph-shell {
    grid-template-columns: 1fr;
    grid-template-rows: minmax(420px, 56vh) auto;
  }
  .memory-graph-main {
    border-right: 0;
    border-bottom: 1px solid var(--border);
  }
  .memory-graph-inspector {
    min-height: 320px;
  }
  .memory-graph-inspector-inner {
    max-width: none;
    padding: 18px 16px;
  }
}
```

- [ ] **Step 2: Build**

Run:

```bash
cd internal/server/ui
pnpm build
```

Expected: exits `0`.

- [ ] **Step 3: Commit**

```bash
git add internal/server/ui/src/styles/app.css
git commit -m "style: add agent memory graph layout"
```

---

### Task 5: Final Verification

**Files:**
- No new files.

**Interfaces:**
- Consumes: all previous tasks.
- Produces: verified graph-first Agent Memories page.

- [ ] **Step 1: Run self-check**

Run:

```bash
cd internal/server/ui
pnpm exec tsc --module NodeNext --moduleResolution NodeNext --target ES2022 --skipLibCheck --outDir /tmp/flow-ui-memory-graph-check src/screens/memoryGraph.ts src/screens/memoryGraph.selfcheck.ts
node /tmp/flow-ui-memory-graph-check/screens/memoryGraph.selfcheck.js
```

Expected:

```text
memoryGraph self-check passed
```

- [ ] **Step 2: Run UI typecheck**

Run:

```bash
cd internal/server/ui
pnpm typecheck
```

Expected: exits `0`.

- [ ] **Step 3: Run UI build**

Run:

```bash
cd internal/server/ui
pnpm build
```

Expected: exits `0`.

- [ ] **Step 4: Smoke-test locally**

Run:

```bash
cd internal/server/ui
pnpm dev --host 127.0.0.1
```

Open `/memories` and verify:

- the page title remains `Memories`
- the heading says `agent memory`
- no copy says `flow memories`
- nodes appear for agent memory sources
- selecting a node opens the existing editor panel
- unavailable sources are visible and non-editable
- search filters nodes and hides edges to filtered-out nodes
- provider chips filter nodes

- [ ] **Step 5: Commit verification note if any fix was needed**

If Task 5 finds a defect and a fix is made:

```bash
git add internal/server/ui/src
git commit -m "fix: verify agent memory graph"
```

If Task 5 finds no defect, do not create an empty commit.
