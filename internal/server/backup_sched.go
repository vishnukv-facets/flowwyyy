package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"flow/internal/flowbackup"
	"flow/internal/schedule"
)

// backupScheduler is the in-process heartbeat that runs a full backup on a
// configurable cadence (default daily) while `flow ui serve` is up — a
// first-class scheduler alongside the playbook scheduler and the KB dreamer.
// Each run checkpoints the curated markdown, writes a rotated db snapshot, and
// (when an offsite remote is configured) pushes.
//
// Unlike the dreamer's process-start-anchored ticker, the next-fire time is
// derived from a PERSISTED last-run, so frequent `flow ui serve` restarts can't
// starve backups: a run that came due while the server was down fires once on
// the next boot (catch-up).
type backupScheduler struct {
	srv *Server

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	stateMu sync.Mutex
	running bool
	lastRun time.Time
	nextRun time.Time
	history []BackupRunRecord
}

const (
	backupCheckInterval = 60 * time.Second
	backupHistoryCap    = 12
	defaultBackupPhrase = "daily"
)

// BackupRunRecord is one completed scheduled backup, for the UI.
type BackupRunRecord struct {
	At         string `json:"at"`
	Status     string `json:"status"` // "ok" | "error"
	Committed  bool   `json:"committed"`
	DBSnapshot string `json:"db_snapshot,omitempty"`
	Pushed     bool   `json:"pushed"`
	Detail     string `json:"detail,omitempty"`
}

func newBackupScheduler(srv *Server) *backupScheduler {
	if srv == nil || strings.TrimSpace(srv.cfg.FlowRoot) == "" {
		return nil
	}
	return &backupScheduler{srv: srv}
}

// backupSchedulePhrase returns the operator's schedule phrase (env/Settings),
// defaulting to daily.
func backupSchedulePhrase() string {
	if v := strings.TrimSpace(os.Getenv("FLOW_BACKUP_SCHEDULE")); v != "" {
		return v
	}
	return defaultBackupPhrase
}

// cron resolves the configured phrase to a canonical cron, falling back to daily
// when the phrase is unparseable (logged once per tick by the caller).
func backupCron() (string, error) {
	spec, err := schedule.Parse(backupSchedulePhrase())
	if err != nil {
		return "", err
	}
	return spec.Cron, nil
}

func (b *backupScheduler) start() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		return
	}
	// Seed last/next from persisted state so a missed run catches up on boot.
	st := flowbackup.LoadSchedState(b.srv.cfg.FlowRoot)
	b.stateMu.Lock()
	if t, err := time.Parse(time.RFC3339, st.LastRunAt); err == nil {
		b.lastRun = t
	}
	b.nextRun = b.computeNext()
	b.stateMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	b.cancel = cancel
	b.done = done
	go b.loop(ctx, done)
}

func (b *backupScheduler) stop() {
	b.mu.Lock()
	cancel := b.cancel
	done := b.done
	b.cancel = nil
	b.done = nil
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// computeNext returns the next fire time after the last run (or after now for a
// brand-new install with no prior run — so a fresh install doesn't immediately
// re-run the backup that `flow init` already took).
func (b *backupScheduler) computeNext() time.Time {
	cron, err := backupCron()
	if err != nil {
		// Unparseable phrase → fall back to daily so backups still happen.
		cron, _ = backupCron2(defaultBackupPhrase)
	}
	anchor := time.Now()
	if !b.lastRun.IsZero() {
		anchor = b.lastRun
	}
	if next, err := schedule.Next(cron, anchor); err == nil {
		return next
	}
	return time.Now().Add(24 * time.Hour)
}

// backupCron2 resolves a specific phrase (used for the daily fallback).
func backupCron2(phrase string) (string, error) {
	spec, err := schedule.Parse(phrase)
	if err != nil {
		return "", err
	}
	return spec.Cron, nil
}

func (b *backupScheduler) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	tick := time.NewTicker(backupCheckInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !flowbackup.Enabled() {
				continue
			}
			b.stateMu.Lock()
			due := !b.nextRun.IsZero() && !time.Now().Before(b.nextRun)
			b.stateMu.Unlock()
			if due {
				b.run(ctx)
			}
		}
	}
}

// run performs one scheduled backup: checkpoint → db snapshot → gc → push.
func (b *backupScheduler) run(ctx context.Context) {
	b.stateMu.Lock()
	if b.running {
		b.stateMu.Unlock()
		return
	}
	b.running = true
	b.stateMu.Unlock()

	root := b.srv.cfg.FlowRoot
	rec := BackupRunRecord{At: time.Now().UTC().Format(time.RFC3339), Status: "ok"}

	committed, err := flowbackup.Checkpoint(root, "scheduled backup")
	if err != nil {
		rec.Status = "error"
		rec.Detail = truncate(err.Error(), 300)
	}
	rec.Committed = committed

	if snap, err := flowbackup.SnapshotDB(root); err != nil {
		rec.Status = "error"
		rec.Detail = truncate(err.Error(), 300)
	} else if snap != "" {
		rec.DBSnapshot = snap
	}
	_ = flowbackup.GC(root)

	now := time.Now()
	pushed := false
	if pushErr := b.srv.maybeBackupPush(ctx); pushErr == nil {
		if flowbackup.RemoteConfigured(root) {
			pushed = true
		}
	} else {
		fmt.Fprintf(os.Stderr, "flow backup: scheduled push: %v\n", pushErr)
	}
	rec.Pushed = pushed

	b.stateMu.Lock()
	b.running = false
	b.lastRun = now
	b.nextRun = b.computeNext()
	b.history = append([]BackupRunRecord{rec}, b.history...)
	if len(b.history) > backupHistoryCap {
		b.history = b.history[:backupHistoryCap]
	}
	last, next := b.lastRun, b.nextRun
	b.stateMu.Unlock()

	// Persist last/next for restart catch-up + CLI/UI. Load-modify-save to
	// preserve LastPushAt, which maybeBackupPush already wrote during this run.
	st := flowbackup.LoadSchedState(root)
	st.Schedule = backupSchedulePhrase()
	st.LastRunAt = last.UTC().Format(time.RFC3339)
	st.NextRunAt = next.UTC().Format(time.RFC3339)
	if err := flowbackup.SaveSchedState(root, st); err != nil {
		fmt.Fprintf(os.Stderr, "flow backup: persist sched state: %v\n", err)
	}
}

// BackupStatus is the observable state of the backup subsystem for the UI.
type BackupStatus struct {
	Enabled          bool              `json:"enabled"`
	Running          bool              `json:"running"`
	Schedule         string            `json:"schedule"`
	LastRunAt        string            `json:"last_run_at,omitempty"`
	NextRunAt        string            `json:"next_run_at,omitempty"`
	LastPushAt       string            `json:"last_push_at,omitempty"`
	Commits          int               `json:"commits"`
	DBSnapshots      int               `json:"db_snapshots"`
	RemoteConfigured bool              `json:"remote_configured"`
	RemoteURL        string            `json:"remote_url,omitempty"`
	// OffsiteMode is the configured policy: "auto" (provision a PRIVATE repo in
	// the operator's personal GitHub account when a token is available) or
	// "local" (this machine only). Cheap env read — safe on the status poll.
	OffsiteMode string `json:"offsite_mode"`
	// TokenSet reports whether an explicit personal backup token is configured
	// (env/keyring). Cheap env read — no network, safe on the status poll.
	TokenSet bool              `json:"token_set"`
	History  []BackupRunRecord `json:"history"`
}

// status returns the observable backup state.
func (b *backupScheduler) status() BackupStatus {
	root := b.srv.cfg.FlowRoot
	b.stateMu.Lock()
	st := BackupStatus{
		Enabled:          flowbackup.Enabled(),
		Running:          b.running,
		Schedule:         backupSchedulePhrase(),
		Commits:          flowbackup.CommitCount(root),
		DBSnapshots:      flowbackup.DBSnapshotCount(root),
		RemoteConfigured: flowbackup.RemoteConfigured(root),
		RemoteURL:        flowbackup.RemoteURL(root),
		OffsiteMode:      backupOffsiteMode(),
		TokenSet:         flowbackup.TokenConfigured(),
		History:          append([]BackupRunRecord(nil), b.history...),
	}
	if !b.lastRun.IsZero() {
		st.LastRunAt = b.lastRun.UTC().Format(time.RFC3339)
	}
	if !b.nextRun.IsZero() {
		st.NextRunAt = b.nextRun.UTC().Format(time.RFC3339)
	}
	b.stateMu.Unlock()
	// LastPushAt is owned by the push path (maybeBackupPush), persisted in
	// SchedState — read it from there so boot + scheduled pushes both surface.
	st.LastPushAt = flowbackup.LoadSchedState(root).LastPushAt
	return st
}
