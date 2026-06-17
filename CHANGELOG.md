# Changelog

All notable changes to flowwyyy are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
(`0.x.y` until the API stabilises).

## [Unreleased]

## [0.1.0-alpha.2] — 2026-06-17

Steerer hardening: the per-channel session model is now the single, authoritative
steering path — with opt-in autonomous replies, cleaner routing, and a much
faster attention feed.

### Added

- **Opt-in autonomous replies.** A new **Auto-send reply** autonomy toggle (off by
  default, gated at a high threshold). When enabled, a high-confidence reply is
  posted by the channel's own per-channel chat (Slack) or the `gh` agent (GitHub),
  then forwarded to the matched task. Session-surfaced cards now also honor the
  existing forward / make-task autonomy server-side (they previously bypassed the
  gate entirely).

### Changed

- **The per-channel session model is the master switch.** ON ⇒ the steerer owns
  routing (triage → attention feed / per-channel sessions). OFF ⇒ the steerer
  stands down and events route the old way (Slack reaction-trigger → task
  `inbox.jsonl`, GitHub webhook → legacy task pipeline) — no attention, no triage.
- **Replies always post through the connector's own send path** — Slack via the
  per-channel chat, GitHub via the `gh` agent — never by forwarding to a matched
  task and asking it to send.
- **Dev builds are version-stamped** via `git describe --tags --always --dirty`
  (e.g. `v0.1.0-alpha.2-3-g<sha>`), so a local `make build` is distinguishable
  from a tagged release at a glance.

### Removed

- The ephemeral Slack send session and its `FLOW_STEERING_SEND_MODEL` setting —
  replies post through the channel chat / `gh` agent instead.

### Fixed

- **Backfill bypassed the session model.** The steerer catch-up sweep ran the old
  stateless cascade and surfaced `digest_only` FYI cards from backfilled Slack
  traffic; it now delivers to the per-channel session like the live path.
- **Wake prompts stranded mid-turn.** The session wake now pastes and presses
  Enter (Claude Code / Codex queue mid-turn input) instead of waiting for the
  agent to go idle, which left the prompt unsent during long turns.
- **DMs and group chats were titled by the operator** instead of the other
  participant.
- **`flow ui serve` left a stale server running.** It now takes over the port from
  an existing flow ui-serve, so a rebuild + restart actually serves the new binary
  instead of silently failing to bind.

### Performance

- **Attention feed.** `acted` / `dismissed` / `all` no longer stall (a cold
  `dismissed` tab took ~43s): Slack permalinks are resolved concurrently up front
  and the per-row lookup is cache-only — ~43s → ~2s on a cold tab.

## [0.1.0-alpha.1] — 2026-06-15

Initial offering. **flowwyyy is `flow`, with batteries** — the original
[`flow`](https://github.com/Facets-cloud/flow) CLI, kept backward-compatible,
plus the browser UI, connectors, triage, and autonomy layer that turn each
agent session from a brilliant new hire into the engineer on your team.

### Added

- **The `flow` CLI.** Personal tasks, projects, playbooks, and owners in a
  single pure-Go SQLite store (`modernc.org/sqlite`, no CGO). Free-form briefs
  and dated progress updates live as markdown on disk; metadata lives in
  `~/.flow/flow.db`. Exit codes, RFC3339 timestamps, and `flag.FlagSet`
  parsing throughout.
- **Per-task agent sessions.** `flow do <task>` bootstraps or resumes a
  long-lived **Claude Code** or **Codex** session in its own terminal tab,
  pre-loaded with the brief, updates, and repo conventions. Priority-aware
  model selection picks the tier when `--model` isn't pinned.
- **Mission Control.** A browser UI (Vite + React + TS, embedded in the
  binary) over a websocket-RPC bridge: dashboard, tasks/projects/playbooks,
  knowledge base, owners, and a live in-browser terminal that attaches to the
  same sessions the CLI spawns.
- **Connectors.** First-class **Slack** (Socket Mode + user-token channel
  monitoring) and **GitHub App** (org-wide webhooks) ingestion, with setup
  wizards in the UI. Connector secrets live in the OS keyring, never in config.
- **Attention Router.** A cross-source triage pipeline ("steerer") that
  classifies inbound Slack/GitHub activity into an operator feed — surface,
  forward, make-task, capture-to-KB — gated by an autonomy policy that
  defaults to surface-only.
- **Autonomous owners + headless runs.** `flow owner` schedules recurring
  agent work; `flow do --auto` runs a task headlessly. The `brain_runs` ledger
  records every autonomous run.
- **Working-memory knowledge base.** `~/.flow/kb/*.md` (user, org, products,
  processes, business) plus per-entity briefs, carried into every session so
  context isn't re-typed.
- **Six terminal backends.** iTerm2, kitty, Ghostty, Warp, zellij, and
  Terminal.app, selected by runtime environment detection. `FLOW_TERM`
  overrides detection.
- **`flow standup`.** Aggregates attention, waiting-on, stale, ready, and
  recent activity into a copyable briefing.
- **Embedded skill + hooks.** `~/.claude/skills/flow/SKILL.md` is the
  natural-language interface; SessionStart/PostToolUse hooks re-inject context
  and wake live sessions on inbound events. Installed by `flow init`.
- **Quality gate.** `golangci-lint` CI check (new-issue gated) alongside
  `go vet` / `go build` / `go test ./...`.
- **License.** MIT.

[Unreleased]: https://github.com/vishnukv-facets/flowwyyy/compare/v0.1.0-alpha.2...HEAD
[0.1.0-alpha.2]: https://github.com/vishnukv-facets/flowwyyy/releases/tag/v0.1.0-alpha.2
[0.1.0-alpha.1]: https://github.com/vishnukv-facets/flowwyyy/releases/tag/v0.1.0-alpha.1
