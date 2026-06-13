# Session KB distiller — periodic, idle-gated KB capture from live agent work

**Date:** 2026-06-14
**Status:** design / approved (built)
**Reuses:** the `flow done` close-out KB sweep (`internal/app/done.go`) and the
flow skill's §4.10 KB capture rules.

## 1. Problem

Long-running agent work accumulates durable knowledge (decisions, org/process
facts) that today is only captured at a *terminal* event:

- **Tasks** get a close-out KB sweep on `flow done`.
- **Chats** never call `flow done`, so their knowledge is never swept.

We want KB capture to also happen *mid-flight*, for both **chats** and
**in-progress tasks**, while the agent is still working — without interfering
with the running session and without burning tokens on idle/unchanged work.

## 2. Approach

A server-side background worker — **the KB distiller** — that periodically
**wakes the live session itself** with a §4.10 KB-checkpoint instruction. The
agent already holds the whole conversation in context, so it distills durable
knowledge into `~/.flow/kb/*.md` directly. This **reuses the existing capture
discipline** (the flow skill's §4.10 — the same rules `flow done` applies), not a
parallel mechanism. No separate headless process re-reads the transcript.

Three gates keep it non-intrusive, cheap, and loop-free:

- **Idle gate (never interrupt a working agent):** only wake a session whose
  transcript `.jsonl` has been quiet for >= `idle`. A working agent appends
  continuously (fresh mtime); an idle one waiting at the prompt goes stale. So we
  never inject a checkpoint mid-turn.
- **Activity gate (don't waste tokens):** a per-session cursor (transcript byte
  offset last requested a capture through) means a session is woken only once it
  has >= `minDelta` new bytes. Unchanged sessions cost nothing.
- **Cooldown gate (no self-trigger loop):** the checkpoint turn itself appends to
  the transcript, which would look like fresh activity. A `cooldown` since the
  last capture blocks re-waking so the checkpoint can't re-trigger itself.

Only **live** sessions are woken (`running` / `sharedRunning`); a finished task
is covered by its `flow done` close-out sweep, and we never resurrect a dead
process to scoop KB. Chats stay eligible even when archived (an archived-but-live
chat keeps getting checkpoints — this subsumes "capture on archive" continuously,
until delete stops the session).

## 3. Components

### 3.1 `kb_capture` cursor table (flowdb)
New idempotent table in `schemaDDL` (no migration), keyed by `session_id` so it
unifies tasks and chats: `(session_id PK, slug, kind, cursor INTEGER,
captured_at)`. Helpers `GetKBCaptureCursor` / `UpsertKBCaptureCursor`. The cursor
is "requested through at last wake" (optimistic — we don't parse the agent's
reply); the cooldown makes that safe.

### 3.2 The distiller worker (`internal/server/kb_distill.go`)
Mirrors `livenessReconciler` (interval + ctx cancel + done chan). Per tick:
1. Candidates: `ListTasks{Status:"in-progress"}` (with a session) + `ListChats`
   incl. archived (with a session), each carrying the fields
   `resolveSessionJSONLPath` needs (a synthetic `flowdb.Task` for chats).
2. Skip if not live (`running`/`sharedRunning`).
3. `transcripts.get(path)` -> entries + mtime; `kbShouldWake(now, mtime,
   capturedAt, cursor, maxOffset, minDelta, idle, cooldown)` (pure, tested).
4. If it passes: `wakeTaskForInboxNotify(slug, kbCheckpointPrompt)` then
   `UpsertKBCaptureCursor` (cursor = current max offset, captured_at = now).

`kbCheckpointPrompt` injects the §4.10 checkpoint and instructs the agent to
capture silently and resume (no user-facing reply).

### 3.3 Wiring & config
- Started/stopped alongside the liveness reconciler in `ListenAndServe`.
- `FLOW_KB_DISTILL_ENABLED` (settingBool, default **true**) in the General
  settings group; `FLOW_KB_DISTILL_INTERVAL` / `_IDLE` / `_COOLDOWN` (env;
  defaults 5m / 8m / 30m), `minDelta` 600 bytes.

## 4. Out of scope (v1)
- Sweeping done/backlog tasks (close-out sweep covers done).
- Confirming the agent actually wrote KB (optimistic cursor + cooldown instead).
- Re-mining transcript history older than the cached tail on a very long idle gap.

## 5. Testing
- flowdb: cursor get (missing) / upsert round-trip / advance.
- `kbShouldWake`: active / idle / cooldown / below-delta / no-transcript / exact
  threshold (pure table).
- `maxTranscriptByteOffset`, `kbDistillEnabled` default, checkpoint-prompt reuse.
