# flow — repo conventions

## What this is

A Go CLI (`flow`) that manages personal tasks/projects/playbooks/owners and
bootstraps per-task Claude Code or Codex sessions. SQLite via
`modernc.org/sqlite` (pure Go, no CGO).

It started as a small task CLI and has grown a substantial surface:
- a **Mission Control web UI** + websocket terminal bridge (`internal/server`),
- **connector ingestion + triage** for Slack and a GitHub App
  (`internal/monitor`, `internal/steering`),
- **autonomous/headless runs and owners** (the `brain_runs` ledger,
  `flow do --auto`, `flow owner ...`),
- **six terminal backends** for spawning tabs.

Keep this file honest: when you add a package or a major subsystem, update the
map below. A stale architecture doc is how the next feature ends up bolted on
in the wrong place.

## Build and test

```bash
# Build (produces ./flow in the repo dir, which is on PATH)
make build
# or: go build -o flow .

# Full install (build + PATH + init + skill + hook)
make install

# Run all tests (fast — no network, no real iTerm/Claude)
make test
# or: go test ./...

# Run a single test
go test -run TestE2EFullRoundtrip -v ./internal/app/
```

Tests use `$FLOW_ROOT` pointed at a temp directory and override `$HOME` so
nothing touches real `~/.flow/` or `~/.claude/`. External dependencies
(osascript, claude/codex CLIs, Slack, GitHub) are mocked via package-level
function vars.

## Architecture (27 packages)

```
flow/
├── main.go                  # thin entry point — calls app.Run()
└── internal/
    ├── app/                 # CLI: every `flow <subcommand>` handler + dispatch
    ├── flowdb/              # SQLite data layer: schema DDL, models, CRUD, migrations
    ├── server/             # Mission Control: web UI, websocket terminal bridge,
    │                       #   Slack/GitHub setup wizards, attention-feed API
    ├── monitor/            # Slack Web API + GitHub App webhook ingestion → dispatcher
    ├── steering/           # attention router ("steerer") + autonomy policy (ApplyAction)
    ├── schedule/           # human schedule phrase ("every 6 hours") → cron
    ├── workevents/         # work-event read model (the activity log)
    ├── briefing/           # `flow standup` aggregation + markdown render
    ├── harness/            # agent runtime abstraction …
    │   ├── claude/         #   … Claude Code backend
    │   └── codex/          #   … Codex backend
    ├── spawner/            # picks a terminal backend at runtime, forwards SpawnTab
    ├── termutil/           # shared terminal helpers (ShellQuote, EscapeAppleScript,
    │                       #   AccessibilityDenied) — single home for what the 6
    │                       #   backends used to copy-paste
    ├── iterm/ kitty/ ghostty/ warp/ zellij/ terminal/   # 6 terminal-tab backends
    ├── agents/             # Codex/Claude session discovery (walk session JSONL)
    ├── agenthooks/         # install/upgrade agent hook config (claude + codex providers)
    ├── ghpr/ ghref/        # GitHub PR/ref resolution for a local checkout
    ├── worktree/           # per-task git worktrees
    ├── workdirreg/         # workdir registry + git-remote detection
    ├── listfmt/            # shared `flow list` table renderer
    └── memorysrc/          # discover KB / agent-memory markdown sources
```

## Package responsibilities (the load-bearing ones)

- **`internal/app`** — all CLI command handlers, dispatch (`app.go`), shared
  helpers (`flagSet()`). Roughly one file per subcommand. Also hosts hidden
  commands `__auto-exec` and `__owner-tick` (headless run / owner-tick entry
  points). Imports nearly every other internal package.
- **`internal/flowdb`** — schema DDL, model structs (`Project`, `Task`,
  `Playbook`, `Workdir`, `BrainRun`, owners, attention/steering tables), scan
  helpers, CRUD, and the boot migration runner. All access via `database/sql` +
  `modernc.org/sqlite`. `brain_runs` is the **live** autonomous-run ledger;
  the old Brain *orchestration* (brain_plans/policy/action_audit) was removed
  in #34 — don't resurrect those tables.
- **`internal/server`** — the largest package; Mission Control web UI and its
  JSON/websocket API, the terminal bridge, and the Slack/GitHub connector
  setup wizards. New web features go here, not in `app`.
- **`internal/monitor` + `internal/steering`** — connector ingestion (Slack
  Socket Mode + GitHub App webhooks) and the attention router that triages
  events into the feed / tasks. `steering.ApplyAction` + `DefaultAutonomy()`
  gate every outward action; autonomy defaults to surface-only.
- **`internal/spawner` + the 6 backends + `internal/termutil`** — backend
  selection is by runtime env detection in `spawner`; each backend renders an
  osascript/CLI invocation. Shared string/error helpers live in `termutil` —
  add new ones there, never re-copy into a backend.

## Conventions

- **No CGO.** Pure Go SQLite driver (`modernc.org/sqlite`).
- **Flag parsing:** `flag.FlagSet` with `ContinueOnError`, not `flag.Parse()`.
  Created via `flagSet()` helper in `internal/app/helpers.go`.
- **Exit codes:** 0 = success, 1 = runtime error, 2 = usage error.
- **Timestamps:** RFC3339 strings everywhere (never Unix timestamps).
- **Tests:** Table-driven where possible. Command tests live alongside source.
  `internal/app/e2e_test.go` exercises the full command surface in sequence.
- **No mocks for DB.** Tests use real SQLite in a temp dir. External processes
  (osascript, claude/codex, Slack, GitHub) are mocked via function vars.
- **Skill file is the source of truth** for how Claude/Codex sessions interact
  with flow. If the skill says something, the code must support it.
- **Skill embed path:** `internal/app/skill/SKILL.md` is embedded at compile
  time via `//go:embed` in `internal/app/skill.go`. Rebuild after editing for
  `flow skill update` to pick it up.

## Data directory layout

```
~/.flow/
  flow.db
  kb/{user,org,products,processes,business}.md
  projects/<slug>/brief.md, updates/*.md
  tasks/<slug>/brief.md, updates/*.md, inbox.md, inbox.jsonl
  playbooks/<slug>/brief.md, updates/*.md
  owners/<slug>/charter.md, updates/*.md
```

## Things to watch out for

- `hookCommand` in `internal/app/skill.go` is the exact string matched in
  `~/.claude/settings.json`. Changing it orphans existing installations.
- `do.go` uses `openConcurrentDB` with `busy_timeout(30000)` and
  `_txlock=immediate` for safe concurrent access.
- Tests override `$HOME` — any code that calls `os.UserHomeDir()` sees the
  test's temp dir, not the real home.
- `flowdb` migrations run on every boot and include a drop-list for tables
  from removed features; add orphaned tables there rather than leaving dead
  schema behind.
- Connector secrets (Slack tokens, GitHub App key) live in the OS keyring, not
  config.json.
