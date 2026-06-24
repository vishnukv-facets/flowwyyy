package server

// flow_writes.go holds the server-side wrappers for mutations to Bucket-O
// (official-flow-owned) core tables — task_tags and tasks.waiting_on. Per the
// Phase-3 ownership model (seam §11) flowwyyy never writes those tables
// directly; it routes through the `flow` CLI via runFlowCommand. The reads that
// gate these writes still go through productdb. These wrappers preserve the
// exact semantics of the flowdb helpers they replace
// (AddTaskTag / SetTaskWaitingOnIfClear / ClearTaskWaitingOnIfNote).

import (
	"database/sql"
	"errors"
	"strings"

	"flow/internal/productdb"
)

// taskTagWriter performs the actual `flow update task --tag` exec. It is a
// package var (not inlined) so tests can stub the Bucket-O write without a real
// flow binary — mirroring monitor.tagFlowTask. Production always uses the
// runFlowCommand path.
var taskTagWriter = func(s *Server, slug, tag string) error {
	_, err := s.runFlowCommand("update", "task", slug, "--tag", tag)
	return err
}

// tagTask attaches a tag to a task via `flow update task <slug> --tag <tag>`
// (idempotent in the CLI). Replaces flowdb.AddTaskTag.
func (s *Server) tagTask(slug, tag string) error {
	return taskTagWriter(s, slug, tag)
}

// setTaskWaitingIfClear sets a task's waiting_on note only when it is currently
// empty, so an automated marker never clobbers an operator's own note. It reads
// the current note via productdb, then routes the write through `flow update
// task --waiting`. Returns true only when it actually set the note. Twin of
// flowdb.SetTaskWaitingOnIfClear (read-then-exec; the read gates the write).
func (s *Server) setTaskWaitingIfClear(slug, note string) (bool, error) {
	t, err := productdb.GetTask(s.cfg.DB, slug)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if t.WaitingOn.Valid && strings.TrimSpace(t.WaitingOn.String) != "" {
		return false, nil // already has a note — never clobber
	}
	if _, err := s.runFlowCommand("update", "task", slug, "--waiting", note); err != nil {
		return false, err
	}
	return true, nil
}

// clearTaskWaitingIfNote clears waiting_on only when it exactly matches note —
// the auto-resolve twin of setTaskWaitingIfClear, so clearing an automated
// marker never wipes an operator's own note. Twin of
// flowdb.ClearTaskWaitingOnIfNote.
func (s *Server) clearTaskWaitingIfNote(slug, note string) (bool, error) {
	t, err := productdb.GetTask(s.cfg.DB, slug)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !t.WaitingOn.Valid || t.WaitingOn.String != note {
		return false, nil // not our marker — leave it
	}
	if _, err := s.runFlowCommand("update", "task", slug, "--clear-waiting"); err != nil {
		return false, err
	}
	return true, nil
}
