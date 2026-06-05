# Attention Router — P1.4b Feed UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

> ⚠️ **Repo rule:** work on branch `flow/attention-router-p1.1`; per-task commits pre-approved. Never commit to `main`.

> 🎨 **Verification honesty:** UI correctness is partly *visual*. The gate a subagent enforces is `pnpm typecheck && pnpm build` (compiles + builds cleanly) PLUS faithful reuse of the existing design system. The *visual* review — does it look right and match the operator-console aesthetic — is the OPERATOR's, via `make ui` + browser. Do NOT claim it "looks right" — claim it "compiles and reuses the real primitives."

**Goal:** Add the **Attention feed screen** to Mission Control — a `/attention` route showing surfaced candidates as cards with Make-task / Forward / Dismiss actions and a status filter, plus a sidebar nav entry with an unread-count badge. It consumes the P1.4a endpoints (`GET /api/attention`, `attention-act` action).

**Architecture:** Pure additive React/TS, matching the existing dark operator-console design system. New `screens/Attention.tsx` follows the `screens/Inbox.tsx` skeleton; data via a new `useAttention()` React Query hook (`apiGet<AttentionItem[]>('/api/attention')`); mutations via the existing `useAction()` (`action.mutate({kind:'attention-act', target, attention_action})`); auto-refresh via the existing server-event invalidation (no new wiring). Route added in `app.tsx`; nav entry + badge in `Shell.tsx`. New CSS in `app.css` using `tokens.css` variables and existing class conventions (`.card`, `.btn`, `.badge`, `.eyebrow`).

**Tech Stack:** Vite + React + TS, `wouter` router, `@tanstack/react-query`, `lucide-react` icons, `pnpm`. No new dependencies. Build/typecheck from `internal/server/ui/`.

**Out of scope (→ P1.4c):** the Slack channel checkbox multi-select (Settings is registry-driven; watched-channels is already settable via the P1.4a `FLOW_STEERING_WATCH_CHANNELS` registry string input in the interim) and rich desktop-push for urgent items (the nav badge surfaces the count for now).

**Builds on:** P1.4a endpoints; UI patterns — `useAction`/`apiGet` (lib/query.ts:258, lib/api.ts:21), `useQuery` (lib/query.ts:196), `ActionRequest` (lib/types.ts:543), `NavDef`/`groups` (Shell.tsx:38/133), `<Switch>` (app.tsx:25), ui primitives `EmptyState`/`Loading`/`ErrorNote`/`SourceIcon` (components/ui.tsx), tokens (styles/tokens.css), classes (base.css/app.css).

---

## File Structure

| File | Change |
|---|---|
| `internal/server/ui/src/lib/types.ts` (modify) | Add `AttentionItem` interface; add `attention_action?` to `ActionRequest`. |
| `internal/server/ui/src/lib/query.ts` (modify) | Add `useAttention(status?)` hook. |
| `internal/server/ui/src/screens/Attention.tsx` (create) | The feed screen: status filter, cards, action buttons, empty/loading/error. |
| `internal/server/ui/src/app.tsx` (modify) | Import `Attention`; add `<Route path="/attention" component={Attention} />`. |
| `internal/server/ui/src/components/Shell.tsx` (modify) | `useAttention('new')` count + a `/attention` `NavDef` with badge. |
| `internal/server/ui/src/styles/app.css` (modify) | `.att-*` styles using tokens. |

---

## Task 1: Types + query hook

**Files:**
- Modify: `internal/server/ui/src/lib/types.ts`, `internal/server/ui/src/lib/query.ts`

- [ ] **Step 1: Add the `AttentionItem` type** — append to `internal/server/ui/src/lib/types.ts`:

```ts
export interface AttentionItem {
  id: string
  source: string
  thread_key: string
  summary: string
  suggested_action: string
  matched_task?: string
  suggested_project?: string
  suggested_priority?: string
  urgency?: string
  is_vip: boolean
  confidence: number
  draft?: string
  reason?: string
  status: string
  created_at: string
  acted_at?: string
}
```

- [ ] **Step 2: Add the `attention_action` field to `ActionRequest`** — in the same file, add this field to the existing `ActionRequest` interface (around line 543):

```ts
  attention_action?: string
```

- [ ] **Step 3: Add the `useAttention` hook** — in `internal/server/ui/src/lib/query.ts`:

First ensure `AttentionItem` is imported from `./types` (add it to the existing `import type { ... } from './types'` line). Then add (near the other read hooks like `useInbox`):

```ts
export function useAttention(status: string = 'new') {
  const q = status ? `?status=${encodeURIComponent(status)}` : ''
  return useQuery({
    queryKey: ['attention', status],
    queryFn: () => apiGet<AttentionItem[]>(`/api/attention${q}`),
  })
}
```

- [ ] **Step 4: Typecheck**

Run (from `internal/server/ui/`): `pnpm typecheck`
Expected: no errors. (The new hook + type are unused until Task 2, but exported declarations don't trip `tsc`.)

- [ ] **Step 5: Commit**

```bash
git add internal/server/ui/src/lib/types.ts internal/server/ui/src/lib/query.ts
git commit -m "feat(ui): AttentionItem type + useAttention query hook

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Attention feed screen + nav + route + styles

**Files:**
- Create: `internal/server/ui/src/screens/Attention.tsx`
- Modify: `internal/server/ui/src/app.tsx`, `internal/server/ui/src/components/Shell.tsx`, `internal/server/ui/src/styles/app.css`

- [ ] **Step 1: Create the screen** — `internal/server/ui/src/screens/Attention.tsx`

```tsx
import { useState } from 'react'
import { Check, ListPlus, Share2 } from 'lucide-react'
import { useAction, useAttention } from '../lib/query'
import { useDocumentTitle } from '../lib/useDocumentTitle'
import { EmptyState, ErrorNote, Loading, SourceIcon } from '../components/ui'
import type { AttentionItem } from '../lib/types'

const STATUSES = ['new', 'acted', 'dismissed', 'all'] as const

export function Attention() {
  useDocumentTitle('Attention')
  const [status, setStatus] = useState<string>('new')
  const { data, isLoading, error } = useAttention(status)
  const action = useAction()

  const act = (item: AttentionItem, verb: string) => {
    if (action.isPending) return
    action.mutate({ kind: 'attention-act', target: item.id, attention_action: verb })
  }

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <div className="eyebrow">attention</div>
          <h1 className="h-xl">Attention Feed</h1>
        </div>
        <div className="spacer" />
        <div className="row gap">
          {STATUSES.map((s) => (
            <button
              key={s}
              type="button"
              className={`btn sm ${status === s ? 'primary' : 'ghost'}`}
              onClick={() => setStatus(s)}
            >
              {s}
            </button>
          ))}
        </div>
      </div>

      {isLoading ? (
        <Loading label="loading attention feed" />
      ) : error ? (
        <ErrorNote error={error} />
      ) : (data ?? []).length === 0 ? (
        <EmptyState
          title="Nothing needs you"
          hint="The steerer surfaces messages worth your attention here — from watched channels, DMs, and mentions."
        />
      ) : (
        <div className="att-list">
          {(data ?? []).map((it) => (
            <AttentionCard key={it.id} item={it} disabled={action.isPending} onAct={act} />
          ))}
        </div>
      )}
    </div>
  )
}

function AttentionCard({
  item,
  disabled,
  onAct,
}: {
  item: AttentionItem
  disabled: boolean
  onAct: (item: AttentionItem, verb: string) => void
}) {
  const urgent = item.urgency === 'urgent'
  return (
    <div className={`card att-card${urgent ? ' att-urgent' : ''}`}>
      <div className="att-head row gap">
        <SourceIcon source={item.source} />
        <span className="badge accent">{item.suggested_action.replace(/_/g, ' ')}</span>
        {item.urgency ? <span className={`badge ${urgent ? 'warn' : ''}`}>{item.urgency}</span> : null}
        {item.is_vip ? <span className="badge info">vip</span> : null}
        <span className="spacer" />
        <span className="num faint" title="confidence">{Math.round(item.confidence * 100)}%</span>
      </div>

      <div className="att-summary">{item.summary || <span className="faint">(no summary)</span>}</div>
      {item.reason ? <div className="att-reason dim">{item.reason}</div> : null}
      {item.matched_task ? <div className="att-meta mono faint">→ {item.matched_task}</div> : null}

      {item.draft ? (
        <div className="att-draft">
          <div className="eyebrow">drafted reply</div>
          <div className="att-draft-body">{item.draft}</div>
        </div>
      ) : null}

      {item.status === 'new' ? (
        <div className="att-actions row gap">
          <button type="button" className="btn primary sm" disabled={disabled} onClick={() => onAct(item, 'make-task')}>
            <ListPlus size={13} /> Make task
          </button>
          {item.matched_task ? (
            <button type="button" className="btn sm" disabled={disabled} onClick={() => onAct(item, 'forward')}>
              <Share2 size={13} /> Forward
            </button>
          ) : null}
          <button type="button" className="btn ghost sm" disabled={disabled} onClick={() => onAct(item, 'dismiss')}>
            <Check size={13} /> Dismiss
          </button>
        </div>
      ) : (
        <div className="att-resolved faint mono">
          {item.status}
          {item.acted_at ? ` · ${item.acted_at.slice(0, 10)}` : ''}
        </div>
      )}
    </div>
  )
}
```

> **Icon note:** `Check` is definitely in lucide-react; `ListPlus` and `Share2` are standard lucide icons but VERIFY they import cleanly (typecheck will fail if a name is wrong). If either is missing in the installed version, substitute a valid lucide icon (e.g. `Plus`, `Share`, `ArrowRight`) — do not invent names.

- [ ] **Step 2: Add the route** — in `internal/server/ui/src/app.tsx`:

Add the import near the other screen imports:
```tsx
import { Attention } from './screens/Attention'
```
Add the route inside `<Switch>`, immediately before the catch-all `<Route>`:
```tsx
<Route path="/attention" component={Attention} />
```

- [ ] **Step 3: Add the nav entry + badge** — in `internal/server/ui/src/components/Shell.tsx`:

Ensure `useAttention` is imported from `../lib/query` (add to the existing query import). Inside the `Shell` component body, near the other data hooks, add:
```tsx
const { data: attentionItems } = useAttention('new')
const attentionCount = attentionItems?.length ?? 0
```
Then add this `NavDef` to the `items` array of the `'Workspace'` group (alongside the Inbox entry). `Bell` is already imported in Shell.tsx:
```tsx
{
  to: '/attention',
  label: 'Attention',
  icon: <Bell size={16} />,
  match: (p) => p === '/attention',
  badge: attentionCount || undefined,
  tone: 'var(--warn)',
},
```

- [ ] **Step 4: Add styles** — append to `internal/server/ui/src/styles/app.css`:

```css
/* Attention feed */
.att-list {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.att-card {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 14px 16px;
}
.att-card.att-urgent {
  border-left: 2px solid var(--warn);
}
.att-summary {
  font-size: 14px;
  line-height: 1.45;
  color: var(--text);
}
.att-reason {
  font-size: 12.5px;
  line-height: 1.4;
}
.att-meta {
  font-size: 11.5px;
}
.att-draft {
  margin-top: 2px;
  padding: 10px 12px;
  background: var(--bg-1);
  border: 1px solid var(--border);
  border-radius: var(--r-sm);
}
.att-draft-body {
  margin-top: 5px;
  font-size: 13px;
  line-height: 1.5;
  color: var(--text-2);
  white-space: pre-wrap;
}
.att-actions {
  margin-top: 4px;
}
.att-resolved {
  margin-top: 2px;
  font-size: 11.5px;
}
```

- [ ] **Step 5: Typecheck + build**

Run (from `internal/server/ui/`):
```
pnpm typecheck && pnpm build
```
Expected: typecheck passes (0 errors), build succeeds. If `node_modules` is missing, run `pnpm install` first. The build writes to the gitignored `internal/server/static/assets/` — do NOT stage that (it's not tracked; `git check-ignore internal/server/static/assets` confirms).

- [ ] **Step 6: Commit (source only — built bundle is gitignored)**

```bash
git add internal/server/ui/src/screens/Attention.tsx internal/server/ui/src/app.tsx internal/server/ui/src/components/Shell.tsx internal/server/ui/src/styles/app.css
git status --short   # confirm NO internal/server/static/assets entries are staged
git commit -m "feat(ui): Attention feed screen + nav badge

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 7: Operator visual-review note (do not run; report for the operator)**

```
Visual review (operator): `make ui && make build && ./flow ui serve`, open the UI, click "Attention" in the sidebar.
Seed a row to see it render:
  sqlite3 ~/.flow/flow.db "INSERT INTO attention_feed (id,source,thread_key,summary,suggested_action,urgency,confidence,status,created_at) VALUES ('demo1','slack','C1:1.1','Customer asks for the rollout date','make_task','urgent',0.9,'new','2026-06-05T10:00:00Z');"
Confirm: card renders with the urgent left-border, the make-task/dismiss buttons work (dismiss → row leaves 'new', badge decrements), the status filter switches lists, and the aesthetic matches the rest of Mission Control (dark cards, indigo accent, mono labels).
```

---

## Self-Review

**1. Spec coverage (P1.4b scope):**
- §7/§11 view the feed in Mission Control → `screens/Attention.tsx` + `/attention` route + nav badge. ✅
- §8 act on items from the UI → `useAction({kind:'attention-act', target, attention_action})` (make-task/forward/dismiss), reusing the P1.4a action. ✅
- live refresh → existing server-event invalidation (no new wiring). ✅
- *Deferred to P1.4c (correct, noted):* the Slack channel checkbox multi-select (registry string input bridges) and rich desktop-push for urgent items (nav badge surfaces count).

**2. Placeholder scan:** No TBD/TODO. The icon note (verify `ListPlus`/`Share2`) and the operator visual-review note are explicit instructions, not placeholders. Every step has complete code.

**3. Type/contract consistency:**
- `AttentionItem` fields ↔ the P1.4a `AttentionItemView` JSON tags (id, source, thread_key, summary, suggested_action, matched_task, suggested_project, suggested_priority, urgency, is_vip, confidence, draft, reason, status, created_at, acted_at) — exact match. ✅
- `ActionRequest.attention_action?` ↔ the P1.4a `actionRequest.AttentionAction json:"attention_action"`. ✅
- Action verbs `make-task`/`forward`/`dismiss` ↔ the P1.4a `attentionAct` switch cases. ✅
- `useAttention(status)` queryKey `['attention', status]` — Shell uses `'new'`, screen uses its filter; React Query dedupes on the shared `['attention','new']` key. ✅
- Reused primitives `EmptyState{title,hint}`, `Loading{label}`, `ErrorNote{error}`, `SourceIcon{source}` — match components/ui.tsx signatures. ✅
- `NavDef{to,label,icon,match,badge?,tone?}` literal matches Shell.tsx:38. ✅
- Build gate: `pnpm typecheck && pnpm build`; built bundle gitignored (commit source only). ✅

No unresolved issues.

---

## After P1.4b

Mission Control shows the Attention feed and lets you action items — P1 is visibly usable end to end (pending your visual review). **P1.4c** (optional polish): the Slack channel checkbox multi-select (custom section, reading `/api/slack/channels` + current settings, saving `FLOW_STEERING_WATCH_CHANNELS`) and desktop-push for urgent items via the existing `NotificationsBell`/`Notification` path. Then **P2** turns autonomy on (per-action gate + thresholds UI, `waiting_on` auto-resolution, presence-aware AFK), **P3** the intelligence layer, **P4** more connectors + the confirm-handoff enabler.

## Execution Handoff

Plan complete. Execute subagent-driven (recommended) or inline?
