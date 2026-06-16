package server

import (
	"database/sql"
	"errors"
	"flow/internal/workdirreg"
	"flow/internal/worktree"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) destroyDeletedEntity(kind, slug string) (actionResponse, int) {
	table, err := entityTable(kind)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	tx, err := s.cfg.DB.Begin()
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	defer tx.Rollback()

	var deletedAt sql.NullString
	if err := tx.QueryRow(fmt.Sprintf(`SELECT deleted_at FROM %s WHERE slug = ?`, table), slug).Scan(&deletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: kind + " not found: " + slug}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if !deletedAt.Valid || strings.TrimSpace(deletedAt.String) == "" {
		return actionResponse{OK: false, Message: kind + " must be in trash before it can be permanently deleted"}, http.StatusConflict
	}
	if msg, err := permanentDeleteBlocker(tx, kind, slug); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	} else if msg != "" {
		return actionResponse{OK: false, Message: msg}, http.StatusConflict
	}
	var taskWorkDir, taskAgent, taskWorktreePath string
	if kind == "task" {
		var provider, wt sql.NullString
		if err := tx.QueryRow(
			`SELECT work_dir, session_provider, worktree_path FROM tasks WHERE slug = ?`,
			slug,
		).Scan(&taskWorkDir, &provider, &wt); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		taskAgent = taskWorktreeAgent(provider.String, wt.String)
		taskWorktreePath = strings.TrimSpace(wt.String)
	}
	if _, err := tx.Exec(`DELETE FROM search_docs WHERE entity_type = ? AND entity_slug = ?`, kind, slug); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if _, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE slug = ?`, table), slug); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if err := tx.Commit(); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	// The row is gone; reclaim the on-disk directory the entity owned
	// (tasks/<slug>, projects/<slug>, playbooks/<slug>) — its brief, updates,
	// inbox.jsonl, and workspace. Best-effort: the authoritative delete is the
	// row, so a leftover dir shouldn't fail the action, but destroy must fully
	// clean up rather than orphaning files in ~/.flow.
	if dir := s.entityDir(kind, slug); dir != "" {
		_ = os.RemoveAll(dir)
	}
	msg := "deleted " + kind + " " + slug
	if kind == "task" {
		var err error
		if taskWorktreePath != "" {
			err = worktree.RemovePath(taskWorktreePath, taskAgent, slug)
		} else {
			err = worktree.Remove(taskWorkDir, taskAgent, slug)
		}
		if err != nil {
			msg += "; worktree cleanup failed: " + err.Error()
		}
	}
	return actionResponse{OK: true, Message: msg}, http.StatusOK
}

func taskWorktreeAgent(provider, worktreePath string) string {
	clean := filepath.Clean(worktreePath)
	sep := string(os.PathSeparator)
	if strings.Contains(clean, sep+".codex"+sep+"worktrees"+sep) {
		return worktree.AgentCodex
	}
	if strings.Contains(clean, sep+".claude"+sep+"worktrees"+sep) {
		return worktree.AgentClaude
	}
	switch strings.TrimSpace(provider) {
	case worktree.AgentCodex:
		return worktree.AgentCodex
	case worktree.AgentClaude:
		return worktree.AgentClaude
	}
	return worktree.AgentClaude
}

// emptyTrash permanently deletes every soft-deleted task, project, and
// playbook. It reuses destroyDeletedEntity per item (so each gets the same
// dependency-blocker check + filesystem cleanup), looping so a trashed parent
// is removed after its trashed children unblock it. Items still referenced by
// ACTIVE entities (e.g. a live child) can't be destroyed and are reported as
// kept rather than failing the whole sweep.
func (s *Server) emptyTrash() (actionResponse, int) {
	trash := s.uiTrash()
	pending := make([]uiTrashItem, 0, trash.Total)
	pending = append(pending, trash.Tasks...)
	pending = append(pending, trash.Projects...)
	pending = append(pending, trash.Playbooks...)
	if len(pending) == 0 {
		return actionResponse{OK: true, Message: "Trash is already empty"}, http.StatusOK
	}

	deleted := 0
	for {
		progressed := false
		remaining := pending[:0:0]
		for _, it := range pending {
			if resp, _ := s.destroyDeletedEntity(it.Kind, it.Slug); resp.OK {
				deleted++
				progressed = true
			} else {
				remaining = append(remaining, it)
			}
		}
		pending = remaining
		if !progressed || len(pending) == 0 {
			break
		}
	}

	noun := "items"
	if deleted == 1 {
		noun = "item"
	}
	msg := fmt.Sprintf("Emptied trash — permanently deleted %d %s", deleted, noun)
	if len(pending) > 0 {
		msg += fmt.Sprintf("; kept %d still referenced by active items", len(pending))
	}
	return actionResponse{OK: true, Message: msg}, http.StatusOK
}

// entityDir resolves the ~/.flow directory that backs a trashable entity, with
// the result confined to <FlowRoot>/<tasks|projects|playbooks>/ so a crafted
// slug can never escape the data tree.
func (s *Server) entityDir(kind, slug string) string {
	if validateSlug(slug) != nil {
		return ""
	}
	var sub string
	switch strings.TrimSpace(kind) {
	case "task":
		sub = "tasks"
	case "project":
		sub = "projects"
	case "playbook":
		sub = "playbooks"
	default:
		return ""
	}
	root := strings.TrimSpace(s.cfg.FlowRoot)
	if root == "" {
		return ""
	}
	base := filepath.Clean(filepath.Join(root, sub)) + string(os.PathSeparator)
	dir := filepath.Clean(filepath.Join(root, sub, slug))
	if !strings.HasPrefix(dir, base) {
		return ""
	}
	return dir
}

func entityTable(kind string) (string, error) {
	switch strings.TrimSpace(kind) {
	case "task":
		return "tasks", nil
	case "project":
		return "projects", nil
	case "playbook":
		return "playbooks", nil
	default:
		return "", fmt.Errorf("invalid entity kind %q", kind)
	}
}

func permanentDeleteBlocker(tx *sql.Tx, kind, slug string) (string, error) {
	var count int
	switch kind {
	case "task":
		if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE parent_slug = ?`, slug).Scan(&count); err != nil {
			return "", err
		}
		if count > 0 {
			return fmt.Sprintf("task %s still has %d child task(s); delete or detach them first", slug, count), nil
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE forked_from_slug = ?`, slug).Scan(&count); err != nil {
			return "", err
		}
		if count > 0 {
			return fmt.Sprintf("task %s still has %d forked task(s); delete those forks first", slug, count), nil
		}
	case "project":
		if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE project_slug = ?`, slug).Scan(&count); err != nil {
			return "", err
		}
		if count > 0 {
			return fmt.Sprintf("project %s still has %d task(s); delete or move them first", slug, count), nil
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM playbooks WHERE project_slug = ?`, slug).Scan(&count); err != nil {
			return "", err
		}
		if count > 0 {
			return fmt.Sprintf("project %s still has %d playbook(s); delete or move them first", slug, count), nil
		}
	case "playbook":
		if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE playbook_slug = ?`, slug).Scan(&count); err != nil {
			return "", err
		}
		if count > 0 {
			return fmt.Sprintf("playbook %s still has %d run task(s); delete them first", slug, count), nil
		}
	}
	return "", nil
}

func (s *Server) workdirAction(req actionRequest) (actionResponse, int) {
	rawPath := strings.TrimSpace(firstNonEmpty(req.Path, req.WorkDir, req.Target))
	if rawPath == "" {
		return actionResponse{OK: false, Message: "workdir path is required"}, http.StatusBadRequest
	}
	abs, err := filepath.Abs(rawPath)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	switch req.Kind {
	case "workdir-add", "workdir-rename":
		info, err := os.Stat(abs)
		if err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
		}
		if !info.IsDir() {
			return actionResponse{OK: false, Message: abs + " is not a directory"}, http.StatusBadRequest
		}
		name := strings.TrimSpace(req.Name)
		description := strings.TrimSpace(req.Description)
		if req.Kind == "workdir-add" && name == "" {
			name = filepath.Base(abs)
		}
		if req.Kind == "workdir-rename" && name == "" {
			return actionResponse{OK: false, Message: "workdir name is required"}, http.StatusBadRequest
		}
		if err := workdirreg.Register(s.cfg.DB, abs, name, description); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		verb := "registered"
		if req.Kind == "workdir-rename" {
			verb = "renamed"
		}
		return actionResponse{OK: true, Message: verb + " workdir " + abs}, http.StatusOK
	case "workdir-remove":
		if _, err := s.cfg.DB.Exec(`DELETE FROM workdirs WHERE path = ?`, abs); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "removed workdir " + abs}, http.StatusOK
	default:
		return actionResponse{OK: false, Message: "unknown workdir action " + req.Kind}, http.StatusBadRequest
	}
}
