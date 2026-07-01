# Memory Graph Redesign Design

## Summary

Replace the current Memories list/detail page with a graph-first memory visualizer. The first slice uses the existing `@xyflow/react` and `dagre` stack already present in the UI, not a new 3D dependency. This keeps the scope small while proving whether graph navigation is useful for agent memory.

The page still edits the same memory files through the existing `/api/memory` save flow. The data source remains `/api/memory/sources`.

## Goals

- Show one graph node per memory source.
- Draw edges from existing `[[wiki link]]` references in memory source content.
- Replace the current list-first page with graph-first navigation.
- Keep source search and provider filtering.
- Keep the existing `DocEditor`, wiki-link navigation, backlinks, and save behavior.
- Make unavailable or errored sources visible without treating them as editable.

## Non-Goals

- Do not add true 3D rendering in this slice.
- Do not add a new memory API or storage layer.
- Do not infer semantic links with embeddings or LLM calls.
- Do not rewrite the existing editor.

## Current State

`internal/server/ui/src/screens/Memories.tsx` renders a two-pane page:

- left pane: searchable flat source list
- right pane: selected source metadata and `DocEditor`

The component already computes backlinks by scanning `[[label]]` references in memory source content. That same logic can power graph edges.

The query hook `useMemorySources()` calls `/api/memory/sources` and returns `MemorySource[]`.

## Proposed UX

The page becomes a graph canvas with an inspector/editor panel.

- The canvas is the primary surface.
- Each memory source appears as a node.
- Nodes are colored or badged by provider and muted when unavailable.
- Edges represent explicit `[[wiki link]]` references.
- Selecting a node opens the editor panel for that source.
- Search and provider chips filter visible graph nodes.
- When filters hide a linked source, the hidden edge is omitted.
- Empty and loading states reuse the existing page conventions.

The editor panel keeps the existing behavior:

- show source scope, kind, label, path
- show source error if present
- allow edits only when the source is available
- save through `/api/memory`
- invalidate `memory-sources` after save

## Architecture

Add a small graph-shaping layer inside the Memories screen area:

- `buildMemoryGraph(sources, filters)` converts `MemorySource[]` into graph nodes and edges.
- Nodes use source `id` as the graph node id.
- Edges are created by resolving `[[wiki link]]` targets to source labels.
- Duplicate labels resolve to the first matching filtered source, matching the current simple wiki-link behavior.
- Unresolved links are ignored in this first slice.

Use existing UI dependencies:

- `@xyflow/react` for the graph canvas.
- `@dagrejs/dagre` for deterministic layout.
- existing `DocEditor` for editing.
- existing `useMemorySources`, `apiPost`, and `queryClient` for data.

Keep this in the Memories feature area unless the code gets genuinely shared elsewhere.

## Data Flow

1. `useMemorySources()` loads sources from `/api/memory/sources`.
2. Search/provider state filters the sources.
3. `buildMemoryGraph` builds visible nodes and edges.
4. The graph renders via React Flow.
5. Selecting a graph node updates `selected`.
6. The inspector/editor reads the selected source from the full source list.
7. Save posts to `/api/memory` and invalidates `memory-sources`.

## Error Handling

- Loading: render the existing loading skeleton.
- No sources: render the existing empty state.
- No matches: render an empty graph message.
- Source error: show the error in the inspector.
- Unavailable source: render a muted node and non-editable inspector.
- Save failure: let the existing API error path surface the failure.

## Testing

Add the smallest checks that catch the real behavior:

- Unit-test graph shaping for source nodes, wiki-link edges, filters, and unavailable sources.
- Typecheck the UI.
- Run the existing UI build or targeted tests available in the repo.

## Follow-Up: True 3D

Add a true 3D graph only after the 2D graph proves useful. The follow-up should add one focused dependency, likely `three` plus a small graph wrapper, and keep the same graph data model.

The trigger for 3D is concrete need: the 2D graph becomes too dense, spatial exploration is useful, or the page needs an immersive relationship map rather than a readable editor-adjacent graph.
