import { useMemo, useReducer } from 'react'
import { AlertTriangle, Boxes } from 'lucide-react'
import { BrainGraphCanvas } from '../components/brainGraph/BrainGraphCanvas'
import { BrainGraphInspector } from '../components/brainGraph/BrainGraphInspector'
import { BrainGraphLegend } from '../components/brainGraph/BrainGraphLegend'
import { BrainGraphToolbar } from '../components/brainGraph/BrainGraphToolbar'
import { EmptyState, ErrorNote, Loading } from '../components/ui'
import { useBrainGraph } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import type { BrainGraphNode } from '../lib/types'

function nodeOwnerSlug(node: BrainGraphNode, nodes: BrainGraphNode[]) {
  if (node.owner_slug) return node.owner_slug
  if (!node.task_slug) return 'unowned'
  const taskNode = nodes.find((candidate) =>
    candidate.type === 'task' && (candidate.task_slug === node.task_slug || candidate.id === `task:${node.task_slug}`),
  )
  return taskNode?.owner_slug || 'unowned'
}

interface BrainGraphState {
  q: string
  includeDone: boolean
  expanded: Set<string>
  selectedId: string | null
  selectedOwner: string | null
  // warningsOpen surfaces graph-wide warnings in the drawer when no node is
  // selected (the drawer is otherwise driven by node selection).
  warningsOpen: boolean
}

type BrainGraphAction =
  | { type: 'query'; q: string }
  | { type: 'includeDone'; includeDone: boolean }
  | { type: 'selectNode'; id: string; ownerSlug: string; expandTask: boolean }
  | { type: 'selectOwner'; ownerSlug: string }
  | { type: 'viewWarnings' }
  | { type: 'clearSelection' }

const initialBrainGraphState: BrainGraphState = {
  q: '',
  includeDone: false,
  expanded: new Set(),
  selectedId: null,
  selectedOwner: null,
  warningsOpen: false,
}

function brainGraphReducer(state: BrainGraphState, action: BrainGraphAction): BrainGraphState {
  switch (action.type) {
    case 'query':
      return { ...state, q: action.q }
    case 'includeDone':
      return { ...state, includeDone: action.includeDone }
    case 'selectOwner':
      return { ...state, selectedId: null, selectedOwner: action.ownerSlug, warningsOpen: false }
    case 'viewWarnings':
      return { ...state, selectedId: null, warningsOpen: true }
    case 'clearSelection':
      return { ...state, selectedId: null, selectedOwner: null, warningsOpen: false }
    case 'selectNode': {
      const expanded =
        action.expandTask && !state.expanded.has(action.id)
          ? new Set([...state.expanded, action.id])
          : state.expanded
      return { ...state, selectedId: action.id, selectedOwner: action.ownerSlug, warningsOpen: false, expanded }
    }
  }
}

export function BrainGraph() {
  useDocumentTitle('Graph')
  const [state, dispatch] = useReducer(brainGraphReducer, initialBrainGraphState)
  const { q, includeDone, expanded, selectedId, selectedOwner, warningsOpen } = state
  const expand = useMemo(() => [...expanded].sort(), [expanded])
  const { data, isLoading, error, isFetching } = useBrainGraph({ q, includeDone, expand })

  const selected = useMemo(
    () => data?.nodes.find((node) => node.id === selectedId) ?? null,
    [data?.nodes, selectedId],
  )

  const selectNode = (node: BrainGraphNode) => {
    // Selecting a task only opens the drawer — it must NOT auto-expand the task.
    // Expanding refetched the graph with the task's run nodes, which relayouts
    // the whole owner subgraph (every node moves), and at a zoomed-in view the
    // other tasks slide out of sight — that's the "tasks vanish on click" report.
    dispatch({
      type: 'selectNode',
      id: node.id,
      ownerSlug: data ? nodeOwnerSlug(node, data.nodes) : node.owner_slug || 'unowned',
      expandTask: false,
    })
  }

  return (
    <div className="page brain-page">
      <BrainGraphToolbar
        counts={data?.counts}
        q={q}
        includeDone={includeDone}
        expandedCount={expanded.size}
        onQ={(next) => dispatch({ type: 'query', q: next })}
        onIncludeDone={(next) => dispatch({ type: 'includeDone', includeDone: next })}
      />

      {isLoading ? (
        <Loading label="loading graph" />
      ) : error ? (
        <ErrorNote error={error} />
      ) : !data || data.nodes.length === 0 ? (
        <EmptyState icon={<Boxes size={30} />} title="No graph nodes" hint="No visible Brain graph nodes match the current filters." />
      ) : (
        <div className="brain-shell">
          <div className="brain-main">
            <div className="brain-surface">
              <div className="brain-surface-head">
                <div className="brain-freshness">
                  <span className={`dot ${isFetching ? 'waiting' : 'done'}`} />
                  {isFetching ? 'refreshing' : data.freshness}
                </div>
                {data.warnings.length > 0 ? (
                  <button
                    type="button"
                    className="brain-warning-pill"
                    onClick={() => dispatch({ type: 'viewWarnings' })}
                    title="Review graph warnings"
                  >
                    <AlertTriangle size={13} />
                    {data.warnings.length} warning{data.warnings.length === 1 ? '' : 's'}
                  </button>
                ) : null}
              </div>
              <BrainGraphCanvas
                nodes={data.nodes}
                edges={data.edges}
                owners={data.owners}
                selectedId={selected?.id ?? null}
                selectedOwner={selectedOwner}
                onSelectNode={selectNode}
                onSelectOwner={(ownerSlug) => {
                  dispatch({ type: 'selectOwner', ownerSlug })
                }}
                onClearSelection={() => dispatch({ type: 'clearSelection' })}
              />
            </div>
            <BrainGraphLegend />
          </div>

          <BrainGraphInspector
            open={Boolean(selected) || warningsOpen}
            selected={selected}
            actions={data.selected_actions}
            warnings={data.warnings}
            onClose={() => dispatch({ type: 'clearSelection' })}
          />
        </div>
      )}
    </div>
  )
}
