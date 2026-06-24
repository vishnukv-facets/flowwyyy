# Upstream delta — flowwyyy vs official Facets-cloud/flow

- **Date:** 2026-06-23
- **Upstream snapshot:** `Facets-cloud/flow` @ `f59f6a8` ("feat: flow stats — local usage & ROI analytics (#80)"), module `flow`, go 1.25, 12 internal packages.
- **flowwyyy:** @ `main`, 26 internal packages.
- **Method:** read-only shallow clone of upstream into scratch; structural diff (commands, packages, schema, shared-file divergence). No remote added to the flowwyyy repo, no merge.

## Conclusion (read first)

flowwyyy's **core** has diverged far from official flow, **bidirectionally**. Depending on *current* official flow would **break the "same experience" requirement**: official flow lacks ~8 core commands flowwyyy relies on, 6 core tables flowwyyy reads, and the shared commands (`do`/`update`/`show`/`done`) are substantially rewritten in flowwyyy. Upstream has separately added `stats`/`card` that flowwyyy lacks.

**Therefore:** the "flowwyyy depends on official flow" end-state is gated on a **real upstreaming project**, not a small Phase-3 task. The two-binary architecture (Phases 1–2) runs against a **local** core extracted from flowwyyy's current code and is unaffected — that's the de-risking path. The dependency switch (Phase 3) needs a **strategy decision** (below) before it can be scheduled.

## Strategy decision required (CTO/owner)

Because it's a two-way fork, there are two routes to "official flow as the dependency":

- **Route A — flowwyyy's core becomes official flow (recommended).** flowwyyy's core is *ahead* (richer sessions/auto/deps + 8 extra commands). Extract it and make it the basis of `Facets-cloud/flow` (Facets owns the repo), cherry-picking upstream's `stats`/`card` if desired. Phase 3 = "publish our core as official flow." Avoids a bidirectional merge.
- **Route B — reconcile flowwyyy to current upstream.** Treat current `Facets-cloud/flow` as authoritative; port flowwyyy's 8 commands + 6 tables + column/logic deltas onto upstream's (separately-evolved) core, resolving conflicts in the heavily-rewritten shared files. Higher merge cost; risks losing flowwyyy's core improvements.

This doc plans Route A (lower risk, preserves flowwyyy's behavior); Route B would reorder Phase 3 tasks around upstream's code as the base.

## Commands

| Command | Upstream | flowwyyy | Class | Phase-3 action (Route A) |
|---|---|---|---|---|
| init, add, do, run, done, show, list, edit, owner, archive, unarchive, workdir, transcript, hook | ✓ | ✓ | core | reconcile flowwyyy's version as canonical (see shared-file divergence) |
| update | ✓ | ✓ (≈rewritten) | core | flowwyyy version canonical |
| `playbook` (top-level) | ✗ (exists as `run playbook`) | ✓ alias | core | keep alias in core |
| **search** | ✗ | ✓ | core | **land in official flow** |
| **standup** | ✗ | ✓ | core | **land in official flow** |
| **delete** | ✗ | ✓ | core | **land in official flow** |
| **restore** | ✗ | ✓ | core | **land in official flow** |
| **spawn** | ✗ | ✓ | core | **land in official flow** |
| **tell** | ✗ | ✓ | core (after inbox extraction) | **land in official flow** (+ `inbox` pkg) |
| **wait** | ✗ | ✓ | core | **land in official flow** |
| **backup** | ✗ | ✓ | core | **land in official flow** (+ `flowbackup` pkg) |
| ui / serve | ✗ | ✓ | **product** | stays in flowwyyy |
| attention | ✗ | ✓ | **product** | stays in flowwyyy |
| slack | ✗ | ✓ | **product** | stays in flowwyyy |
| stats | ✓ | ✗ | core (upstream-only) | decide: adopt into our core, or drop |
| card / card_png | ✓ | ✗ | core (upstream-only) | likely drop (flowwyyy has the UI) |

## Packages

- **Core, flowwyyy-only (land in official flow):** `agenthooks`, `agents`, `briefing`, `flowbackup`, `ghpr`, `ghref`, `memorysrc`, `schedule`, `termutil`, `workdirreg`, `workevents`, `worktree` (12) + new `cli`, `inbox`.
- **Product (stay in flowwyyy):** `server`, `monitor`, `steering` (+ new `product`, `flowclient`, `productdb`).
- **Upstream-only:** `stats`.
- **Shared (both):** `app`, `flowdb`, `harness`, `spawner`, `iterm`, `kitty`, `ghostty`, `warp`, `zellij`, `terminal`, `listfmt` — all heavily diverged on the flowwyyy side.

## Schema (tables)

- **Upstream tables:** `owners`, `playbooks`, `projects`, `task_tags`, `tasks`, `workdirs` (6; `tasks_new` is a migration temp).
- **flowwyyy CORE tables missing upstream (land in official flow):** `brain_runs`, `task_dependencies`, `task_links`, `agent_runtime_states`, `pending_wakes`, `search_docs` (6).
- **flowwyyy PRODUCT tables (stay in flowwyyy / `productdb`):** `attention_feed`, `attention_feedback`, `attention_handoffs`, `attention_thread_state`, `steering_trace`, `steering_mutes`, `steering_watermark`, `github_event_log`, `github_webhook_deliveries`, `chats`, `remote_devices`, `pending_sends`, `kb_capture` (13).
- **Column-level:** flowwyyy added columns to shared tables (`tasks`: `due_date`, `status_changed_at`, `model`, `assignee`, `waiting_on`, `session_id`, `session_provider`, `permission_mode`, …). A precise column diff is a Phase-3 sub-task.

## Shared core-file divergence (difflines vs upstream)

| File | upstream LOC | flowwyyy LOC | difflines | Note |
|---|---|---|---|---|
| do.go | 1017 | 1189 | 1186 | ≈ rewritten (sessions/auto/worktree/model logic) |
| update.go | 449 | 1085 | 724 | ≈ rewritten (deps, tags, waiting, model, brief-status) |
| show.go | 741 | 936 | 289 | substantial |
| list.go | 1018 | 1038 | 144 | moderate |
| add.go | 355 | 439 | 130 | moderate |
| init.go | 194 | 311 | 129 | moderate (KB/persona seeding) |
| done.go | 191 | 273 | 114 | moderate (close-out sweep) |

Implication: even *shared* commands behave differently. "exec official flow for `do`/`update`/`show`" would NOT reproduce flowwyyy's behavior — another reason Route A (flowwyyy's core canonical) is lower-risk.

## Effort summary (Route A, to switch the dependency to official flow)

Land into `Facets-cloud/flow`: 8 core commands + 12 core packages + 6 core tables + column additions + the reconciled shared-file versions. This is a **substantial upstreaming project** — sized as its own milestone, gated behind Phases 1–2 (which deliver the working two-binary same-experience product on a local core regardless).
