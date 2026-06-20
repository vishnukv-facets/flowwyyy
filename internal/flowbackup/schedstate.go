package flowbackup

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// SchedState is the persisted state of the scheduled backup worker. It lives
// under the (gitignored) backups/ dir so a server restart can compute catch-up
// and both the server status API and `flow backup status` can read it.
type SchedState struct {
	Schedule   string `json:"schedule,omitempty"`     // operator phrase, e.g. "daily"
	LastRunAt  string `json:"last_run_at,omitempty"`  // RFC3339
	NextRunAt  string `json:"next_run_at,omitempty"`  // RFC3339
	LastPushAt string `json:"last_push_at,omitempty"` // RFC3339, when offsite sync last succeeded
}

// schedStatePath is backups/.backup-sched.json under the flow root.
func schedStatePath(root string) string {
	return filepath.Join(root, "backups", ".backup-sched.json")
}

// LoadSchedState reads the scheduler state, returning a zero value when absent.
func LoadSchedState(root string) SchedState {
	var st SchedState
	b, err := os.ReadFile(schedStatePath(root))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	return st
}

// SaveSchedState writes the scheduler state atomically-ish (best-effort).
func SaveSchedState(root string, st SchedState) error {
	if err := os.MkdirAll(filepath.Dir(schedStatePath(root)), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(schedStatePath(root), append(b, '\n'), 0o644)
}
