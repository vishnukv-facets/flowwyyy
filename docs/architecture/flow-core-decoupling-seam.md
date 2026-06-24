# Design — flow-core / flowwyyy decoupling: two binaries, subprocess boundary

- **Date:** 2026-06-24 (rev 3 — Phase-3 strategy pivot, see §11; supersedes the single-composed-binary framing of rev 1 and the "upstream our core into Facets-cloud/flow" framing of rev 2 / the upstream-delta Route A/B)
- **Task:** flow-core-seam (follows the spike [[flow-decouple-divergence]])
- **Status:** **Phase 2 complete (plan T1–T12) — behavior parity verified 2026-06-24** (see §10). Two binaries build; `flowwyyy` execs the local `flow` for writes + reads the shared DB; guard test empty; same experience confirmed across the automated surface. **Phase 3 in progress** on a revised strategy (§11): we **cannot modify `Facets-cloud/flow`**, so flowwyyy owns the gap. T13 (product sheds `app`/`flowdb`) started; T14–T16 still gated on operator sign-off of §11.
- **Directive:** CTO Anshul Sao (Slack, 2026-06-17): *"decouple flowwy with flow, it can be a prerequisite, don't bundle and duplicate code there."* **Constraint (Vishnu, 2026-06-24):** we cannot update the `Facets-cloud/flow` repo to accommodate the decoupling — flowwyyy must handle everything official flow lacks on its own side (§11).

## 1. Goal & end-state

**End-state: two separate binaries.**
- `flow` — the **core** engine + CLI (tasks/projects/playbooks/owners/sessions/search/backup). Independently built, versioned, released, and installable.
- `flowwyyy` — the **product** binary (Mission Control UI, connectors, Attention Router/steering, remote). Installed **with a compatible `flow` version alongside it**.

**The reuse mechanism is subprocess invocation.** For every core **mutation and session launch**, the `flowwyyy` binary **execs `flow` by absolute path** — it never reimplements core command logic, so there is no source/logic duplication. **Hybrid reads:** for high-frequency UI reads and the websocket terminal bridge, flowwyyy opens the shared `~/.flow/flow.db` **read-only** directly (linking the `flowdb` read layer), because shelling out per refresh is too costly.

**Why two binaries / subprocess (not one composed binary, not a pure Go-module import):** independent upgradability — drop in a newer compatible `flow` binary without recompiling `flowwyyy`. (See [[flowwyyy-flow-binary-dependency]].)

**Phase-1 proof (done-when):** `cmd/flow` builds and runs as a standalone core CLI; `cmd/flowwyyy` builds, reads the shared DB, and performs core mutations by exec-ing the resolved `flow` binary; a startup version-compat check passes; the guard test confirms core never imports product.

**Decisions locked:**
- Prerequisite is **our own extracted core**, NOT upstream `Facets-cloud/flow` (308 commits / +141k lines diverged).
- **Two binaries**, flowwyyy execs `flow` by abs path for writes/launches; **hybrid** direct-DB reads.
- **Attention Router / steering / connectors = product** (flowwyyy); `flow` has no connector concept.
- **Owners = core** (`flow`); engine-level autonomy alongside tasks/playbooks/`brain_runs`.
- `ghpr`/`ghref` = core utilities.
- **In-repo package seam first** (Phase 1), then the binary/repo split (Phase 2). Keep packages physically in place in Phase 1 (guard-test enforced); relocate in Phase 2.

**Non-goals (this effort):**
- Re-adding/merging the `Facets-cloud/flow` upstream remote.
- Phase 1 does NOT split repos or publish a `flow-core` module — that's Phase 2.

## 2. Organizing principle

**The boundary is responsibility, not provenance.** Packages added *since* the fork (`harness/codex`, `briefing`, `memorysrc`, `worktree`, `agenthooks`, `workdirreg`, `schedule`, `flowbackup`) are still **core engine**. The product surface is essentially three packages: **`server`, `monitor`, `steering`**. (`ghpr`/`ghref` are core GitHub-resolution utilities — used by core's `flow done` PR-linking and the product monitor — so they are core.)

## 3. The boundary

| Dimension | **flow** (core binary `cmd/flow`) | **flowwyyy** (product binary `cmd/flowwyyy`) |
|---|---|---|
| **Packages** | `flowdb`, `app` (core handlers + dispatch), `harness/*`, `spawner` + 6 terminal backends + `termutil`, `agents`, `agenthooks`, `worktree`, `workdirreg`, `workevents`, `briefing`, `memorysrc`, `schedule`, `flowbackup`, `listfmt`, `ghpr`, `ghref`, new `cli`, `inbox` | `server` (Mission Control, terminal bridge, connector wizards, remote), `monitor` (Slack/GitHub ingest), `steering` (attention router + autonomy), new `product` + `flowclient` |
| **Commands** | `flow init add do run playbook done show search standup owner list edit update archive unarchive delete restore workdir skill transcript hook spawn tell wait backup` + hidden `__auto-exec __owner-tick` | `flowwyyy ui serve` (Mission Control) + `flowwyyy attention` + `flowwyyy slack` |
| **DB tables (owns + migrates)** | `projects playbooks tasks workdirs task_tags task_dependencies task_links agent_runtime_states pending_wakes brain_runs search_docs schema_meta owners` | `attention_* steering_* github_event_log github_webhook_deliveries chats remote_devices pending_sends kb_capture` (created by the flowwyyy binary on the shared DB) |
| **Skill** | core skill: §1–§9, §11; references `flow <core>` verbs | product fragment: §4.9b, §10a–§10e; references `flowwyyy <product>` verbs |
| **DB access** | read+write (owns core mutations) | **read** shared `flow.db` directly (links `flowdb`); **write** via exec `flow <cmd>` |

## 4. Shared contracts (the four seams between the binaries)

1. **CLI contract** — the `flow` subcommands + their exit codes / output that flowwyyy execs. Stable; treated as API.
2. **DB schema + `~/.flow` layout** — flowwyyy reads it directly. `flow` owns core tables; flowwyyy owns product tables on the same physical DB. Concurrent access is already handled (`busy_timeout(30000)`, `_txlock=immediate`).
3. **`flowdb` Go read-layer** — flowwyyy links it for reads + product migrations. Same module in Phase 1; a published `flow-core` module in Phase 2.
4. **Version/capability compatibility** — `flow version --json` → `{version, schema, capabilities}`; flowwyyy holds a required floor and refuses to start (clear error) on mismatch.

## 5. Current coupling — the violation work list (unchanged)

Cardinal rule: **no core package imports `server`/`monitor`/`steering`.** `flowdb` already imports nothing product. The full violation surface is **6 files**:

| File | Imports (product) | Fix | Effort |
|---|---|---|---|
| `app/serve.go` | `server` | `ui`/`serve` move to the flowwyyy binary | trivial |
| `app/attention.go` | `steering` | `attention` moves to the flowwyyy binary | trivial |
| `app/slack.go` | `monitor` | `slack` moves to the flowwyyy binary | trivial |
| `app/init.go` | `steering` | core seeds core; product seeds steerer persona via init-hook | small |
| `app/tell.go` | `monitor`, `server` | extract inbox-event model to core `internal/inbox`; use `cli.SessionTokenFileName` | real refactor |
| `workevents/builder.go` | `monitor` | use core `internal/inbox` instead of `monitor` | real refactor |

## 6. Mechanisms

### 6.1 Command registry (`internal/cli`)
A per-process registry (`Command`, `Register`, `Lookup`, `Each`). **`cmd/flow/main.go`** registers core commands; **`cmd/flowwyyy/main.go`** registers product commands (`ui serve`, `attention`, `slack`). Each binary has its own dispatcher; there is NO cross-binary registration.

### 6.2 The `flowclient` exec layer (flowwyyy → flow)
New core-agnostic package `internal/flowclient` used by flowwyyy:
- **Abs-path resolution** (once at startup): `$FLOW_BIN` → sibling of flowwyyy's own executable (`os.Executable()` dir) → `exec.LookPath("flow")`. Error clearly if unresolved.
- **`FlowClient{Bin string}`** with `Run(args ...) (stdout, stderr string, code int)` (context timeout) and typed helpers for the mutations flowwyyy performs: `Do(slug, opts)`, `Done(slug)`, `Add(...)`, `Update(...)`, `Archive/Unarchive/Delete/Restore(ref)`, `Spawn(...)`, `RunPlaybook(...)`, `PlaybookTickDue()`, `OwnerTickDue()`. These **replace** the in-process mutations in `server/actions.go` (`runFlowCommand` is the seed) and the in-process launch in `server/terminal_launch.go` (→ `flowClient.Do`).
- **Version-compat check** at flowwyyy startup: `flow version --json`, parse, compare to the embedded floor; refuse with a clear message if below it.

### 6.3 Migration registry + DB ownership
- `flowdb` gains `MigrationSet{Domain, Apply}` + `RegisterMigrations`. `OpenDB` applies `coreSchemaDDL` + core migrations + any registered sets.
- **`flow` binary:** opens the DB → core tables created/migrated.
- **`flowwyyy` binary:** links `flowdb`, registers the **product** migration set (the `attention_*`/`steering_*`/`github_*`/`chats`/`remote_devices`/`pending_sends`/`kb_capture` DDL), and opens the DB at startup → core tables (from flowdb) + product tables (its registered set) on the same physical DB. Product DDL is additive over existing installs.

### 6.4 Extension hooks
- **`internal/inbox`** — extract the inbox-event model (`InboundEvent`, `InboxEntry`, `ReadInboxEntries`, `AppendInboxEvent[Stamped]`, `FlowTellEvent`, `ClassifyInboxEvent`, `ThreadKey`) out of `monitor` into core. `tell` (core) and `workevents` (core) import `inbox`; `monitor` (product) becomes a producer that imports `inbox`. (No `Waker` interface needed — `tell`'s live wake already goes over HTTP via `FLOW_UI_URL`.)
- **init-hook** — `app.RegisterInitHook`; core `flow init` runs registered hooks; flowwyyy registers the steerer-persona seed. Removes the last `app`→`steering` import.
- **`cli.SessionTokenFileName`** — the `.ui-session-token` filename moves to core `cli` so `tell` needn't import `server`.

### 6.5 Skill differentiation (two binaries)
- Core skill (§1–§9, §11) embedded in `flow`; installed by `flow skill install`; references `flow <core>` verbs. Add `flow skill print` (emit the raw core skill to stdout).
- Product fragment (§4.9b, §10a–§10e) embedded in `flowwyyy`; references `flowwyyy <product>` verbs. `flowwyyy skill install` **composes** the core skill (obtained via `flow skill print` — no duplication) + the product fragment, and installs the combined `SKILL.md` for agents. The SessionStart/hook wiring points the agent CLI at `flow` for core verbs.

### 6.6 The two `main` packages
- `cmd/flow/main.go` — registers core commands, calls `app.Run`.
- `cmd/flowwyyy/main.go` — resolves `flow` via `flowclient`, runs the version-compat check, registers product commands, ensures product DB tables, serves Mission Control. Build both with `go build ./cmd/flow` and `go build ./cmd/flowwyyy`.

## 7. Phasing

**Phase 1 — in-repo package seam (prerequisite; one repo).**
1. Guard test (core↛product) with a violation ratchet.
2. `internal/cli` command registry.
3. Export the core helpers product code needs; `RegisterInitHook`.
4. Extract `internal/inbox`; rewire `tell` + `workevents` (closes 2 violations).
5. init-hook for steerer persona (closes the last `app`→product import).
6. Move `ui`/`attention`/`slack` handlers out of `app` into product packages.
7. Migration registry + schema domain split.
8. **`internal/flowclient`** (abs-path resolution + `FlowClient` + version-compat); add `flow version --json`.
9. Replace in-process mutations (`server/actions.go`, `server/terminal_launch.go`) with `flowclient` execs.
10. Split the two `main` packages (`cmd/flow`, `cmd/flowwyyy`); flowwyyy registers the product migration set + runs the compat check.
11. Skill differentiation: core skill + `flow skill print`; product fragment + `flowwyyy skill install` composer.
12. Acceptance: both binaries build; flowwyyy reads the DB + execs `flow` for mutations; guard test empty; full behavior preserved.

**Phase 2 — repo/module split + packaging (separate spec).**
- Physically relocate core packages into a published `flow-core` Go module (or separate repo); flowwyyy depends on it for the read layer.
- Packaging: the flowwyyy installer (Homebrew formula, etc.) declares a dependency on a compatible `flow` version, installed alongside.
- Independent release pipelines for `flow` and `flowwyyy`.

## 8. Risks & mitigations
- **DB schema drift between binaries.** Mitigation: schema version in `schema_meta` + the §4.4 version-compat check; product migrations additive.
- **`flow` binary not found / wrong version at runtime.** Mitigation: `flowclient` resolution order + a clear startup error naming `$FLOW_BIN`; version floor check.
- **Concurrent DB writers** (flow exec while flowwyyy reads). Mitigation: existing `busy_timeout`/`_txlock=immediate`; flowwyyy reads only (writes go through the `flow` process which owns the write path).
- **exec latency on mutation paths.** Acceptable: mutations are user-initiated/low-frequency; reads (the hot path) stay in-process. 
- **Behavioral drift in the shipped product.** Mitigation: `make test` + e2e suite green throughout; the flowwyyy binary must reproduce today's behavior (the in-process→exec swap is behavior-preserving).
- **Hidden re-coupling over time.** Mitigation: the guard test in CI.

## 9. Open questions (narrowed)
- Does `flowwyyy` also expose a passthrough for core verbs (`flowwyyy do` → exec `flow do`) for one-entrypoint UX, or are core verbs always run via `flow` directly (by skill/hooks/user)? **Resolved (T10):** flowwyyy DOES passthrough — any non-product verb execs the resolved `flow` with inherited stdio (byte-identical). The full command surface is preserved through one entrypoint.
- Phase-2 packaging mechanics (Homebrew formula dependency, version pinning) — deferred to the Phase 2 spec.

## 10. Phase 2 acceptance gate — RESULTS (plan T12, 2026-06-24)

Full report: `~/.flow/tasks/twobin-parity-gate/artifacts/PARITY-REPORT.md`. Verdict: **PASS on the full automated surface**; 2 defects found & fixed, 1 benign observation; live-connector / browser-UI / real-agent items on the operator manual checklist.

**Automated surface — all green:**
- Static: `go vet` clean; `go test ./...` 29/29; `archtest` empty ratchet + `cmd/flow` product-free; both binaries build; `flow version --json` works.
- CLI parity: **44/44** core verbs identical `flow` vs `flowwyyy` passthrough (incl. exit codes + unknown-verb); only cross-run diffs are intra-binary non-determinism (backup commit hashes, workdir sort tiebreak).
- Product: core rejects `ui`/`attention`/`slack`; `flowwyyy ui serve` boots, serves the UI (200 + token injection) and the token-gated `productdb`-backed API, and resolves the sibling `flow` for writes; `attention`/`slack` native.
- Skill (T11): `flowwyyy skill install` composes the full skill — **content-complete vs the pre-split combined SKILL.md (0 lines lost, 2 added)**. SessionStart hook output byte-identical. GitHub webhook endpoint wired (signature gate active).

**Defects fixed during the gate (working tree; commit pending operator approval):**
1. *(test-only)* `internal/server` test helper ran `go run .` after T10 deleted the root `main.go` → `go run ./cmd/flow`. Pre-existed on `exec-mains`/PR #41 (that PR landed with `internal/server` tests red — back-fix recommended).
2. *(blocking)* `flowwyyy skill install` installed a **core-only** skill (T10↔T11 wiring gap: `skill` wasn't routed to `product.ComposeSkill`). Fixed by wiring native `skill` handling in flowwyyy + `make install` using `flowwyyy skill install` + a regression test.

**Observation:** the `seedSteererPersona` init-hook is now unreachable (init passes through to core); `persona.md` isn't materialized at init, but behavior is preserved by the `DefaultPersonaMarkdown` fallback in `OperatorVoice()` + the UI. Cosmetic; cleanup candidate.

**Carryover to Phase 3 (T13–T16):** product still imports `app`/`flowdb` (allowed in Phase 2; T13 removes it via `productdb` read layer + the second guard ratchet). The upstream-delta (T14) remains large — see `flow-core-upstream-delta.md`.

## 11. Phase-3 strategy pivot — flowwyyy owns the gap (rev 3, 2026-06-24)

**New constraint (Vishnu, 2026-06-24):** we **cannot update `Facets-cloud/flow`** to accommodate the decoupling. This supersedes the upstream-delta's Route A (our core *becomes* official flow) and Route B (reconcile onto upstream) — both required changing `Facets-cloud/flow`. The dependency must target official flow **as-is**, and flowwyyy must own everything official flow lacks.

### 11.1 Governing principle — ownership-based write routing

Every table on the shared `~/.flow/flow.db` has **exactly one writer**, decided by which binary owns it:

| Bucket | Tables | flowwyyy reads | flowwyyy writes |
|---|---|---|---|
| **O — official flow owns + exposes a CLI verb** | `tasks, projects, playbooks, task_tags, owners, workdirs, schema_meta` | via `productdb` (own structs + SELECTs) | **only** by exec-ing `flow` (`flowclient`) using verbs/flags official flow already ships (`do/done/add/update/archive/unarchive/run/owner`) — no reimplementation of core mutation logic |
| **F — official flow LACKS it → flowwyyy owns** | 6 core-gap: `brain_runs, task_dependencies, task_links, agent_runtime_states, pending_wakes, search_docs` · 13 product: `attention_*, steering_*, github_event_log, github_webhook_deliveries, chats, remote_devices, pending_sends, kb_capture` | via `productdb` | **directly** via `productdb` — flowwyyy creates/migrates them (`productdb.Ensure`) and writes them in-process. No exec, no `flowdb` import. Concurrency already safe (`busy_timeout` + `_txlock=immediate`). |

The 8 commands official flow lacks (`search standup delete restore spawn wait backup tell`) follow the same rule: flowwyyy provides them itself (its own `cmd/flowwyyy` surface / passthrough), never expecting them from official flow.

**Why this is correct & forward-compatible:** it assumes nothing of official flow beyond verbs it already ships. When the dependency flips from the local extracted core to the real released `flow` binary, **nothing in Bucket F can break** — flowwyyy never touched `Facets-cloud/flow`, and it owns every table/command official flow doesn't provide. Bucket O's exec calls are the only coupling to official flow's CLI contract (§4 seam 1) — the startup version-compat check (§6.2) guards that contract.

### 11.2 Consequence for the code (`productdb` becomes self-contained)

`internal/productdb` stops importing `flow/internal/flowdb`. It grows: its own `Open(path)`/`Ensure(db)` (same pragmas, `modernc` driver), its own row types mirroring the shared schema, read queries for **all** tables flowwyyy uses, and write functions for Bucket F. The migration registration via `flowdb.RegisterMigrations` is replaced by an explicit `productdb.Open` call wired from `cmd/flowwyyy` startup.

Product packages must shed **both** fenced core packages: `flowdb` (→ `productdb`) **and** `app` (the helpers product uses — `FlagSet/FlowRoot/FlowDBPath/FlowServerURL/UISessionToken/Version`/skill compose — relocate to a neutral package both binaries import; `cli` is already product-safe). Only `flow/internal/app` and `flow/internal/flowdb` are fenced; `cli`, `inbox`, `flowclient` remain importable.

### 11.3 T13 burndown (each step a reviewable commit; second ratchet shrinks per step; `make test` green throughout)

1. ✅ **Guard test + second ratchet** — `archtest.TestProductDoesNotImportCoreGo` asserts `cmd/flowwyyy` + `server`/`monitor`/`steering`/`product` don't transitively import `app`/`flowdb`. Ratchet pre-populated to current reality (5 pkgs); RED→GREEN verified.
2. **Keystone — `productdb` self-contained** (own `Open`/`Ensure`/types/read-queries/Bucket-F writes; sever `flowdb.RegisterMigrations`; relocate `app` helpers to a neutral pkg; wire `cmd/flowwyyy` → `productdb.Open`).
3. Cut `product` over to `productdb` (2 files) → remove from ratchet.
4. Cut `monitor` over (5 files) → remove from ratchet.
5. Cut `steering` over (16 files) → remove from ratchet.
6. Cut `server` over (51 files) → remove from ratchet.
7. `cmd/flowwyyy`: product-side version (drop `app`) → ratchet empty.
8. `make test` + Mission Control read/write parity spot-check; revise this doc to "Phase 3 / T13 complete."

**Measured surface (2026-06-24):** 180 distinct `flowdb` symbols across 75 non-test files (server 51, steering 16, monitor 5, product 2, productdb 1). T13 is a sub-milestone; the brief sanctions splitting it into finer tasks.

### 11.4 What this changes vs. `flow-core-upstream-delta.md`

The upstream-delta doc's effort summary and Route A/B framing are **superseded** for execution purposes: we no longer plan to land flowwyyy's core (8 commands, 6 tables, 12 packages, rewritten shared files) *into* `Facets-cloud/flow`. The delta remains useful as the **measurement** of what official flow lacks — i.e. exactly the Bucket F surface flowwyyy must own. T14 is re-scoped from "upstream into official flow" to "flowwyyy self-sufficiently owns the gap against official flow as-is" (still gated on operator sign-off).
