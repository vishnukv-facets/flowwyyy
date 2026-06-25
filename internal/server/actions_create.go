package server

import (
	"database/sql"
	"errors"
	"flow/internal/flowdb"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) createFlow(req actionRequest) (actionResponse, int) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return actionResponse{OK: false, Message: "name is required"}, http.StatusBadRequest
	}
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = strings.TrimSpace(req.Target)
	}
	if err := validateSlug(slug); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	priority := strings.TrimSpace(req.Priority)
	if priority == "" {
		priority = "medium"
	}
	permissionMode, err := flowdb.NormalizePermissionMode(req.PermissionMode)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	provider, err := s.availableProvider(req.Provider)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	project := strings.TrimSpace(req.Project)
	if project == "__adhoc" {
		project = ""
	}
	if project != "" {
		if err := validateSlug(project); err != nil {
			return actionResponse{OK: false, Message: "project: " + err.Error()}, http.StatusBadRequest
		}
	}
	workDir := strings.TrimSpace(req.WorkDir)

	existing, err := flowdb.GetTask(s.cfg.DB, slug)
	if err == nil {
		return s.createFlowFromExisting(req, existing, provider, permissionMode, priority, project, workDir)
	}
	// err is guaranteed non-nil here (the err == nil case returned above).
	if !errors.Is(err, sql.ErrNoRows) {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}

	// `flow add task` now requires an explicit agent (no silent claude default),
	// so always pass the resolved provider — claude included.
	args := []string{"add", "task", name, "--slug", slug, "--priority", priority, "--agent", provider}
	args = append(args, "--permission-mode", permissionMode)
	if model := flowdb.NormalizeModel(req.Model); model != "" {
		args = append(args, "--model", model)
	}
	if effort, err := flowdb.NormalizeEffort(provider, req.Effort); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	} else if effort != "" {
		args = append(args, "--effort", effort)
	}
	if project != "" {
		args = append(args, "--project", project)
	}
	if workDir != "" {
		args = append(args, "--work-dir", workDir)
	}
	out, err := s.runFlowCommand(args...)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
	}
	prompt := strings.TrimSpace(req.Prompt)
	if len(req.AttachmentFiles) > 0 {
		files, err := s.saveTaskAttachmentFiles(slug, req.AttachmentFiles)
		if err != nil {
			return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
		}
		prompt = promptWithAttachedImages(prompt, files)
	}
	if strings.TrimSpace(prompt) != "" {
		if err := s.writeTaskBrief(slug, name, prompt); err != nil {
			return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
		}
	}
	if req.NoOpen {
		// Create-only: leave the task in backlog, don't bridge a session.
		return actionResponse{OK: true, Message: "created " + slug, Output: out}, http.StatusOK
	}
	agent, _ := s.agentForTask(slug)
	return actionResponse{OK: true, Message: "created " + slug + "; opening browser terminal", Output: out, Agent: agent, Bridge: true}, http.StatusOK
}

func (s *Server) createFlowFromExisting(req actionRequest, task *flowdb.Task, provider, permissionMode, priority, project, workDir string) (actionResponse, int) {
	if task == nil {
		return actionResponse{OK: false, Message: "task not found"}, http.StatusInternalServerError
	}
	if !task.ArchivedAt.Valid && !task.DeletedAt.Valid && task.Status != "done" {
		if err := flowdb.EnsureTaskStartable(s.cfg.DB, task); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, taskStartErrorStatus(err)
		}
		if req.NoOpen {
			return actionResponse{OK: true, Message: "task " + task.Slug + " already exists"}, http.StatusOK
		}
		agent, _ := s.agentForTask(task.Slug)
		if agent != nil {
			if err := s.ensureProviderAvailable(firstNonEmpty(agent.Provider, "claude")); err != nil {
				return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
			}
		}
		return actionResponse{
			OK:      true,
			Message: "task " + task.Slug + " already exists; opening browser terminal",
			Agent:   agent,
			Bridge:  true,
		}, http.StatusOK
	}
	if workDir == "" {
		workDir = strings.TrimSpace(task.WorkDir)
	}
	if workDir == "" {
		return actionResponse{OK: false, Message: "work_dir is required to reactivate " + task.Slug}, http.StatusBadRequest
	}
	now := flowdb.NowISO()
	var projectValue any
	if project != "" {
		projectValue = project
	}
	if _, err := s.cfg.DB.Exec(
		`UPDATE tasks SET
			name = ?,
			project_slug = ?,
			status = 'backlog',
			kind = 'regular',
			playbook_slug = NULL,
			priority = ?,
			work_dir = ?,
			waiting_on = NULL,
			permission_mode = ?,
				status_changed_at = ?,
				session_provider = ?,
				harness = ?,
				session_id = NULL,
			session_started = NULL,
			session_last_resumed = NULL,
			updated_at = ?,
			archived_at = NULL,
			deleted_at = NULL
		 WHERE slug = ?`,
		strings.TrimSpace(req.Name),
		projectValue,
		priority,
		workDir,
		permissionMode,
		now,
		provider,
		provider,
		now,
		task.Slug,
	); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	prompt := strings.TrimSpace(req.Prompt)
	if len(req.AttachmentFiles) > 0 {
		files, err := s.saveTaskAttachmentFiles(task.Slug, req.AttachmentFiles)
		if err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		prompt = promptWithAttachedImages(prompt, files)
	}
	if strings.TrimSpace(prompt) != "" {
		if err := s.writeTaskBrief(task.Slug, strings.TrimSpace(req.Name), prompt); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
	}
	agent, _ := s.agentForTask(task.Slug)
	return actionResponse{
		OK:      true,
		Message: "reactivated " + task.Slug + "; opening browser terminal",
		Agent:   agent,
		Bridge:  true,
	}, http.StatusOK
}

func promptWithAttachedImages(prompt string, files []FileRef) string {
	prompt = strings.TrimSpace(prompt)
	if len(files) == 0 {
		return prompt
	}
	var b strings.Builder
	if prompt != "" {
		b.WriteString(prompt)
		b.WriteString("\n\n")
	}
	b.WriteString("Attached images:\n")
	for _, file := range files {
		if strings.TrimSpace(file.Filename) != "" {
			b.WriteString("- ")
			b.WriteString(file.Filename)
			b.WriteString(": ")
			b.WriteString(file.Path)
			b.WriteByte('\n')
			continue
		}
		b.WriteString("- ")
		b.WriteString(file.Path)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func (s *Server) createProject(req actionRequest) (actionResponse, int) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return actionResponse{OK: false, Message: "name is required"}, http.StatusBadRequest
	}
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = strings.TrimSpace(req.Target)
	}
	if err := validateSlug(slug); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	workDir := strings.TrimSpace(req.WorkDir)
	if workDir == "" {
		return actionResponse{OK: false, Message: "work_dir is required"}, http.StatusBadRequest
	}
	priority := strings.TrimSpace(req.Priority)
	if priority == "" {
		priority = "medium"
	}

	if _, err := flowdb.GetProject(s.cfg.DB, slug); err == nil {
		return actionResponse{OK: false, Message: "project " + slug + " already exists"}, http.StatusConflict
	} else if !errors.Is(err, sql.ErrNoRows) {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}

	args := []string{"add", "project", name, "--slug", slug, "--priority", priority, "--work-dir", workDir}
	if req.Mkdir {
		args = append(args, "--mkdir")
	}
	out, err := s.runFlowCommand(args...)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
	}
	if strings.TrimSpace(req.Description) != "" {
		if err := s.writeProjectBrief(slug, name, req.Description); err != nil {
			return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
		}
	}
	return actionResponse{OK: true, Message: "created project " + slug, Output: out}, http.StatusOK
}

func (s *Server) createPlaybook(req actionRequest) (actionResponse, int) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return actionResponse{OK: false, Message: "name is required"}, http.StatusBadRequest
	}
	slug := firstNonEmpty(req.Slug, req.Target)
	if err := validateSlug(slug); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	workDir := strings.TrimSpace(req.WorkDir)
	if workDir == "" {
		return actionResponse{OK: false, Message: "work_dir is required"}, http.StatusBadRequest
	}
	if _, err := flowdb.GetPlaybook(s.cfg.DB, slug); err == nil {
		return actionResponse{OK: false, Message: "playbook " + slug + " already exists"}, http.StatusConflict
	} else if !errors.Is(err, sql.ErrNoRows) {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}

	args := []string{"add", "playbook", name, "--slug", slug, "--work-dir", workDir}
	if req.Project != "" {
		if err := validateSlug(req.Project); err != nil {
			return actionResponse{OK: false, Message: "project: " + err.Error()}, http.StatusBadRequest
		}
		args = append(args, "--project", req.Project)
	}
	if req.Mkdir {
		args = append(args, "--mkdir")
	}
	if sched := strings.TrimSpace(req.Schedule); sched != "" {
		args = append(args, "--schedule", sched)
	}
	out, err := s.runFlowCommand(args...)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
	}
	definition := firstNonEmpty(req.Description, req.Prompt)
	if definition != "" {
		if err := s.writePlaybookBrief(slug, name, definition); err != nil {
			return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
		}
	}
	return actionResponse{OK: true, Message: "created playbook " + slug, Output: out}, http.StatusOK
}

func (s *Server) createKB(req actionRequest) (actionResponse, int) {
	filename := strings.TrimSpace(firstNonEmpty(req.Slug, req.Target, req.Path))
	if filename == "" {
		return actionResponse{OK: false, Message: "filename is required"}, http.StatusBadRequest
	}
	if !strings.HasSuffix(strings.ToLower(filename), ".md") {
		filename += ".md"
	}
	if !validFilename(filename) {
		return actionResponse{OK: false, Message: "invalid KB filename"}, http.StatusBadRequest
	}
	title := strings.TrimSpace(req.Name)
	if title == "" {
		title = strings.TrimSuffix(filename, filepath.Ext(filename))
	}
	path := filepath.Join(s.cfg.FlowRoot, "kb", filename)
	cleanBase := filepath.Join(filepath.Clean(s.cfg.FlowRoot), "kb") + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(path), cleanBase) {
		return actionResponse{OK: false, Message: "invalid KB path"}, http.StatusBadRequest
	}
	if _, err := os.Stat(path); err == nil {
		return actionResponse{OK: false, Message: "KB document " + filename + " already exists"}, http.StatusConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	body := markdownWithHeading(title, firstNonEmpty(req.Description, req.Prompt))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	return actionResponse{OK: true, Message: "created KB document " + filename}, http.StatusOK
}

func (s *Server) createPlaybookRunTask(pb *flowdb.Playbook, provider, permissionMode string) (string, error) {
	root := strings.TrimSpace(s.cfg.FlowRoot)
	if root == "" {
		return "", errors.New("flow root is not configured")
	}
	pbBriefPath := filepath.Join(root, "playbooks", pb.Slug, "brief.md")
	pbBriefBytes, err := os.ReadFile(pbBriefPath)
	if err != nil {
		return "", fmt.Errorf("read playbook brief %s: %w", pbBriefPath, err)
	}
	runSlug, err := generatePlaybookRunSlug(s.cfg.DB, pb.Slug, time.Now())
	if err != nil {
		return "", err
	}
	now := flowdb.NowISO()
	_, err = s.cfg.DB.Exec(
		`INSERT INTO tasks (
				slug, name, project_slug, status, kind, playbook_slug, priority,
				work_dir, permission_mode, session_provider, harness, status_changed_at, created_at, updated_at
			) VALUES (?, ?, ?, 'backlog', 'playbook_run', ?, 'medium', ?, ?, ?, ?, ?, ?, ?)`,
		runSlug,
		fmt.Sprintf("%s run %s", pb.Slug, runSlug),
		pb.ProjectSlug,
		pb.Slug,
		pb.WorkDir,
		permissionMode,
		provider,
		provider,
		now, now, now,
	)
	if err != nil {
		return "", fmt.Errorf("insert run task: %w", err)
	}
	runDir := filepath.Join(root, "tasks", runSlug)
	if err := os.MkdirAll(filepath.Join(runDir, "updates"), 0o755); err != nil {
		_ = deleteUnstartedPlaybookRun(s.cfg.DB, runSlug)
		return "", fmt.Errorf("mkdir %s: %w", runDir, err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "brief.md"), pbBriefBytes, 0o644); err != nil {
		_ = deleteUnstartedPlaybookRun(s.cfg.DB, runSlug)
		return "", fmt.Errorf("write run brief.md: %w", err)
	}
	return runSlug, nil
}

func generatePlaybookRunSlug(db *sql.DB, playbookSlug string, t time.Time) (string, error) {
	t = t.UTC()
	minute := fmt.Sprintf("%s--%04d-%02d-%02d-%02d-%02d",
		playbookSlug, t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute())
	if ok, err := taskSlugAvailable(db, minute); err != nil {
		return "", err
	} else if ok {
		return minute, nil
	}
	second := fmt.Sprintf("%s-%02d", minute, t.Second())
	if ok, err := taskSlugAvailable(db, second); err != nil {
		return "", err
	} else if ok {
		return second, nil
	}
	for n := 2; n < 1000; n++ {
		candidate := fmt.Sprintf("%s-%d", second, n)
		if ok, err := taskSlugAvailable(db, candidate); err != nil {
			return "", err
		} else if ok {
			return candidate, nil
		}
	}
	return "", errors.New("could not generate unique run slug after 1000 attempts")
}

func taskSlugAvailable(db *sql.DB, slug string) (bool, error) {
	var got string
	err := db.QueryRow(`SELECT slug FROM tasks WHERE slug = ?`, slug).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func deleteUnstartedPlaybookRun(db *sql.DB, slug string) error {
	_, err := db.Exec(`DELETE FROM tasks WHERE slug = ? AND kind = 'playbook_run' AND session_id IS NULL`, slug)
	return err
}

func (s *Server) availableTaskSlug(base string) string {
	slug := base
	for i := 0; i < 50; i++ {
		if err := validateSlug(slug); err != nil {
			return base
		}
		_, err := flowdb.GetTask(s.cfg.DB, slug)
		if errors.Is(err, sql.ErrNoRows) {
			return slug
		}
		if err != nil {
			return slug
		}
		slug = fmt.Sprintf("%s-%d", base, i+2)
	}
	return fmt.Sprintf("%s-%d", base, time.Now().Unix())
}

func (s *Server) writeTaskBrief(slug, name, prompt string) error {
	path := filepath.Join(s.cfg.FlowRoot, "tasks", slug, "brief.md")
	if !strings.HasPrefix(filepath.Clean(path), filepath.Join(s.cfg.FlowRoot, "tasks")+string(os.PathSeparator)) {
		return errors.New("invalid task brief path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create task brief dir: %w", err)
	}
	body := fmt.Sprintf("# %s\n\n%s\n", name, strings.TrimSpace(prompt))
	return os.WriteFile(path, []byte(body), 0o644)
}

func (s *Server) writeProjectBrief(slug, name, description string) error {
	path := filepath.Join(s.cfg.FlowRoot, "projects", slug, "brief.md")
	if !strings.HasPrefix(filepath.Clean(path), filepath.Join(s.cfg.FlowRoot, "projects")+string(os.PathSeparator)) {
		return errors.New("invalid project brief path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create project brief dir: %w", err)
	}
	body := fmt.Sprintf("# %s\n\n%s\n", name, strings.TrimSpace(description))
	return os.WriteFile(path, []byte(body), 0o644)
}

func (s *Server) writePlaybookBrief(slug, name, definition string) error {
	path := filepath.Join(s.cfg.FlowRoot, "playbooks", slug, "brief.md")
	if !strings.HasPrefix(filepath.Clean(path), filepath.Join(s.cfg.FlowRoot, "playbooks")+string(os.PathSeparator)) {
		return errors.New("invalid playbook brief path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create playbook brief dir: %w", err)
	}
	return os.WriteFile(path, []byte(markdownWithHeading(name, definition)), 0o644)
}

func markdownWithHeading(title, body string) string {
	text := strings.TrimSpace(body)
	if text == "" {
		return fmt.Sprintf("# %s\n", strings.TrimSpace(title))
	}
	if strings.HasPrefix(text, "# ") {
		return text + "\n"
	}
	return fmt.Sprintf("# %s\n\n%s\n", strings.TrimSpace(title), text)
}

func (s *Server) overviewChat(req actionRequest) (actionResponse, int) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return actionResponse{OK: false, Message: "prompt is required"}, http.StatusBadRequest
	}
	launch, err := s.prepareOverviewFloatingLaunch(req)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	terminal := s.terminals.registerFloatingLaunch(launch, "Ask Flow")
	// Record a durable chat row for the Chats sidebar. The session has already
	// started, so a DB hiccup must not fail the launch — log and continue,
	// mirroring the best-effort style of persistFloatingLocked.
	if s.cfg.DB != nil {
		now := flowdb.NowISO()
		var sid sql.NullString
		if launch.SessionID != "" {
			sid = sql.NullString{String: launch.SessionID, Valid: true}
		}
		if err := flowdb.InsertChat(s.cfg.DB, flowdb.Chat{
			Slug:           launch.Slug,
			Title:          deriveChatTitle(prompt),
			Provider:       launch.Provider,
			Origin:         "ui",
			SessionID:      sid,
			CreatedAt:      now,
			LastActivityAt: now,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "flow: record chat %q: %v\n", launch.Slug, err)
		}
	}
	return actionResponse{OK: true, Message: "opened floating overview agent", FloatingTerminal: &terminal}, http.StatusOK
}

func overviewBrief(prompt string) string {
	return "You are the Flow overview command-center agent. Help the user decide what to do today, inspect Flow/GitHub/Slack monitor context when relevant, and route work into Flow tasks or sessions.\n\nLatest user request:\n" + prompt
}
