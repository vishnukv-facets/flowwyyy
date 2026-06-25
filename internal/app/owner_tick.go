package app

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"flow/internal/flowdb"
	"flow/internal/spawner"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var ownerTickRunner = func(provider, workDir, flowRootPath, prompt string) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = sessionProviderClaude
	}
	if provider == sessionProviderCodex {
		cmd := commandRunner("codex", codexExecCLIArgs(workDir, flowRootPath, "bypass", "", "")...)
		cmd.Stdin = strings.NewReader(prompt)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Dir = workDir
		cmd.Env = autoRunEnv(flowRootPath, "", provider, "bypass")
		return cmd.Run()
	}
	// Claude: run in streaming mode and distill each event into a readable line
	// written live to this process's stdout — which, for the detached
	// `__owner-tick`, is the tick log file. So the tick log fills in real time
	// (tool calls, reasoning, result) instead of staying empty until the
	// one-shot `claude -p` finally dumps its result at exit. This is what makes
	// "what is the tick doing right now" visible while it runs.
	cmd := exec.Command("claude", "-p", prompt, "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions")
	cmd.Dir = workDir
	cmd.Env = autoRunEnv(flowRootPath, "", provider, "bypass")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	streamTickActivity(stdout, os.Stdout)
	return cmd.Wait()
}

// tickStreamEvent is the subset of Claude's --output-format stream-json events
// the tick activity log cares about.
type tickStreamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

// streamTickActivity reads Claude's NDJSON event stream and writes a compact,
// human-readable activity line per meaningful event (start, reasoning, tool
// call, result), skipping hook/rate-limit/tool-result noise.
func streamTickActivity(r io.Reader, w io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev tickStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" {
				fmt.Fprintln(w, "▸ tick started")
			}
		case "assistant":
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					if t := oneLine(c.Text, 280); t != "" {
						fmt.Fprintln(w, t)
					}
				case "tool_use":
					fmt.Fprintf(w, "→ %s %s\n", c.Name, toolUseBrief(c.Input))
				}
			}
		case "result":
			if ev.IsError {
				fmt.Fprintln(w, "✗ tick ended with an error")
			} else if t := oneLine(ev.Result, 280); t != "" {
				fmt.Fprintf(w, "✓ %s\n", t)
			} else {
				fmt.Fprintln(w, "✓ tick finished")
			}
		}
	}
}

// toolUseBrief renders a one-line summary of a tool call for the activity log.
func toolUseBrief(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, key := range []string{"command", "file_path", "path", "pattern", "query", "url"} {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return oneLine(v, 160)
		}
	}
	return ""
}

// oneLine collapses whitespace/newlines and truncates for a single log line.
func oneLine(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if max > 0 && len(s) > max {
		s = strings.TrimSpace(s[:max-1]) + "…"
	}
	return s
}

var ownerTickLauncher = func(slug, workDir, logPath string, env []string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate flow binary: %w", err)
	}
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer logF.Close()

	cmd := exec.Command(self, "__owner-tick", slug)
	cmd.Dir = workDir
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start owner tick: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

var ownerInteractiveLauncher = func(o *flowdb.Owner, prompt string) error {
	if o.Harness == sessionProviderCodex {
		return errors.New("interactive owner ticks are currently supported only for claude owners; use --auto for codex")
	}
	sessionID, err := newUUID()
	if err != nil {
		return fmt.Errorf("new session id: %w", err)
	}
	command := agentShellCommand("claude", []string{"--session-id", sessionID, prompt})
	return spawner.SpawnTab("owner: "+o.Slug, o.WorkDir, command, flowSessionEnv(os.Getenv("FLOW_ROOT")))
}

func ownerTickManual(args []string) int {
	if len(args) == 0 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "error: owner tick requires an owner slug")
		return 2
	}
	slug := args[0]
	fs := flagSet("owner tick")
	auto := fs.Bool("auto", false, "run the tick headlessly in the background")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	dbPath, err := flowDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	db, err := openConcurrentDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()
	o, err := flowdb.GetOwner(db, slug)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fmt.Fprintf(os.Stderr, "error: no owner %q\n", slug)
			return 1
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if o.Status != "active" {
		fmt.Fprintf(os.Stderr, "error: owner %q is %s; run `flow owner start %s` before ticking it\n", slug, o.Status, slug)
		return 1
	}
	if *auto {
		return dispatchOwnerTick(db, o, false)
	}

	ownerDir, err := ownerDirFor(slug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if err := ownerInteractiveLauncher(o, buildOwnerTickPromptInteractive(slug, ownerDir)); err != nil {
		fmt.Fprintf(os.Stderr, "error: spawn interactive tick: %v\n", err)
		return 1
	}
	if err := recordOwnerInteractiveTick(db, slug); err != nil {
		fmt.Fprintf(os.Stderr, "warning: record interactive tick for %q: %v\n", slug, err)
	}
	fmt.Printf("opened an interactive tick for owner %q\n", slug)
	return 0
}

func ownerTickDue(args []string) int {
	fs := flagSet("owner tick-due")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dbPath, err := flowDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	db, err := openConcurrentDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()
	due, err := flowdb.DueOwners(db, flowdb.NowISO())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	dispatched := 0
	for _, o := range due {
		if dispatchOwnerTick(db, o, true) == 0 {
			dispatched++
		}
	}
	fmt.Printf("dispatched %d owner tick(s)\n", dispatched)
	return 0
}

func dispatchOwnerTick(db *sql.DB, o *flowdb.Owner, advance bool) int {
	if o.TickPID.Valid && processAlive(int(o.TickPID.Int64)) && !ownerTickStale(o) {
		if !advance {
			fmt.Fprintf(os.Stderr, "error: owner %q already has a tick running (pid %d)\n", o.Slug, o.TickPID.Int64)
			return 1
		}
		return 2
	}
	root, err := flowRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	now := time.Now()
	var nextWakeAt string
	if advance {
		dur, err := time.ParseDuration(o.Every)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: owner %q has invalid every %q: %v\n", o.Slug, o.Every, err)
			return 1
		}
		nextWakeAt = now.Add(dur).Format(time.RFC3339)
	}
	ticksDir := filepath.Join(root, "owners", o.Slug, "ticks")
	if err := os.MkdirAll(ticksDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: mkdir ticks for %q: %v\n", o.Slug, err)
		return 1
	}
	logPath := filepath.Join(ticksDir, now.UTC().Format("2006-01-02-150405")+".log")
	pid, err := ownerTickLauncher(o.Slug, o.WorkDir, logPath, autoChildEnv())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: launch tick for %q: %v\n", o.Slug, err)
		return 1
	}
	started := now.Format(time.RFC3339)
	if advance {
		err = recordOwnerTickDispatched(db, o.Slug, pid, started, nextWakeAt)
	} else {
		err = recordOwnerTickStarted(db, o.Slug, pid, started)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: record tick for %q: %v\n", o.Slug, err)
		return 1
	}
	if !advance {
		fmt.Printf("dispatched a headless tick for owner %q (pid %d)\n", o.Slug, pid)
	}
	return 0
}

func cmdOwnerTick(args []string) int {
	if len(args) == 0 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "error: __owner-tick requires an owner slug")
		return 2
	}
	slug := args[0]
	dbPath, err := flowDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	db, err := openConcurrentDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer db.Close()
	o, err := flowdb.GetOwner(db, slug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load owner %q: %v\n", slug, err)
		return 1
	}
	if o.Status != "active" {
		_ = clearOwnerTickBookkeeping(db, slug)
		fmt.Printf("owner %q is %s; skipping tick\n", slug, o.Status)
		return 0
	}
	ownerDir, err := ownerDirFor(slug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		_ = recordOwnerTick(db, slug, "error")
		return 1
	}
	root, _ := flowRoot()
	err = ownerTickRunner(o.Harness, o.WorkDir, root, buildOwnerTickPrompt(slug, ownerDir, o.Harness))
	status := "ok"
	if err != nil {
		status = "error"
	}
	if recErr := recordOwnerTick(db, slug, status); recErr != nil {
		fmt.Fprintf(os.Stderr, "warning: record tick for %q: %v\n", slug, recErr)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "owner tick for %q failed: %v\n", slug, err)
		return 1
	}
	fmt.Printf("owner tick for %q finished: %s\n", slug, status)
	return 0
}

const ownerTickStaleAfter = time.Hour

func ownerTickStale(o *flowdb.Owner) bool {
	if !o.TickStarted.Valid || o.TickStarted.String == "" {
		return false
	}
	started, err := time.Parse(time.RFC3339, o.TickStarted.String)
	if err != nil {
		return false
	}
	return time.Since(started) > ownerTickStaleAfter
}

func ownerDirFor(slug string) (string, error) {
	root, err := flowRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "owners", slug), nil
}

func clearOwnerTickBookkeeping(db *sql.DB, slug string) error {
	_, err := db.Exec(`UPDATE owners SET tick_pid=NULL, tick_started=NULL, updated_at=? WHERE slug=?`, flowdb.NowISO(), slug)
	return err
}

func recordOwnerTickStarted(db *sql.DB, slug string, pid int, started string) error {
	_, err := db.Exec(`UPDATE owners SET tick_pid=?, tick_started=?, updated_at=? WHERE slug=?`, pid, started, flowdb.NowISO(), slug)
	return err
}

func recordOwnerTickDispatched(db *sql.DB, slug string, pid int, started, nextWakeAt string) error {
	_, err := db.Exec(`UPDATE owners SET tick_pid=?, tick_started=?, next_wake_at=?, updated_at=? WHERE slug=?`, pid, started, nextWakeAt, flowdb.NowISO(), slug)
	return err
}

func recordOwnerInteractiveTick(db *sql.DB, slug string) error {
	now := flowdb.NowISO()
	_, err := db.Exec(`UPDATE owners SET last_tick_at=?, last_tick_status='interactive', updated_at=? WHERE slug=?`, now, now, slug)
	return err
}

func recordOwnerTick(db *sql.DB, slug, status string) error {
	now := flowdb.NowISO()
	_, err := db.Exec(`UPDATE owners SET last_tick_at=?, last_tick_status=?, tick_pid=NULL, tick_started=NULL, updated_at=? WHERE slug=?`, now, status, now, slug)
	return err
}

func reconcileOwnerTick(db *sql.DB, o *flowdb.Owner) {
	if o == nil || !o.TickPID.Valid {
		return
	}
	if processAlive(int(o.TickPID.Int64)) && !ownerTickStale(o) {
		return
	}
	now := flowdb.NowISO()
	if _, err := db.Exec(
		`UPDATE owners SET last_tick_status='dead', last_tick_at=COALESCE(last_tick_at, ?),
		 tick_pid=NULL, tick_started=NULL, updated_at=? WHERE slug=? AND tick_pid IS NOT NULL`,
		now, now, o.Slug,
	); err != nil {
		return
	}
	o.LastTickStatus = sql.NullString{String: "dead", Valid: true}
	if !o.LastTickAt.Valid {
		o.LastTickAt = sql.NullString{String: now, Valid: true}
	}
	o.TickPID = sql.NullInt64{}
	o.TickStarted = sql.NullString{}
}

// ownerModelGuidance tells a dispatching owner how to pick `--model` for the
// tasks it creates, so autonomous work runs on a model matched to its
// difficulty instead of always the default tier. The model names are
// provider-specific (the dispatched tasks inherit the owner's harness).
func ownerModelGuidance(agentFlag string) string {
	if agentFlag == "codex" {
		return "Match the model to the work with `--model`: `gpt-5.5` for hard, risky, or architectural tasks; omit `--model` for routine work (defaults to gpt-5.4); `gpt-5.4-mini` for trivial mechanical edits. High-priority tasks (`--priority high`) auto-upgrade one tier when you do not pin a model."
	}
	return "Match the model to the work with `--model`: `opus` for hard, risky, or architectural tasks; omit `--model` for routine work (defaults to sonnet); `haiku` for trivial mechanical edits. High-priority tasks (`--priority high`) auto-upgrade one tier when you do not pin a model."
}

func buildOwnerTickPromptInteractive(slug, ownerDir string) string {
	return fmt.Sprintf(
		"You are running one interactive tick of flow owner %q with the user present. Invoke the flow skill first. Read %s/charter.md and recent notes under %s/updates/. Review current owned work with `flow owner show %s`. Orchestrate work through flow tasks and playbooks; do not perform durable work inline. Use `flow add task \"<what>\" --agent claude --tag owner:%s` for one-time work, add `--tag question` for human decisions, and `flow do --auto <task>` when it can run unattended. %s Before finishing, write a short journal note under %s/updates/ and self-pace with `flow owner next %s --in <duration>`.",
		slug, ownerDir, ownerDir, slug, slug, ownerModelGuidance("claude"), ownerDir, slug,
	)
}

func buildOwnerTickPrompt(slug, ownerDir, harnessName string) string {
	agentFlag := "claude"
	if harnessName == sessionProviderCodex {
		agentFlag = "codex"
	}
	return fmt.Sprintf(
		"You are the autonomous owner %q running one headless tick. Invoke the flow skill first. No human is watching: do not ask for input and do not wait.\n\n"+
			"Read your charter at %s/charter.md and recent journal notes under %s/updates/. Review all owned work with `flow owner show %s` so you do not duplicate in-progress tasks or playbook runs.\n\n"+
			"Orchestrate only. Do not execute substantive work inline in this tick. Dispatch one-time work with `flow add task \"<what>\" --agent %s --tag owner:%s` then `flow do --auto <task>`. %s Park human decisions with `flow add task \"<question>\" --agent %s --tag question --tag owner:%s`. Trigger recurring work through playbooks and tag any run owner:%s.\n\n"+
			"Before exiting, write a concise journal note under %s/updates/ with observations, dispatched slugs, and what to check next. Then self-pace with `flow owner next %s --in <duration>` or `--at <RFC3339>`. Keep the tick short and exit.",
		slug, ownerDir, ownerDir, slug, agentFlag, slug, ownerModelGuidance(agentFlag), agentFlag, slug, slug, ownerDir, slug,
	)
}
