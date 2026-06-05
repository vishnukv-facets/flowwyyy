# Brief "Current state" — agent-maintained freshness

Date: 2026-06-05
Status: design approved (mechanism: agent-maintained, command-only, terse)

## Problem

`brief.md` is write-once. It's created by `flow add task` (stub) / `flow spawn`
(rewritten from `--prompt`), edited by a human via `flow edit`, but **nothing
refreshes it once the task is running**. All progress lands in `updates/*.md`.
So `brief.md` keeps describing the day-one ask forever — a resumed session or a
human reading the brief sees stale intent, not current reality.

## Goal

Keep the brief current via a **small, agent-maintained "Current state" snapshot
(1–3 lines)**, without disturbing the original ask.

- `updates/*.md` = append-only history (full notes) — already exists, unchanged.
- brief **Current state** = the latest *snapshot*, overwritten in place, terse.

## Non-goals

- Server-side AI summarization (rejected by user).
- UI "stale brief" badges.
- Back-filling existing long-running tasks' briefs (one-off, separate).
- Verbose summaries in the brief — long content belongs in `updates/`.

## Design

### Brief layout

The original sections (What / Why / Where / Done when / Out of scope / Open
questions) stay untouched. One machine-maintained block, delimited by invisible
HTML-comment markers (never render in markdown), is appended once and thereafter
replaced in place:

```markdown
# <task name>
... original What/Why/Scope/Done-when (never auto-touched) ...

<!-- flow:state:start -->
**Current state** · updated 2026-06-05
<1–3 line snapshot: where it stands now + current blocker / next step>
<!-- flow:state:end -->
```

### CLI: `flow update task <slug> --brief-status <text>`

- `<text>` is the snapshot body (markdown, may be multi-line). `--brief-status -`
  reads the body from stdin.
- Locate the `flow:state:start` / `flow:state:end` markers in
  `tasks/<slug>/brief.md` and replace the block (inclusive) with a freshly
  stamped one. If markers are absent, append the block at EOF after a blank line
  (so existing/stub briefs upgrade in place). Everything outside the markers is
  preserved byte-for-byte.
- The binary stamps `· updated <date>` (RFC3339 date); the agent supplies only
  the body.
- Atomic write (temp file + rename). Bumps `task.updated_at` (same as
  `flow edit`), so freshness/sort reflect the refresh.
- Empty body → no-op with a clear message (never write an empty state).
- Unknown slug → clean error (exit 2/1 per existing conventions).

### Skill (SKILL.md)

Add a rule near the progress-notes guidance:

- Two surfaces: `updates/*.md` = append-only history; brief **Current state** =
  a terse 1–3 line snapshot, overwritten.
- Refresh it via `flow update task <slug> --brief-status "..."` at checkpoints:
  scope/understanding shifts, a blocker appears or clears, and before going idle
  or on `flow done`.
- Keep it to a few lines — if it needs more, it's an `updates/` note.

## Testing

Table-driven CLI tests in `internal/app`:

- inserts block when markers absent (appended at EOF, original preserved)
- replaces block when markers present (surrounding content preserved, date
  re-stamped)
- stdin body path (`-`)
- bumps `updated_at`
- empty body → no-op
- unknown slug → error

Rebuild the embedded skill (`flow skill update` picks up SKILL.md after rebuild).

## Out of scope / future

UI freshness indicator; a one-off back-fill command for existing tasks.
