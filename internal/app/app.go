// Package app implements the flow CLI — personal task and agent session
// manager backed by SQLite.
package app

import (
	"encoding/json"
	"flow/internal/flowdb"
	"fmt"
	"os"

	"flow/internal/cli"
)

// Version holds the binary version string, set by main.go from a
// `-ldflags -X main.version=<tag>` build. Defaults to "dev" if main
// never assigns it (e.g. tests linking the package directly).
var Version = "dev"

// Run is the entry point for the CLI. Returns an exit code.
func Run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 0
	}
	cmd, rest := args[0], args[1:]

	// Auto-upgrade the skill + SessionStart hook if the binary version
	// has changed since the last install. Skipped for `init`, `skill`,
	// and `--version` — those manage the skill themselves or need to
	// run before any install state exists. See maybeAutoUpgradeSkill.
	switch cmd {
	case "init", "skill", "--version", "-v", "version", "-h", "--help", "help", "__auto-exec", "__owner-tick":
		// no auto-upgrade
	default:
		maybeAutoUpgradeSkill()
	}

	// Special cases handled directly (not in the command registry).
	switch cmd {
	case "--version", "-v", "version":
		return cmdVersion(rest)
	case "-h", "--help", "help":
		printUsage()
		return 0
	}

	// Every other verb dispatches through the shared cli registry, populated
	// by registerCore() in register.go.
	if c, ok := cli.Lookup(cmd); ok {
		return c.Run(rest)
	}
	fmt.Fprintf(os.Stderr, "error: unknown subcommand %q\n", cmd)
	printUsage()
	return 2
}

func cmdVersion(args []string) int {
	if len(args) == 1 && args[0] == "--json" {
		payload := struct {
			Version      string   `json:"version"`
			Schema       int      `json:"schema"`
			Capabilities []string `json:"capabilities"`
		}{
			Version: Version,
			Schema:  flowdb.SchemaVersion,
			Capabilities: []string{
				"version-json",
				"tasks",
				"projects",
				"playbooks",
				"owners",
				"auto-runs",
				"backups",
			},
		}
		if err := json.NewEncoder(os.Stdout).Encode(payload); err != nil {
			fmt.Fprintf(os.Stderr, "error: encode version: %v\n", err)
			return 1
		}
		return 0
	}
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: flow version [--json]")
		return 2
	}
	fmt.Println(Version)
	return 0
}

func printUsage() {
	fmt.Println(`flow — personal task and agent session manager

Setup:
  flow init
  flow skill install [--force]
  flow skill uninstall
  flow skill update
  flow skill print

Create:
  flow add project "<name>" --work-dir <path> [--slug <s>] [--priority h|m|l] [--mkdir]
  flow add task    "<name>" [--slug <s>] [--project <slug>] [--work-dir <path>] [--mkdir] [--priority h|m|l] [--due <date>] [--agent claude|codex] [--tag <t> ...] [--permission-mode default|auto|bypass]
  flow add owner   "<name>" --work-dir <path> [--project <slug>] [--every 24h] [--agent claude|codex]

Sessions:
  flow do                <ref> [--agent claude|codex] [--fresh] [--dangerously-skip-permissions]
  flow do --auto         <ref> [--with "<instruction>"|--with-file <path>]  (headless autonomous run; no tab; Claude or Codex)
  flow done              <ref>
  flow hook session-start                      (SessionStart hook handler — wire via ~/.claude/settings.json)
  flow hook agent-event --provider claude|codex (forwards lifecycle hooks to the local UI)

Read:
  flow ui serve        [--host 127.0.0.1] [--port 8787] [--bg] (local web Mission Control UI)
  flow standup         [--for today|monday|24h] [--clipboard]   (copyable daily briefing)
  flow search "<query>" [--in briefs,updates,memories,transcripts] [--limit N] [--format table|json|tsv]
  flow show task       [<ref>]
  flow show project    [<ref>]
  flow show owner      <slug>
  flow transcript      [<ref>] [--compact]           (readable transcript from session jsonl)
  flow list tasks    [--status ...] [--project ...] [--priority ...] [--tag <t>] [--since ...] [--include-archived] [--include-deleted|--deleted]
  flow list projects [--status ...] [--include-archived] [--include-deleted|--deleted]
  flow list owners   [--status active|paused|retired] [--include-archived]
  flow list tags                                            (every tag in use, with per-tag task counts)
  flow attention list      [--status new|acted|dismissed|all]   (review the attention-router feed)
  flow attention act       <id> <make-task|forward|dismiss>
  flow attention feedback  [--group source|channel|author|thread-type|suggested-action|confidence-band]

Edit / mutate:
  flow owner start|pause|retire <slug>
  flow owner tick <slug> [--auto]
  flow owner tick-due
  flow owner next <slug> (--in <duration> | --at <RFC3339>)
  flow edit        <ref>
  flow update task     <ref> [--slug <new>] [--name <new>]
                             [--work-dir <path>] [--mkdir]
                             [--status <s>] [--priority h|m|l]
                             [--assignee <name>] [--clear-assignee]
                             [--due-date <date>] [--clear-due]
                             [--parent <task>] [--clear-parent]
                             [--waiting "<who or what>"] [--clear-waiting]
                             [--tag <t> ...] [--remove-tag <t> ...] [--clear-tags]
  flow update project  <ref> [--slug <new>] [--name <new>] [--work-dir <path>] [--mkdir] [--priority h|m|l]
  flow update playbook <ref> [--slug <new>] [--name <new>] [--work-dir <path>] [--mkdir]
  flow do        <ref> [--agent claude|codex] [--fresh] [--dangerously-skip-permissions] [--force]   (spawn a new tab; --force overrides the live-session guard)
  flow do --here <ref> [--force]                                              (bind THIS Claude/Codex session to the task; --force overwrites a prior binding)
  flow archive   <ref>
  flow unarchive <ref>
  flow delete    <ref>   (soft-delete; hides from normal lists/UI)
  flow restore   <ref>   (restore a soft-deleted task/project/playbook)

Workdirs:
  flow workdir list
  flow workdir add <path> [--name <nickname>]
  flow workdir remove <path>
  flow workdir scan [<root>] [--add]

Playbooks:
  flow add playbook   "<name>" --work-dir <path> [--slug <s>] [--project <slug>] [--mkdir]
  flow run playbook   <slug> [--agent claude|codex] [--dangerously-skip-permissions] [--auto] [--with "<instruction>"|--with-file <path>]
  flow show playbook  <ref>
  flow list playbooks [--project <slug>] [--include-archived] [--include-deleted|--deleted]

Slack:
  flow slack send  --channel <id> --text <message> [--at <when>]   (post now or schedule; requires FLOW_SLACK_WRITES_ENABLED=1)
  flow slack react --channel <id> --ts <ts> [--emoji +1]           (ack a message with an emoji reaction)`)
}
