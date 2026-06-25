package server

import (
	"database/sql"
	"errors"
	"flow/internal/flowdb"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"syscall"
)

func (s *Server) updatePermissionMode(req actionRequest) (actionResponse, int) {
	target := firstNonEmpty(req.Target, req.Slug)
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	mode, err := flowdb.NormalizePermissionMode(req.PermissionMode)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	task, err := flowdb.GetTask(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if task.PermissionMode == mode {
		return actionResponse{OK: true, Message: "permission mode unchanged (" + mode + ")"}, http.StatusOK
	}

	if _, err := s.cfg.DB.Exec(
		`UPDATE tasks SET permission_mode = ?, updated_at = ? WHERE slug = ?`,
		mode, flowdb.NowISO(), task.Slug,
	); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}

	browserLive := s.terminals.running(task.Slug)
	sharedLive := s.terminals.sharedRunning(task.Slug)
	if browserLive {
		s.terminals.stop(task.Slug)
	} else if sharedLive {
		_ = sharedTerminalKillSession(sharedTerminalSessionName(task.Slug))
		s.terminals.sharedRunningCache.invalidate(task.Slug)
	}

	terminatedNative := false
	if !browserLive && !sharedLive && task.SessionID.Valid && strings.TrimSpace(task.SessionID.String) != "" && safeSessionRe.MatchString(task.SessionID.String) {
		if pid, perr := claudePIDForSession(task.SessionID.String); perr == nil {
			if kerr := syscall.Kill(pid, syscall.SIGTERM); kerr == nil {
				terminatedNative = true
			}
		}
	}

	msg := "permission mode set to " + mode
	if browserLive || sharedLive {
		msg += "; restarting browser terminal"
		agent, err := s.agentForTask(task.Slug)
		if err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: msg, Agent: agent, Bridge: true, AlreadyLive: true}, http.StatusOK
	}
	if terminatedNative {
		msg += "; current session terminated, reattach to apply"
	}
	return actionResponse{OK: true, Message: msg, AlreadyLive: terminatedNative}, http.StatusOK
}

// updateProvider switches a not-yet-started task's agent (claude ↔ codex). The
// provider is sticky on the task row; the browser/native terminal launch reads
// tasks.session_provider when it spawns (see terminal_bridge.startSessionLocked
// and buildTerminalLaunch), so persisting the choice is all that's needed — the
// spawn path is unchanged. applyBacklogProviderChoice enforces the same
// "only before a session starts" guard and provider-availability check the
// new-task form relies on, so a session that has already launched is rejected.
func (s *Server) updateProvider(req actionRequest) (actionResponse, int) {
	target := firstNonEmpty(req.Target, req.Slug)
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	provider, err := flowdb.NormalizeSessionProvider(req.Provider)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	if err := s.applyBacklogProviderChoice(target, provider); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	return actionResponse{OK: true, Message: "agent set to " + provider}, http.StatusOK
}

// updateModel pins (or clears) the per-task session model for a not-yet-started
// task. An empty model means "Auto" — no explicit pin, so flow resolves a tier
// at launch (with auto-downshift). Like the agent picker, the model is
// backlog-locked: flow do/resume reads tasks.model at bootstrap and never
// switches a live session's model mid-life, so a started session is rejected.
func (s *Server) updateModel(req actionRequest) (actionResponse, int) {
	target := firstNonEmpty(req.Target, req.Slug)
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	if err := s.applyBacklogModelChoice(target, req.Model); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	if model := flowdb.NormalizeModel(req.Model); model != "" {
		return actionResponse{OK: true, Message: "model set to " + model}, http.StatusOK
	}
	return actionResponse{OK: true, Message: "model cleared (auto — resolved at launch)"}, http.StatusOK
}

func (s *Server) updateEffort(req actionRequest) (actionResponse, int) {
	target := firstNonEmpty(req.Target, req.Slug)
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	task, err := flowdb.GetTask(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	effort, err := flowdb.NormalizeEffort(task.SessionProvider, req.Effort)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	if err := s.applyBacklogEffortChoice(task, effort); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	if effort != "" {
		return actionResponse{OK: true, Message: "effort set to " + effort}, http.StatusOK
	}
	return actionResponse{OK: true, Message: "effort cleared (provider/model default)"}, http.StatusOK
}

// applyBacklogModelChoice persists tasks.model for a backlog task, mirroring
// applyBacklogProviderChoice. An empty model clears the column (NULL → auto).
// The same "only before a session starts" guard applies; a no-op change on an
// already-started task is allowed so re-saving the unchanged value never errors.
func (s *Server) applyBacklogModelChoice(target, rawModel string) error {
	model := flowdb.NormalizeModel(rawModel)
	task, err := flowdb.GetTask(s.cfg.DB, target)
	if err != nil {
		return err
	}
	current := ""
	if task.Model.Valid {
		current = strings.TrimSpace(task.Model.String)
	}
	if task.Status != "backlog" || task.SessionID.Valid || task.SessionStarted.Valid {
		if current == model {
			return nil
		}
		return fmt.Errorf("model can only be changed before a session starts")
	}
	var modelArg any
	if model != "" {
		modelArg = model
	}
	_, err = s.cfg.DB.Exec(
		`UPDATE tasks SET model = ?, updated_at = ?
		 WHERE slug = ? AND status = 'backlog' AND session_id IS NULL AND session_started IS NULL`,
		modelArg, flowdb.NowISO(), target,
	)
	return err
}

func (s *Server) applyBacklogEffortChoice(task *flowdb.Task, effort string) error {
	if task == nil {
		return sql.ErrNoRows
	}
	current := ""
	if task.Effort.Valid {
		current = strings.TrimSpace(task.Effort.String)
	}
	if task.Status != "backlog" || task.SessionID.Valid || task.SessionStarted.Valid {
		if current == effort {
			return nil
		}
		return fmt.Errorf("effort can only be changed before a session starts")
	}
	var effortArg any
	if effort != "" {
		effortArg = effort
	}
	_, err := s.cfg.DB.Exec(
		`UPDATE tasks SET effort = ?, updated_at = ?
		 WHERE slug = ? AND status = 'backlog' AND session_id IS NULL AND session_started IS NULL`,
		effortArg, flowdb.NowISO(), task.Slug,
	)
	return err
}

func (s *Server) applyBacklogProviderChoice(target, rawProvider string) error {
	if strings.TrimSpace(rawProvider) == "" {
		return nil
	}
	provider, err := s.availableProvider(rawProvider)
	if err != nil {
		return err
	}
	task, err := flowdb.GetTask(s.cfg.DB, target)
	if err != nil {
		return err
	}
	currentProvider := strings.TrimSpace(task.SessionProvider)
	if currentProvider == "" {
		currentProvider = "claude"
	}
	if task.Status != "backlog" || task.SessionID.Valid || task.SessionStarted.Valid {
		if currentProvider == provider {
			return nil
		}
		return fmt.Errorf("provider can only be changed before a session starts")
	}
	now := flowdb.NowISO()
	_, err = s.cfg.DB.Exec(
		`UPDATE tasks SET session_provider = ?, harness = ?, updated_at = ?
			 WHERE slug = ? AND status = 'backlog' AND session_id IS NULL AND session_started IS NULL`,
		provider, provider, now, target,
	)
	return err
}

func (s *Server) updatePriority(req actionRequest) (actionResponse, int) {
	target := firstNonEmpty(req.Target, req.Slug)
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	priority, err := flowdb.NormalizePriority(req.Priority)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	task, err := flowdb.GetTask(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if task.Priority == priority {
		return actionResponse{OK: true, Message: "priority unchanged (" + priority + ")"}, http.StatusOK
	}
	if _, err := s.cfg.DB.Exec(
		`UPDATE tasks SET priority = ?, updated_at = ? WHERE slug = ?`,
		priority, flowdb.NowISO(), task.Slug,
	); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	return actionResponse{OK: true, Message: "priority set to " + priority}, http.StatusOK
}

func (s *Server) updateTaskName(req actionRequest) (actionResponse, int) {
	target := firstNonEmpty(req.Target, req.Slug)
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return actionResponse{OK: false, Message: "task name is required"}, http.StatusBadRequest
	}
	task, err := flowdb.GetTask(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if task.Name == name {
		return actionResponse{OK: true, Message: "task name unchanged"}, http.StatusOK
	}
	if _, err := s.cfg.DB.Exec(
		`UPDATE tasks SET name = ?, updated_at = ? WHERE slug = ?`,
		name, flowdb.NowISO(), task.Slug,
	); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	return actionResponse{OK: true, Message: "task name updated"}, http.StatusOK
}

// updateProject edits a project's display name and/or priority in place — the
// browser twin of `flow update project`. Slug rename is deliberately not
// exposed here (it moves the on-disk ~/.flow/projects/<slug> dir and cascades
// references — a CLI-only operation). At least one of name/priority must be
// given.
func (s *Server) updateProject(req actionRequest) (actionResponse, int) {
	target := firstNonEmpty(req.Target, req.Slug)
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	name := strings.TrimSpace(req.Name)
	priorityRaw := strings.TrimSpace(req.Priority)
	if name == "" && priorityRaw == "" {
		return actionResponse{OK: false, Message: "nothing to update (provide name and/or priority)"}, http.StatusBadRequest
	}
	var priority string
	if priorityRaw != "" {
		p, err := flowdb.NormalizePriority(priorityRaw)
		if err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		priority = p
	}
	project, err := flowdb.GetProject(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "project not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	now := flowdb.NowISO()
	var changed []string
	if name != "" && name != project.Name {
		if _, err := s.cfg.DB.Exec(`UPDATE projects SET name = ?, updated_at = ? WHERE slug = ?`, name, now, project.Slug); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		changed = append(changed, "name")
	}
	if priority != "" && priority != project.Priority {
		if _, err := s.cfg.DB.Exec(`UPDATE projects SET priority = ?, updated_at = ? WHERE slug = ?`, priority, now, project.Slug); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		changed = append(changed, "priority")
	}
	if len(changed) == 0 {
		return actionResponse{OK: true, Message: "project unchanged"}, http.StatusOK
	}
	return actionResponse{OK: true, Message: "project " + strings.Join(changed, " + ") + " updated"}, http.StatusOK
}

// updatePlaybook edits a playbook's display name in place. Slug rename (which
// moves the playbook dir and cascades to playbook-run tasks) stays CLI-only.
func (s *Server) updatePlaybook(req actionRequest) (actionResponse, int) {
	target := firstNonEmpty(req.Target, req.Slug)
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return actionResponse{OK: false, Message: "playbook name is required"}, http.StatusBadRequest
	}
	pb, err := flowdb.GetPlaybook(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "playbook not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if pb.Name == name {
		return actionResponse{OK: true, Message: "playbook name unchanged"}, http.StatusOK
	}
	if _, err := s.cfg.DB.Exec(`UPDATE playbooks SET name = ?, updated_at = ? WHERE slug = ?`, name, flowdb.NowISO(), pb.Slug); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	return actionResponse{OK: true, Message: "playbook name updated"}, http.StatusOK
}

// updatePlaybookSchedule sets, clears, pauses, or resumes a playbook's
// recurring schedule. Like createPlaybook it shells out to the flow binary
// (`flow update playbook ... --schedule/--clear-schedule/...`) so schedule
// parsing and next-fire computation live in exactly one place — the CLI.
func (s *Server) updatePlaybookSchedule(req actionRequest) (actionResponse, int) {
	target := firstNonEmpty(req.Target, req.Slug)
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	op := strings.TrimSpace(req.ScheduleOp)
	if op == "" && strings.TrimSpace(req.Schedule) != "" {
		op = "set"
	}
	args := []string{"update", "playbook", target}
	switch op {
	case "set":
		if strings.TrimSpace(req.Schedule) == "" {
			return actionResponse{OK: false, Message: "schedule is required"}, http.StatusBadRequest
		}
		args = append(args, "--schedule", req.Schedule)
	case "clear":
		args = append(args, "--clear-schedule")
	case "pause":
		args = append(args, "--pause-schedule")
	case "resume":
		args = append(args, "--resume-schedule")
	default:
		return actionResponse{OK: false, Message: "unknown schedule op: " + op}, http.StatusBadRequest
	}
	out, err := s.runFlowCommand(args...)
	if err != nil {
		return actionResponse{OK: false, Message: strings.TrimSpace(firstNonEmpty(out, err.Error())), Output: out}, http.StatusInternalServerError
	}
	return actionResponse{OK: true, Message: "playbook schedule updated", Output: out}, http.StatusOK
}

func (s *Server) editPlaybook(target string) (actionResponse, int) {
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	pb, err := flowdb.GetPlaybook(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "playbook not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	brief := filepath.Join(s.cfg.FlowRoot, "playbooks", pb.Slug, "brief.md")
	return actionResponse{OK: true, Message: "playbook brief: " + brief}, http.StatusOK
}
