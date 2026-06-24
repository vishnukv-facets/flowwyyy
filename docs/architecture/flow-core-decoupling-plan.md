# flow ↔ flowwyyy decoupling — Full Implementation Plan (two binaries, same experience)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split the current single `flow` binary into **`flow` (core engine)** and **`flowwyyy` (product layer)** such that flowwyyy consumes flow as an external versioned binary (exec by absolute path for writes/launches; direct read of the shared `~/.flow/flow.db`), with **zero change to the end-user, UI, connector, or agent experience**.

**Architecture:** A three-phase path. (1) Carve a test-enforced core↔product package boundary in-repo. (2) Build two `main`s in the repo — `cmd/flow` and `cmd/flowwyyy` — wired together via an exec client + a startup version-compat check, with flowwyyy preserving the full command surface via passthrough; validated against a locally-built `flow` for behavior parity. (3) Land the extracted core in official `Facets-cloud/flow`, make flowwyyy's Homebrew formula `depends_on "flow"` (official, not bundled), and target official releases.

**Tech Stack:** Go 1.25, pure-Go SQLite (`modernc.org/sqlite`, no CGO), `flag.FlagSet`, `//go:embed`, `os/exec`, Homebrew.

## Global Constraints

- **SAME EXPERIENCE (the prime directive).** After every phase, the shipped product's behavior, Mission Control UI, connectors (Slack/GitHub), Attention/steering, terminal bridge, agent sessions, and **full command surface** must be **identical to today**. The decoupling is internal. Any user-visible or behavioral change is a defect. Every phase ends with a parity check; Phases 2 and 3 end with a full parity acceptance gate.
- **No CGO.** Pure-Go SQLite only.
- **Flag parsing:** `flag.FlagSet` with `ContinueOnError` via the `flagSet()` helper. Never `flag.Parse()`.
- **Exit codes:** 0 success, 1 runtime error, 2 usage error.
- **Timestamps:** RFC3339 strings.
- **Tests:** real SQLite in a temp dir; `$FLOW_ROOT` + `$HOME` overridden; external processes mocked via package-level function vars. `make test` and `internal/app/e2e_test.go` MUST pass after every task.
- **Dependency direction (cardinal rule):** no core package imports `flow/internal/server`, `flow/internal/monitor`, or `flow/internal/steering`. Task 1's guard test enforces it. **Stronger rule from Task 10 on:** product packages + `cmd/flowwyyy` must not import `flow/internal/app` either (so they can compile against *official* flow in Phase 3, whose `app` is unreachable under `internal/`).
- **Commits:** repo develops on `main`; **commit only when the operator approves.** The `commit` steps mark intended boundaries.
- **Phase 3 reality:** official `Facets-cloud/flow` keeps its code under `internal/`, so flowwyyy can NEVER import flow's Go packages. flowwyyy reads the DB with its **own** read layer and execs the `flow` binary for writes. Phases 1–2 are built to be Phase-3-ready (no `app`/`flowdb` import leakage into product).

## Boundary reference

- **Core (`flow`):** `flowdb`, `app`, `harness/*`, `spawner` + 6 terminal backends + `termutil`, `agents`, `agenthooks`, `worktree`, `workdirreg`, `workevents`, `briefing`, `memorysrc`, `schedule`, `flowbackup`, `listfmt`, `ghpr`, `ghref`, new `cli`, `inbox`.
- **Product (`flowwyyy`):** `server`, `monitor`, `steering`, new `product`, `flowclient`, `productdb` (flowwyyy's own DB read/migrate layer).
- **Product commands (must keep working identically):** `ui serve`, `attention`, `slack` — natively in flowwyyy; **all core verbs** — passthrough-exec to `flow`.
- **Product tables (flowwyyy owns + migrates on the shared DB):** `attention_* steering_* github_event_log github_webhook_deliveries chats remote_devices pending_sends kb_capture`.

## File Structure

**New (core):** `internal/cli/cli.go` (registry + `SessionTokenFileName`), `internal/inbox/{event,inbox}.go` (moved from monitor), `internal/app/exports.go` (helper wrappers + `RegisterInitHook`), `internal/archtest/boundary_test.go`, `cmd/flow/main.go`, `internal/app/skill/SKILL.core.md`, plus `flow version --json` + `flow skill print`.

**New (product):** `internal/flowclient/{resolve,client,compat}.go`, `internal/productdb/{schema,read}.go` (flowwyyy's own product-table DDL + read queries), `internal/product/{commands,passthrough}.go`, `internal/product/seed.go`, `internal/product/skill/SKILL.flowwyyy.md`, `cmd/flowwyyy/main.go`.

**Modified:** `internal/app/app.go` (Run→registry), `internal/app/skill.go` (compose + print), `internal/app/tell.go` (use `inbox` + `cli.SessionTokenFileName`), `internal/app/init.go` (init-hooks), `internal/workevents/builder.go` (use `inbox`), `internal/flowdb/{db,schema,migrations}.go` (core-only schema; keep migration registry hook for tests), `internal/monitor/{inbox,inbound_event}.go` (re-home model to `inbox`), `internal/server/actions.go` + `terminal_launch.go` (in-process mutations → `flowclient`), `internal/server/*` (reads → `productdb`; session-token const → `cli`), `Makefile`, `homebrew-flowwyyy` formula.

**Deleted:** `internal/app/serve.go`, `attention.go`, `slack.go` (relocated); root `main.go` (replaced by `cmd/flow` + `cmd/flowwyyy`).

---

# PHASE 1 — Segregate (in-repo package seam)

### Task 1: Guard test (dependency-direction ratchet)

**Files:** Create `internal/archtest/boundary_test.go`.

- [ ] **Step 1: Write the guard test**

```go
package archtest

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

var productPkgs = map[string]bool{
	"flow/internal/server":   true,
	"flow/internal/monitor":  true,
	"flow/internal/steering": true,
}

// knownViolations is the ratchet; each task removes entries; ends empty.
var knownViolations = map[string]bool{
	"flow/internal/app":        true, // serve/attention/slack/tell/init
	"flow/internal/workevents": true, // builder.go → monitor
}

var corePackages = []string{
	"flow/internal/app", "flow/internal/flowdb", "flow/internal/workevents",
	"flow/internal/briefing", "flow/internal/agents", "flow/internal/agenthooks",
	"flow/internal/worktree", "flow/internal/workdirreg", "flow/internal/memorysrc",
	"flow/internal/schedule", "flow/internal/flowbackup", "flow/internal/listfmt",
	"flow/internal/spawner", "flow/internal/termutil", "flow/internal/ghpr", "flow/internal/ghref",
}

func deps(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil { t.Fatalf("go list -deps %s: %v", pkg, err) }
	return strings.Fields(string(out))
}

func TestCoreDoesNotImportProduct(t *testing.T) {
	var clean []string
	for _, pkg := range corePackages {
		bad := false
		for _, d := range deps(t, pkg) {
			if productPkgs[d] { bad = true; break }
		}
		switch {
		case bad && !knownViolations[pkg]:
			t.Errorf("REGRESSION: core package %s imports a product package", pkg)
		case !bad && knownViolations[pkg]:
			clean = append(clean, pkg)
		}
	}
	if len(clean) > 0 {
		sort.Strings(clean)
		t.Errorf("clean now — remove from knownViolations: %v", clean)
	}
}
```

- [ ] **Step 2: Run; expect PASS** — `go test ./internal/archtest/ -v` (allowlist matches reality).
- [ ] **Step 3: Commit** — `git add internal/archtest/ && git commit -m "test(arch): core→product dependency guard with ratchet"`

### Task 2: Command registry (`internal/cli`)

**Files:** Create `internal/cli/cli.go`, `internal/cli/cli_test.go`; Modify `internal/app/app.go`, add `internal/app/register.go`.
**Interfaces:** Produces `cli.Command{Name string; Run func([]string) int; Help string; Hidden bool}`, `cli.Register`, `cli.Lookup`, `cli.Each`, `const cli.SessionTokenFileName = ".ui-session-token"`, `cli.Reset` (tests).

- [ ] **Step 1: Failing test** (`internal/cli/cli_test.go`)

```go
package cli
import "testing"
func TestRegisterAndLookup(t *testing.T) {
	Reset()
	Register(Command{Name: "demo", Run: func([]string) int { return 7 }})
	c, ok := Lookup("demo")
	if !ok || c.Run(nil) != 7 { t.Fatal("demo not registered/runnable") }
	if _, ok := Lookup("nope"); ok { t.Fatal("unexpected hit") }
}
```

- [ ] **Step 2: Run; expect FAIL** — `go test ./internal/cli/`.
- [ ] **Step 3: Implement `internal/cli/cli.go`**

```go
// Package cli is the per-process command registry shared by flow-core and the
// flowwyyy product layer. Importing it pulls in no product code.
package cli

import "sort"

const SessionTokenFileName = ".ui-session-token"

type Command struct {
	Name   string
	Run    func(args []string) int
	Help   string
	Hidden bool
}

var registry = map[string]Command{}

func Register(c Command)                 { registry[c.Name] = c }
func Lookup(name string) (Command, bool) { c, ok := registry[name]; return c, ok }
func Each(fn func(Command)) {
	names := make([]string, 0, len(registry))
	for n := range registry { names = append(names, n) }
	sort.Strings(names)
	for _, n := range names { fn(registry[n]) }
}
func Reset() { registry = map[string]Command{} }
```

- [ ] **Step 4: Run; expect PASS** — `go test ./internal/cli/`.
- [ ] **Step 5: Dispatch via registry.** In `internal/app/app.go`, after the `--version`/`help` special cases, replace the big `switch` with:

```go
	if c, ok := cli.Lookup(cmd); ok {
		return c.Run(rest)
	}
	fmt.Fprintf(os.Stderr, "error: unknown subcommand %q\n", cmd)
	printUsage()
	return 2
```
Create `internal/app/register.go` with `func registerCore()` that `cli.Register`s every CURRENT verb → its `cmdXxx` (including `ui`,`serve`,`attention`,`slack` as TEMPORARY shims so behavior is unchanged; and `__auto-exec`,`__owner-tick` with `Hidden:true`). Call `registerCore()` from `app`'s `init()`.

- [ ] **Step 6: Verify parity** — `make test`; `./flow --help`, `./flow list tasks`, `./flow ui --help` behave as before.
- [ ] **Step 7: Commit** — `git commit -m "refactor(cli): command registry; app.Run dispatches via it"`

### Task 3: Export core helpers + init-hook

**Files:** Create `internal/app/exports.go`.
**Interfaces:** Produces `app.FlagSet`, `app.FlowRoot`, `app.FlowDBPath`, `app.FlowServerURL`, `app.UISessionToken`, `app.RegisterInitHook(func() error)`.

- [ ] **Step 1: Implement**

```go
package app

import "flag"

func FlagSet(name string) *flag.FlagSet { return flagSet(name) }
func FlowRoot() (string, error)         { return flowRoot() }
func FlowDBPath() (string, error)       { return flowDBPath() }
func FlowServerURL(path string) string  { return flowServerURL(path) }
func UISessionToken() string            { return uiSessionToken() }

var initHooks []func() error
func RegisterInitHook(fn func() error) { initHooks = append(initHooks, fn) }
```

- [ ] **Step 2: Verify** — `go build ./...`.
- [ ] **Step 3: Commit** — `git commit -m "refactor(app): export core helpers + init-hook registry"`

### Task 4: Extract `internal/inbox` (closes tell + workevents)

**Files:** Create `internal/inbox/event.go`, `internal/inbox/inbox.go`, `internal/inbox/inbox_test.go`; Modify `internal/monitor/inbound_event.go`, `internal/monitor/inbox.go`, `internal/app/tell.go`, `internal/workevents/builder.go`, `internal/server/*` (session-token const), `internal/archtest/boundary_test.go`.
**Interfaces:** Produces `inbox.InboundEvent`, `inbox.InboxEntry`, `inbox.ReadInboxEntries`, `inbox.AppendInboxEvent`, `inbox.AppendInboxEventStamped`, `inbox.FlowTellEvent`, `inbox.ClassifyInboxEvent`, `inbox.ThreadKey`.

- [ ] **Step 1: Move model + I/O** — relocate `InboundEvent` (+ Kind consts) from `monitor/inbound_event.go` → `inbox/event.go`; relocate `ReadInboxEntries`, `AppendInboxEvent`, `AppendInboxEventStamped`, `FlowTellEvent`, `InboxEntry`, `ClassifyInboxEvent`, `ThreadKey` from `monitor/inbox.go` → `inbox/inbox.go` (`package inbox`). Keep `$FLOW_ROOT`/`$HOME` resolution. Imports: stdlib only (no product).
- [ ] **Step 2: Alias in monitor** — in `internal/monitor`, delete the moved defs; add:

```go
package monitor
import "flow/internal/inbox"
type InboundEvent = inbox.InboundEvent
type InboxEntry   = inbox.InboxEntry
var (
	AppendInboxEvent        = inbox.AppendInboxEvent
	AppendInboxEventStamped = inbox.AppendInboxEventStamped
	FlowTellEvent           = inbox.FlowTellEvent
	ReadInboxEntries        = inbox.ReadInboxEntries
	ClassifyInboxEvent      = inbox.ClassifyInboxEvent
	ThreadKey               = inbox.ThreadKey
)
```

- [ ] **Step 3: Rewire core call-sites** — `app/tell.go`: import `inbox` (not `monitor`); `monitor.AppendInboxEvent`→`inbox.AppendInboxEvent`, `monitor.FlowTellEvent`→`inbox.FlowTellEvent`; replace `server.SessionTokenFileName`→`cli.SessionTokenFileName` (import `cli`, drop `server`). `workevents/builder.go`: import `inbox` (not `monitor`); `monitor.`→`inbox.` for moved symbols.
- [ ] **Step 4: Single source for the token const** — point `internal/server`'s `SessionTokenFileName` at `cli.SessionTokenFileName`.
- [ ] **Step 5: Shrink the ratchet** — remove `"flow/internal/workevents"` from `knownViolations`.
- [ ] **Step 6: Round-trip test** (`internal/inbox/inbox_test.go`)

```go
package inbox
import ("os"; "path/filepath"; "testing"; "time")
func TestAppendAndReadFlowTell(t *testing.T) {
	root := t.TempDir(); t.Setenv("FLOW_ROOT", root)
	os.MkdirAll(filepath.Join(root, "tasks", "demo"), 0o755)
	if err := AppendInboxEvent("demo", FlowTellEvent("parent", "hi", time.Now().UTC())); err != nil { t.Fatal(err) }
	entries, err := ReadInboxEntries("demo")
	if err != nil || len(entries) != 1 { t.Fatalf("got %v / %d", err, len(entries)) }
}
```

- [ ] **Step 7: Verify** — `make test`; `go test ./internal/archtest/ -v` (workevents no longer a violation; app remains).
- [ ] **Step 8: Commit** — `git commit -m "refactor(inbox): extract inbox model to core; tell+workevents drop monitor dep"`

### Task 5: init-hook for steerer persona (closes last app→product import)

**Files:** Create `internal/product/seed.go`; Modify `internal/app/init.go`, `internal/archtest/boundary_test.go`. (Note: `internal/product` is created here; its `init()` wiring is fleshed out in Task 9.)
**Interfaces:** Consumes `app.RegisterInitHook`, `app.FlowRoot`, `steering.DefaultPersonaMarkdown`.

- [ ] **Step 1: Core runs hooks** — in `app/init.go`, delete the `steering` import and the `steering.DefaultPersonaMarkdown` write; after core seeding add:

```go
	for _, hook := range initHooks {
		if err := hook(); err != nil { fmt.Fprintf(os.Stderr, "warning: init hook: %v\n", err) }
	}
```

- [ ] **Step 2: Product seed** (`internal/product/seed.go`) — copy the EXACT persona path from the deleted `init.go` code:

```go
package product
import ("os"; "path/filepath"; "flow/internal/app"; "flow/internal/steering")
func seedSteererPersona() error {
	root, err := app.FlowRoot(); if err != nil { return err }
	p := filepath.Join(root, "persona.md") // EXACT path from the old init.go
	if _, err := os.Stat(p); err == nil { return nil }
	return os.WriteFile(p, []byte(steering.DefaultPersonaMarkdown), 0o644)
}
func init() { app.RegisterInitHook(seedSteererPersona) }
```

- [ ] **Step 3: Empty the ratchet** — `knownViolations` becomes `map[string]bool{}`.
- [ ] **Step 4: Verify** — `go test ./internal/archtest/ -v` PASSES with empty allowlist; `make test`; `flow init` (temp FLOW_ROOT, full binary) still writes `persona.md`.
- [ ] **Step 5: Commit** — `git commit -m "refactor(init): steerer persona via product init-hook; core→product imports now zero"`

### Task 6: Move `ui`/`attention`/`slack` handlers out of `app`

**Files:** Create `internal/product/ui.go`, `internal/product/attention.go`, `internal/product/slack.go` (+ move `slack_test.go`); Modify `internal/product/commands.go` (new), `internal/app/register.go` (drop shims); Delete `internal/app/serve.go`, `attention.go`, `slack.go`.
**Interfaces:** Consumes `app.FlagSet/FlowRoot/FlowDBPath/FlowServerURL/UISessionToken/Version`, `cli.Register`; product impls (`server`/`steering`/`monitor`). Produces `product.cmdUI`, `product.cmdAttention`, `product.cmdSlack` + `product.registerCommands()`.

- [ ] **Step 1: Relocate** the three files into `internal/product/` verbatim, `package app`→`package product`, rewriting core-helper calls to `app.*` wrappers (compiler finds each). For any unexported `app` helper a moved file needs, add a wrapper in `internal/app/exports.go`. `git rm` the three `app` files.
- [ ] **Step 2: Register** — `internal/product/commands.go`:

```go
package product
import "flow/internal/cli"
func registerCommands() {
	cli.Register(cli.Command{Name: "ui", Run: cmdUI, Help: "  flow ui serve [--host] [--port] [--bg]"})
	cli.Register(cli.Command{Name: "serve", Run: cmdUIServe, Hidden: true})
	cli.Register(cli.Command{Name: "attention", Run: cmdAttention, Help: "  flow attention list|act|trace|feedback ..."})
	cli.Register(cli.Command{Name: "slack", Run: cmdSlack, Help: "  flow slack send|react ..."})
}
```
(Called from `product`'s wiring in Task 9. Until then, keep the Task-2 shims so the single binary is unchanged — delete them in Task 9 when the two mains take over.)

- [ ] **Step 3: Verify** — `go build ./...`; `make test`. (Single binary still registers everything via shims; behavior unchanged.)
- [ ] **Step 4: Commit** — `git commit -m "refactor: relocate ui/attention/slack handlers into the product package"`

### Task 7: Migration registry + core/product schema split

**Files:** Modify `internal/flowdb/db.go`, `internal/flowdb/schema.go`, `internal/flowdb/migrations.go`; Create `internal/productdb/schema.go`, `internal/flowdb/schema_domain_test.go`.
**Interfaces:** Produces `flowdb.MigrationSet{Domain string; Apply func(*sql.DB) error}`, `flowdb.RegisterMigrations`; `productdb.Ensure(db *sql.DB) error` (product DDL).

- [ ] **Step 1: Registry in `schema.go`**

```go
type MigrationSet struct { Domain string; Apply func(db *sql.DB) error }
var registeredSets []MigrationSet
func RegisterMigrations(set MigrationSet) { registeredSets = append(registeredSets, set) }
```

- [ ] **Step 2: Split `schemaDDL`** into `coreSchemaDDL` (projects, playbooks, tasks, workdirs, task_tags, task_dependencies, task_links, agent_runtime_states, pending_wakes, brain_runs, search_docs, schema_meta, owners + indexes) and move the product table DDL (attention_*, steering_*, github_*, chats, remote_devices, pending_sends, kb_capture + indexes) into `internal/productdb/schema.go` as `productdb.Ensure`.
- [ ] **Step 3: OpenDB applies core + registered** — in `db.go`: `db.Exec(coreSchemaDDL)`; `runMigrations(db)` (core ALTERs only); then `for _, s := range registeredSets { if err := s.Apply(db); err != nil { ... } }`.
- [ ] **Step 4: Partition `runMigrations`** — move product-table ALTERs/fixups + connector legacy-drops (`monitor_*`, `external_messages`, `slack_oauth_tokens`) into `productdb.Ensure`; core ALTERs/drops stay.
- [ ] **Step 5: productdb registers itself**

```go
package productdb
import ("database/sql"; "flow/internal/flowdb")
func init() { flowdb.RegisterMigrations(flowdb.MigrationSet{Domain: "flowwyyy", Apply: Ensure}) }
func Ensure(db *sql.DB) error { /* product CREATE TABLE + ALTERs */ return nil }
```
(In the single binary today, `productdb` is pulled in transitively by `server`; in Phase 2 the flowwyyy binary imports it explicitly.)

- [ ] **Step 6: Domain test** (`internal/flowdb/schema_domain_test.go`)

```go
package flowdb
import ("database/sql"; "path/filepath"; "testing")
func tableExists(db *sql.DB, name string) bool {
	var n string
	return db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n) == nil
}
func TestCoreSchemaOmitsProductTables(t *testing.T) {
	saved := registeredSets; registeredSets = nil; defer func() { registeredSets = saved }()
	db, err := OpenDB(filepath.Join(t.TempDir(), "core.db")); if err != nil { t.Fatal(err) }
	defer db.Close()
	if !tableExists(db, "tasks") { t.Error("core table tasks missing") }
	if tableExists(db, "attention_feed") { t.Error("product table present in core-only DB") }
}
```

- [ ] **Step 7: Verify** — `make test` (full: all tables; existing tests green); `go test ./internal/flowdb/ -run TestCoreSchemaOmitsProductTables`; **regression:** open a COPY of a real `flow.db` with the full build — no errors, no data loss.
- [ ] **Step 8: Commit** — `git commit -m "refactor(flowdb): split schema into core + product migration sets"`

---

# PHASE 2 — Two binaries that work together (same experience; local core)

### Task 8: `internal/flowclient` — exec layer + version-compat; `flow version --json`

**Files:** Create `internal/flowclient/resolve.go`, `client.go`, `compat.go`, `*_test.go`; Modify `internal/app/app.go` (or a core `version` handler) to support `flow version --json`.
**Interfaces:** Produces `flowclient.Resolve() (string, error)`, `flowclient.Client{Bin string}` with `Run(ctx, args...) (stdout, stderr string, code int, err error)` + typed mutators `Do/Done/Add/Update/Archive/Unarchive/Delete/Restore/Spawn/RunPlaybook/PlaybookTickDue/OwnerTickDue`, `flowclient.CheckCompat(bin string, floor Version) error`. Core produces `flow version --json` → `{"version":"…","schema":N,"capabilities":[…]}`.

- [ ] **Step 1: `flow version --json` (core).** In the version handling, support a `--json` flag emitting `{"version":Version,"schema":flowdb.SchemaVersion,"capabilities":[…]}`. Add `flowdb.SchemaVersion` const if absent. Test: `flow version --json | jq .version` equals `flow version`.
- [ ] **Step 2: Failing test** for resolution order (`flowclient/resolve_test.go`): with `$FLOW_BIN` set to a temp executable, `Resolve()` returns it; unset + a sibling `flow` next to a fake `os.Executable` returns the sibling; else falls to PATH; else error.
- [ ] **Step 3: Implement `resolve.go`**

```go
package flowclient
import ("errors"; "os"; "os/exec"; "path/filepath")
func Resolve() (string, error) {
	if b := os.Getenv("FLOW_BIN"); b != "" {
		if fi, err := os.Stat(b); err == nil && !fi.IsDir() { return b, nil }
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "flow")
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() { return cand, nil }
	}
	if p, err := exec.LookPath("flow"); err == nil { return p, nil }
	return "", errors.New("flow binary not found: set $FLOW_BIN, install flow on PATH, or place it beside flowwyyy")
}
```

- [ ] **Step 4: Implement `client.go`** — `Client{Bin}` with `Run(ctx, args...)` (`exec.CommandContext`, capture stdout/stderr, return exit code) and typed mutators that build the arg list, e.g. `func (c Client) Done(slug string) error { _,e,_,err := c.Run(ctx, "done", slug); ... }`. Mirror today's CLI flags exactly so behavior is preserved.
- [ ] **Step 5: Implement `compat.go`** — parse `flow version --json`; compare `version`+`schema` to an embedded floor; return a clear error if below. Test with a fake `flow` script printing JSON.
- [ ] **Step 6: Verify** — `go test ./internal/flowclient/`; `make test`.
- [ ] **Step 7: Commit** — `git commit -m "feat(flowclient): exec wrapper + abs-path resolution + version-compat; flow version --json"`

### Task 9: Swap server in-process mutations → `flowclient` (the dedup; parity-critical)

**Files:** Modify `internal/server/actions.go` (the `runFlowCommand` sites), `internal/server/terminal_launch.go`; create `internal/product/product.go` (wiring). 
**Interfaces:** Consumes `flowclient.Client`. The server gains a `*flowclient.Client` (or `Config.Flow`).

- [ ] **Step 1: Inject a `flowclient.Client` into the server.** Add `Flow flowclient.Client` to `server.Config`; `cmd/flowwyyy` (Task 10) resolves the bin and passes it.
- [ ] **Step 2: Replace `runFlowCommand` call-sites** in `actions.go` with the typed `flowClient.*` mutators (archive/unarchive/done/delete/restore/create/fork/playbook-schedule). Behavior must be identical (same `flow` subcommands, same args).
- [ ] **Step 3: Replace in-process launch** in `terminal_launch.go` — instead of directly mutating task rows / creating worktrees / setting provider+session fields, call `flowClient.Do(slug, opts)` so launch goes through the SAME path as `flow do`. (This kills the duplicated launch logic — the core point of the exercise.) Preserve the UI terminal-attach behavior (the WS bridge attaches to the session `flow do` launched).
- [ ] **Step 4: Product wiring** (`internal/product/product.go`)

```go
package product
import _ "flow/internal/productdb" // registers product migrations
func registerInitHooks() {} // seed.go's init() already registers
func registerSkill()     {} // Task 11
// registerCommands() is in commands.go; called by cmd/flowwyyy
```

- [ ] **Step 5: Verify parity** — build the single binary; `make test`; **manual:** in Mission Control, the Done/Archive/Delete/Restore buttons and "open session" (terminal launch) behave exactly as before, now routed through `flowClient`. Terminal attach + scrollback unchanged.
- [ ] **Step 6: Commit** — `git commit -m "refactor(server): route mutations + session launch through flowclient (dedup launch logic)"`

### Task 10: Two `main` packages + passthrough (preserve full command surface)

**Files:** Create `cmd/flow/main.go`, `cmd/flowwyyy/main.go`, `internal/product/passthrough.go`; Delete root `main.go`; Modify `internal/app/register.go` (drop the ui/serve/attention/slack shims — the flowwyyy main owns them now).
**Interfaces:** `cmd/flow` registers core; `cmd/flowwyyy` registers product + passthrough + product migrations + compat check.

- [ ] **Step 1: `cmd/flow/main.go`** (core binary)

```go
package main
import ("os"; "flow/internal/app")
var version = "dev"
func main() { app.Version = version; os.Exit(app.Run(os.Args[1:])) }
```
(`app`'s `init()`→`registerCore()` registers only CORE verbs now; remove the ui/serve/attention/slack shims from `register.go`.)

- [ ] **Step 2: `internal/product/passthrough.go`** — preserve the FULL surface so the user experience is unchanged: any verb not natively a product command execs `flow`.

```go
package product
import ("os"; "flow/internal/cli"; "flow/internal/flowclient")
// RunWithPassthrough dispatches a product command if registered, else execs flow.
func RunWithPassthrough(bin string, args []string) int {
	if len(args) == 0 { /* print combined usage */ return 0 }
	if c, ok := cli.Lookup(args[0]); ok { return c.Run(args[1:]) }
	// Core verb → exec official flow, streaming stdio so experience is identical.
	return flowclient.Exec(bin, args) // inherits stdin/stdout/stderr, returns exit code
}
```
Add `flowclient.Exec(bin string, args []string) int` (uses `syscall.Exec`-style stdio passthrough via `cmd.Stdin/out/err = os.*`).

- [ ] **Step 3: `cmd/flowwyyy/main.go`** (product binary)

```go
package main
import ("fmt"; "os"; "flow/internal/flowclient"; "flow/internal/product")
var version = "dev"
func main() {
	bin, err := flowclient.Resolve()
	if err != nil { fmt.Fprintln(os.Stderr, "error:", err); os.Exit(1) }
	if err := flowclient.CheckCompat(bin, flowclient.RequiredFloor); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err); os.Exit(1)
	}
	product.RegisterCommandsAndMigrations() // wraps registerCommands(); productdb init() already ran
	os.Exit(product.RunWithPassthrough(bin, os.Args[1:]))
}
```

- [ ] **Step 4: Makefile** — `build` builds both: `go build -o flow ./cmd/flow && go build -o flowwyyy ./cmd/flowwyyy`.
- [ ] **Step 5: Verify the boundary + parity** — `go build ./cmd/flow ./cmd/flowwyyy`; `go test ./internal/archtest/` (add a check that `cmd/flowwyyy` + product packages do NOT import `flow/internal/app`/`flowdb` except where transitional — see Task 13). Run BOTH binaries: `./flowwyyy do <task>` execs `./flow do <task>` (passthrough); `./flowwyyy ui serve` runs product; outputs identical to today's `flow …`.
- [ ] **Step 6: Commit** — `git commit -m "build: split cmd/flow + cmd/flowwyyy; flowwyyy passthrough preserves full command surface"`

### Task 11: Skill differentiation (preserve agent experience)

**Files:** Rename `internal/app/skill/SKILL.md`→`SKILL.core.md`; Create `internal/product/skill/SKILL.flowwyyy.md`, `internal/product/skill.go`; Modify `internal/app/skill.go` (add `flow skill print`; core install uses core fragment), and the hook-wiring strings.
**Interfaces:** Core: `flow skill print` (emits raw core skill), `flow skill install` (core fragment). Product: `flowwyyy skill install` composes `flow skill print` output + the product fragment.

- [ ] **Step 1: Split the skill** — cut §4.9b + §10a–§10e (and product verbs from §4) out of `SKILL.md` into `internal/product/skill/SKILL.flowwyyy.md` (prefixed `\n\n---\n\n## Product extensions (flowwyyy)\n`). Rename remainder → `SKILL.core.md`. Update `//go:embed skill/SKILL.md`→`skill/SKILL.core.md`.
- [ ] **Step 2: `flow skill print`** — add a `print` subcommand to `cmdSkill` that writes `embeddedCoreSkill` to stdout (used by flowwyyy to compose without duplicating the core skill text).
- [ ] **Step 3: `flowwyyy skill install`** — `internal/product/skill.go` embeds `SKILL.flowwyyy.md`; `flowwyyy skill install` execs `flow skill print`, appends the product fragment, writes the composed `SKILL.md` to the Claude/Codex skill paths, and wires the SessionStart hook. Same install destinations as today → agent experience unchanged.
- [ ] **Step 4: Composition test** — assert the composed skill (core+product) contains both `## 1. What flow is` and `## 10d. Attention Router feed`; core-only `flow skill print` contains the former, not the latter.
- [ ] **Step 5: Verify parity** — install via `flowwyyy skill install`; diff the composed `~/.claude/skills/flow/SKILL.md` against today's — sections present and equivalent.
- [ ] **Step 6: Commit** — `git commit -m "refactor(skill): split core/product fragments; flowwyyy composes via flow skill print"`

### Task 12: Phase-2 behavior-parity acceptance gate

**Files:** Modify `docs/architecture/flow-core-decoupling-seam.md` (mark Phase 2 done + parity results).

- [x] **Step 1: Static gates** — `go vet ./...` clean; `make test` 29/29; `go test ./internal/archtest/` empty allowlist + `cmd/flow` product-free; `make build` both binaries. *(The no-`app`-import-from-product guard is a Phase-3 / T13 check, correctly not part of this gate.)* ✅
- [x] **Step 2: Parity matrix (against locally-built `flow` + `flowwyyy`).** Automated: CLI 44/44 identical (init/add/show/list/update/archive·unarchive·delete·restore/search/standup/owner/spawn/tell/wait/workdir/transcript/backup/version·json + unknown-verb); product `ui serve` (boots, UI 200 + token injection, `productdb` API, flowclient→core), `attention`/`slack` native; skill compose content-complete; SessionStart hook byte-identical; GitHub webhook endpoint wired. **Manual checklist** (live Slack reaction→task; GitHub webhook→task end-to-end; `do` regular/skip/auto + real agent bootstrap; browser terminal attach/scrollback + Done/Archive/open-session) — handed to operator. ✅ automated / ⏳ manual
- [x] **Step 3: Document** — `~/.flow/tasks/twobin-parity-gate/artifacts/PARITY-REPORT.md` + seam doc §10. Found & fixed 2 defects (server test-harness `go run .`; `flowwyyy skill install` core-only wiring), 1 benign observation (dead persona hook). ✅
- [ ] **Step 4: Commit** — `git commit -m "docs: Phase 2 complete — two binaries, behavior parity verified"` *(pending operator approval; also carries the 2 gate fixes)*

---

# PHASE 3 — Official flow dependency + Homebrew packaging

> **Gating reality (measured 2026-06-23 against `Facets-cloud/flow` @ `f59f6a8`; full evidence in `docs/architecture/flow-core-upstream-delta.md`).**
> flowwyyy's core has diverged **far** from official flow, **bidirectionally**. Official flow is missing **8 core commands** flowwyyy relies on (`search standup delete restore spawn wait backup tell`), **6 core tables** (`brain_runs task_dependencies task_links agent_runtime_states pending_wakes search_docs`) + column additions, **12 core packages**, and the **shared commands are substantially rewritten** (`do.go` ≈1186 difflines, `update.go` 724, `show.go` 289…). Upstream separately added `stats`/`card` flowwyyy lacks.
>
> **Consequence:** depending on *current* official flow would BREAK "same experience." The dependency switch is a **substantial upstreaming milestone**, not a single task, and needs a **strategy decision** first:
> - **Route A (recommended):** flowwyyy's (more-advanced) core becomes the basis of official `Facets-cloud/flow`; cherry-pick upstream `stats`/`card` if wanted. Phase 3 = "publish our core as official flow." Lower risk, preserves behavior.
> - **Route B:** reconcile flowwyyy onto current upstream as the base (bidirectional merge; higher cost; risks losing flowwyyy core gains).
>
> **Phases 1–2 are unaffected** — they deliver the working two-binary, same-experience product against a **local** extracted core. Phase 3 below assumes Route A and is sized as its own milestone, scheduled after the local two-binary build is proven.

### Task 13: Enforce no `app`/`flowdb` import from product; flowwyyy owns its read layer

**Files:** Create `internal/productdb/read.go`; Modify `internal/server/*` reads; extend `internal/archtest/boundary_test.go`.
**Why:** official `Facets-cloud/flow` keeps code under `internal/`, so flowwyyy can never import flow's Go packages. flowwyyy must read the DB with its OWN layer.
**Interfaces:** Produces `productdb` read queries (task/attention/etc. structs + `SELECT`s) used by `server` instead of `flowdb`.

- [ ] **Step 1: Strengthen the guard** — add a test asserting `cmd/flowwyyy` + `server`/`monitor`/`steering`/`product` transitive deps do NOT include `flow/internal/app` or `flow/internal/flowdb`. Initially it FAILS (server imports flowdb) — record current importers as a second ratchet.
- [ ] **Step 2: Move read queries into `productdb`** — port the `flowdb` read calls the server uses (task/project/attention/feed lists) into `internal/productdb/read.go` as flowwyyy's own queries against the shared schema. Server reads via `productdb`, not `flowdb`. (Writes already go through `flowclient` from Task 9.)
- [ ] **Step 3: Shrink/empty the second ratchet** as importers move; the gate passes when product no longer imports `app`/`flowdb`.
- [ ] **Step 4: Verify** — `make test`; parity (Mission Control reads identical); guard passes.
- [ ] **Step 5: Commit** — `git commit -m "refactor(productdb): flowwyyy owns its DB read layer; product no longer imports core Go"`

### Task 14: Upstream flowwyyy's core into official `Facets-cloud/flow` (Route A milestone)

**Files:** `docs/architecture/flow-core-upstream-delta.md` (done — the measured delta); a series of PRs against `Facets-cloud/flow`.
**Why:** for the dependency switch to preserve "same experience," official flow must ship every core capability flowwyyy relies on. The delta is measured (see the gating note); this is a milestone, not one commit.
**Prereq:** Route A vs Route B decision confirmed by the owner/CTO (see the Phase-3 gating note).

- [ ] **Step 1: Confirm Route + base.** Confirm flowwyyy's core is canonical (Route A); decide whether upstream `stats` and `card`/`card_png` are adopted into our core or dropped.
- [ ] **Step 2: Land the 6 core tables** (`brain_runs`, `task_dependencies`, `task_links`, `agent_runtime_states`, `pending_wakes`, `search_docs`) + the `tasks`/`projects` column additions into `Facets-cloud/flow`'s schema, as additive migrations.
- [ ] **Step 3: Land the 8 core commands** (`search`, `standup`, `delete`, `restore`, `spawn`, `wait`, `backup`, `tell`) and their packages (`flowbackup`, `inbox`, `briefing`, `memorysrc`, `agents`, `agenthooks`, `worktree`, `workdirreg`, `workevents`, `schedule`, `ghpr`, `ghref`, `cli`) into official flow.
- [ ] **Step 4: Reconcile the rewritten shared files** (`do.go`, `update.go`, `show.go`, `list.go`, `add.go`, `init.go`, `done.go`) — flowwyyy's versions become canonical; preserve any upstream-only behavior worth keeping (per Step 1).
- [ ] **Step 5: Cut an official `flow` release** that includes all of the above; set `flowclient.RequiredFloor` to that version + schema number.
- [ ] **Step 6: Verify** — flowwyyy runs against the released official flow with the FULL Task-12 parity matrix; the compat check rejects anything below `RequiredFloor`.
- [ ] **Step 7: Commit** — `git commit -m "feat: target official flow at RequiredFloor; core upstreamed"`

### Task 15: Homebrew — depend on official flow, do not bundle

**Files:** Modify the `vishnukv-facets/homebrew-flowwyyy` formula; `docs`.

- [ ] **Step 1: Formula `depends_on`** — add `depends_on "facets-cloud/flow/flow"` (or the official tap path) to the flowwyyy formula; ensure flow is NOT vendored/bundled in the flowwyyy tarball.
- [ ] **Step 2: Resolution + floor** — flowwyyy resolves the brew-installed `flow` (PATH/opt prefix) and enforces `RequiredFloor` at startup with a clear message pointing at `brew upgrade flow`.
- [ ] **Step 3: Skill/hook wiring** — `brew install flowwyyy` (post-install or first run) installs the composed skill via `flowwyyy skill install`; the SessionStart hook points at the right binaries.
- [ ] **Step 4: Verify on a clean machine/VM** — `brew install` pulls flow + flowwyyy; full parity matrix (Task 12) passes; removing flow yields the clear compat error.
- [ ] **Step 5: Commit** — `git commit -m "build(brew): flowwyyy depends_on official flow; not bundled"`

### Task 16: Final acceptance — same experience, end to end

- [ ] **Step 1:** Run the full Task-12 parity matrix on the brew-installed pair (official `flow` + `flowwyyy`).
- [ ] **Step 2:** Confirm independent upgrade: `brew upgrade flow` to a newer compatible release without touching flowwyyy → parity holds; an incompatible flow → clear compat error, no silent breakage.
- [ ] **Step 3:** Update `docs/architecture/flow-core-decoupling-seam.md` → "complete"; note `RequiredFloor`, the guard tests, and the parity matrix as the regression suite.
- [ ] **Step 4: Commit** — `git commit -m "docs: decoupling complete — official flow dependency, same experience verified"`

---

## Self-Review

**Spec coverage:** boundary (T1 guard); command registry (T2); helpers/init-hook (T3); inbox extraction (T4); persona init-hook (T5); ui/attention/slack relocation (T6); migration/schema split (T7); flowclient + version-json (T8); in-process→exec dedup incl. terminal_launch (T9); two mains + passthrough preserving full surface (T10); skill split + compose (T11); Phase-2 parity gate (T12); product-owns-reads, no core Go import (T13); upstream-delta reconciliation (T14); Homebrew depends_on official flow, not bundled (T15); final same-experience acceptance + independent upgrade (T16). Same-experience constraint enforced by parity steps in T2/T6/T9/T11 and gates T12/T16. ✓

**Placeholder scan:** new code shown in full; relocations name exact files + exact rewrites; the one judgement call (persona path, T5) flagged to copy verbatim; T14 delta is measured, not guessed. No TBD/TODO. ✓

**Type consistency:** `cli.Command/Register/Lookup/Each/SessionTokenFileName`, `app.RegisterInitHook/FlagSet/FlowRoot/FlowDBPath/FlowServerURL/UISessionToken`, `inbox.*`, `flowdb.MigrationSet/RegisterMigrations/SchemaVersion`, `productdb.Ensure/read`, `flowclient.Resolve/Client/CheckCompat/RequiredFloor/Exec`, `product.registerCommands/RunWithPassthrough` used consistently across defining/consuming tasks. ✓

## Risks
- **Parity regressions from in-process→exec (T9).** Mitigation: typed mutators mirror today's exact `flow` subcommands; T9/T12 parity checks; terminal-attach behavior unchanged (only launch is delegated).
- **`flow` not found / incompatible at runtime.** Mitigation: `flowclient.Resolve` order + `CheckCompat` with clear `brew upgrade flow` guidance.
- **Upstream delta is LARGE and bidirectional (T14) — measured, not hypothetical.** 8 core commands + 6 core tables + 12 packages + heavily-rewritten shared files separate flowwyyy's core from official flow (`docs/architecture/flow-core-upstream-delta.md`). Mitigation: Phases 1–2 ship the full two-binary, same-experience product against a **local** extracted core, so value is delivered *before* the upstreaming milestone. Phase 3 is gated on a Route A/B decision and scheduled as its own milestone; the `flowclient` version-floor prevents flowwyyy from ever running on a too-old official flow.
- **Schema co-tenancy:** official flow must not drop flowwyyy's product tables. Mitigation: documented compatibility contract + the schema_domain test; product tables are additive.
- **exec latency on writes.** Acceptable: writes are low-frequency; reads stay in-process via `productdb`.
