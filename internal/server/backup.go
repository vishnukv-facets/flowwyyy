package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"flow/internal/flowbackup"
)

// backupCheckpoint commits the current state of the curated markdown under the
// flow root, tagged with reason. Best-effort: a failure is logged but never
// blocks the caller (a KB write, a UI save, a dreamer pass). No-op when the flow
// root is unknown or the subsystem is disabled.
func (s *Server) backupCheckpoint(reason string) {
	root := strings.TrimSpace(s.cfg.FlowRoot)
	if root == "" {
		return
	}
	if _, err := flowbackup.Checkpoint(root, reason); err != nil {
		fmt.Fprintf(os.Stderr, "flow backup: checkpoint (%s): %v\n", reason, err)
	}
}

// backupOffsiteMode returns the offsite policy: "auto" (default — use a private
// GitHub repo when the gh CLI is authenticated) or "local" (this machine only).
func backupOffsiteMode() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("FLOW_BACKUP_OFFSITE")))
	switch v {
	case "local", "off":
		return "local"
	default:
		return "auto"
	}
}

// maybeBackupPush syncs the markdown branch + latest db snapshot offsite.
//
// Policy: with FLOW_BACKUP_OFFSITE=local it's a no-op (backups stay local). In
// the default "auto" mode, if no remote is configured yet but GitHub is
// available (the gh CLI is authenticated), flow provisions a PRIVATE flow-backup
// repo under the personal account and uses it; with no GitHub it stays local.
// Best-effort; the ctx is reserved for future cancellation.
func (s *Server) maybeBackupPush(_ context.Context) error {
	root := strings.TrimSpace(s.cfg.FlowRoot)
	if root == "" || !flowbackup.Enabled() {
		return nil
	}
	if backupOffsiteMode() == "local" {
		return nil // backups stay on this machine
	}
	if !flowbackup.RemoteConfigured(root) {
		if !flowbackup.GitHubBackupAvailable() {
			return nil // no remote and no GitHub → local only
		}
		url, created, err := flowbackup.EnsureGitHubRemote(root)
		if err != nil {
			return err
		}
		if created {
			fmt.Fprintf(os.Stderr, "flow backup: provisioned private GitHub backup repo %s\n", url)
		}
	}
	if !flowbackup.RemoteConfigured(root) {
		return nil
	}
	if err := flowbackup.Push(root); err != nil {
		return err
	}
	// Record the push time (single source of truth for "last push", read by the
	// status API + CLI) so boot and scheduled pushes both reflect in the UI.
	st := flowbackup.LoadSchedState(root)
	st.LastPushAt = time.Now().UTC().Format(time.RFC3339)
	_ = flowbackup.SaveSchedState(root, st)
	return flowbackup.PushDBSnapshot(root, flowbackup.LatestDBSnapshot(root))
}
