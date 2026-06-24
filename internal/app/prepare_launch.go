package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"flow/internal/agents"
	"flow/internal/flowdb"
	"flow/internal/workdirreg"
	"flow/internal/worktree"
)

// LaunchPrep is the descriptor emitted by `flow do --prepare-only --json`.
//
// It is the stable cross-binary contract between core's launch preparation and
// the Mission Control terminal bridge (in the flowwyyy product binary): core
// performs the canonical state mutation (status flip, session-id allocation,
// per-task worktree) and reports the resulting session so the bridge can attach
// its own browser pty to it. The bridge unmarshals into its own struct keyed by
// the JSON field names below, so the two sides never share a Go type.
type LaunchPrep struct {
	Slug           string `json:"slug"`
	SessionID      string `json:"session_id"`
	Provider       string `json:"provider"`
	PermissionMode string `json:"permission_mode"`
	WorkDir        string `json:"work_dir"`
	Created        bool   `json:"created"`
	NeedsCapture   bool   `json:"needs_capture"`
}

// runPrepareLaunch is the headless core half of a Mission Control terminal
// launch. It mirrors exactly what the server's prepareTerminalLaunch used to do
// in-process — reconcile a crashed autonomous run (and refuse if one is genuinely
// live), gate startability (skipped for done tasks so revisit/resume is not
// blocked), flip the task to in-progress while allocating (Claude) or clearing
// (Codex) the session id, ensure the per-task git worktree, and bump the workdir
// registry — then prints a LaunchPrep descriptor and exits.
//
// It deliberately does NOT spawn a terminal, install agent hooks, or build CLI
// args: the terminal bridge owns those (browser-flavored args + its own tmux
// session). This is the seam that lets the browser pty go through the SAME core
// launch path as `flow do`, removing the server's reimplementation of it.
// Overview / free-agent / chat launches have no `flow do` analog and stay
// in-process in the server.
func runPrepareLaunch(db *sql.DB, task *flowdb.Task, provider string, fresh, jsonOut bool) int {
	if strings.TrimSpace(task.WorkDir) == "" {
		fmt.Fprintf(os.Stderr, "error: task %q has no work_dir\n", task.Slug)
		return 1
	}

	// Reconcile a crashed autonomous run; refuse if one is genuinely live (an
	// interactive open must not race a running --auto supervisor). Mirrors the
	// server's reconcileAutoRunBeforeTerminalLaunch.
	if task.AutoRunStatus.Valid && task.AutoRunStatus.String == "running" {
		pid := 0
		if task.AutoRunPID.Valid {
			pid = int(task.AutoRunPID.Int64)
		}
		if processAlive(pid) {
			fmt.Fprintf(os.Stderr,
				"error: task %q autonomous run is already running (pid %d); wait for it to finish before opening an interactive session\n",
				task.Slug, pid)
			return 1
		}
		reconcileAutoRun(db, task)
	}

	// Startability gate, skipped for done tasks: a done task only reaches here
	// via revisit/resume and must not be blocked by a now-unfinished dependency.
	if task.Status != "done" {
		if err := flowdb.EnsureTaskStartable(db, task); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
	}

	now := flowdb.NowISO()
	sessionID := strings.TrimSpace(task.SessionID.String)
	// Bootstrap when there is no session yet, --fresh was passed, or the
	// requested provider differs from the stored one. The UPDATE has no
	// status guard (unlike interactive `flow do`) so a done task flips back to
	// in-progress, matching the bridge's revisit behavior.
	needsBootstrap := !hasSessionID(task.SessionID) || fresh ||
		(task.SessionProvider != "" && task.SessionProvider != provider)
	created := needsBootstrap
	if needsBootstrap {
		if provider == agents.ProviderCodex {
			if _, err := db.Exec(
				`UPDATE tasks SET status='in-progress', status_changed_at=?,
					session_provider='codex', harness='codex', session_id=NULL,
					session_started=?, updated_at=? WHERE slug=?`,
				now, now, now, task.Slug,
			); err != nil {
				fmt.Fprintf(os.Stderr, "error: flip status: %v\n", err)
				return 1
			}
			sessionID = ""
		} else {
			id, err := newUUID()
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: allocate session id: %v\n", err)
				return 1
			}
			sessionID = id
			if _, err := db.Exec(
				`UPDATE tasks SET status='in-progress', status_changed_at=?,
					session_provider='claude', harness='claude', session_id=?,
					session_started=?, updated_at=? WHERE slug=?`,
				now, sessionID, now, now, task.Slug,
			); err != nil {
				fmt.Fprintf(os.Stderr, "error: flip status: %v\n", err)
				return 1
			}
		}
	} else {
		if _, err := db.Exec(
			`UPDATE tasks SET status='in-progress', session_last_resumed=?, updated_at=? WHERE slug=?`,
			now, now, task.Slug,
		); err != nil {
			fmt.Fprintf(os.Stderr, "error: flip status: %v\n", err)
			return 1
		}
	}

	// Worktree: swap the agent cwd to a per-task worktree when work_dir is a
	// repo. On failure, roll back a freshly-created session claim so the next
	// open retries cleanly (mirrors the server's rollbackPreparedTerminalLaunch).
	originalWorkDir := task.WorkDir
	workDir := originalWorkDir
	wt, wtErr := worktree.Ensure(originalWorkDir, provider, task.Slug)
	if wtErr != nil {
		if created {
			rollbackPreparedSession(db, task.Slug, provider, sessionID)
		}
		fmt.Fprintf(os.Stderr, "error: worktree setup failed for %s: %v\n", task.Slug, wtErr)
		return 1
	}
	if wt.IsRepo {
		workDir = wt.WorktreePath
		if _, err := db.Exec(
			`UPDATE tasks SET worktree_path=?, updated_at=? WHERE slug=?`,
			wt.WorktreePath, flowdb.NowISO(), task.Slug,
		); err != nil {
			if created {
				rollbackPreparedSession(db, task.Slug, provider, sessionID)
			}
			fmt.Fprintf(os.Stderr, "error: persist worktree_path: %v\n", err)
			return 1
		}
	}

	if err := workdirreg.Touch(db, originalWorkDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: bump workdir last_used_at: %v\n", err)
	}

	prep := LaunchPrep{
		Slug:           task.Slug,
		SessionID:      sessionID,
		Provider:       provider,
		PermissionMode: task.PermissionMode,
		WorkDir:        workDir,
		Created:        created,
		NeedsCapture:   provider == agents.ProviderCodex && created,
	}
	if jsonOut {
		if err := json.NewEncoder(os.Stdout).Encode(prep); err != nil {
			fmt.Fprintf(os.Stderr, "error: encode launch descriptor: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Printf("prepared %s session for %s in %s (created=%v)\n", provider, task.Slug, workDir, created)
	return 0
}

// rollbackPreparedSession undoes a freshly-created session claim when worktree
// setup fails after the status flip, so the task returns to backlog and the
// next launch attempt re-bootstraps cleanly. The WHERE clauses guard against a
// concurrent launch having mutated the row in between.
func rollbackPreparedSession(db *sql.DB, slug, provider, sessionID string) {
	now := flowdb.NowISO()
	if provider == agents.ProviderCodex {
		if _, err := db.Exec(
			`UPDATE tasks SET session_id=NULL, session_started=NULL, status='backlog',
				status_changed_at=NULL, updated_at=? WHERE slug=? AND session_provider='codex' AND session_id IS NULL`,
			now, slug,
		); err != nil {
			fmt.Fprintf(os.Stderr, "warning: rollback prepared codex session: %v\n", err)
		}
		return
	}
	if sessionID == "" {
		return
	}
	if _, err := db.Exec(
		`UPDATE tasks SET session_id=NULL, session_started=NULL, status='backlog',
			status_changed_at=NULL, updated_at=? WHERE slug=? AND session_id=?`,
		now, slug, sessionID,
	); err != nil {
		fmt.Fprintf(os.Stderr, "warning: rollback prepared session: %v\n", err)
	}
}
