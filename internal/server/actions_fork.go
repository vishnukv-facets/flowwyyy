package server

import (
	"database/sql"
	"errors"
	"flow/internal/productdb"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) forkTask(req actionRequest) (actionResponse, int) {
	target := firstNonEmpty(req.Target, req.Slug)
	if err := validateSlug(target); err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	source, err := productdb.GetTask(s.cfg.DB, target)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return actionResponse{OK: false, Message: "task not found: " + target}, http.StatusNotFound
		}
		return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
	}
	slug := s.availableTaskSlug(target + "-fork")
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = source.Name
	}
	priority := strings.TrimSpace(req.Priority)
	if priority == "" {
		priority = firstNonEmpty(source.Priority, "medium")
	}
	forkProvider := s.defaultForkProvider(source, req.Provider)
	provider, err := s.availableProvider(forkProvider)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error()}, http.StatusBadRequest
	}
	project := strings.TrimSpace(req.Project)
	if project == "" && source.ProjectSlug.Valid {
		project = source.ProjectSlug.String
	}
	workDir := strings.TrimSpace(req.WorkDir)
	if workDir == "" {
		workDir = source.WorkDir
	}
	args := []string{"add", "task", name + " fork", "--slug", slug, "--priority", priority, "--agent", provider}
	if project != "" && project != "__adhoc" {
		if err := validateSlug(project); err != nil {
			return actionResponse{OK: false, Message: "project: " + err.Error()}, http.StatusBadRequest
		}
		args = append(args, "--project", project)
	}
	if workDir != "" {
		args = append(args, "--work-dir", workDir)
	}
	out, err := s.runFlowCommand(args...)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
	}
	reason := strings.TrimSpace(req.Description)
	if reason == "" {
		reason = fmt.Sprintf("Provider handoff from %s to %s.", firstNonEmpty(source.SessionProvider, "claude"), provider)
	}
	if _, err := s.cfg.DB.Exec(
		`UPDATE tasks SET forked_from_slug = ?, fork_reason = ?, updated_at = ? WHERE slug = ?`,
		source.Slug, reason, productdb.NowISO(), slug,
	); err != nil {
		return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
	}
	fork, err := productdb.GetTask(s.cfg.DB, slug)
	if err != nil {
		return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
	}
	if err := s.writeForkContext(source, fork, reason); err != nil {
		return actionResponse{OK: false, Message: err.Error(), Output: out}, http.StatusInternalServerError
	}
	// Return the freshly-created fork as Agent so the UI can navigate straight
	// to its session (SessionDetail keys its post-fork navigation off resp.agent).
	agent, _ := s.agentForTask(slug)
	return actionResponse{OK: true, Message: "forked " + target + " to " + slug, Output: out, Agent: agent}, http.StatusOK
}

func (s *Server) defaultForkProvider(source *productdb.Task, requested string) string {
	if strings.TrimSpace(requested) != "" {
		return strings.TrimSpace(requested)
	}
	sourceProvider := "claude"
	if source != nil && strings.TrimSpace(source.SessionProvider) != "" {
		sourceProvider = strings.TrimSpace(source.SessionProvider)
	}
	if sourceProvider == "claude" {
		return "codex"
	}
	return "claude"
}

func (s *Server) writeForkContext(source, fork *productdb.Task, reason string) error {
	if source == nil || fork == nil {
		return errors.New("source and fork tasks are required")
	}
	root := strings.TrimSpace(s.cfg.FlowRoot)
	if root == "" {
		return errors.New("flow root is not configured")
	}
	sourceDir := filepath.Join(root, "tasks", source.Slug)
	forkDir := filepath.Join(root, "tasks", fork.Slug)
	if err := os.MkdirAll(filepath.Join(forkDir, "updates"), 0o755); err != nil {
		return err
	}
	var copied []string
	if copiedBrief, err := copyMarkdownFile(filepath.Join(sourceDir, "brief.md"), filepath.Join(forkDir, "source-brief.md")); err != nil {
		return fmt.Errorf("copy source brief: %w", err)
	} else if copiedBrief {
		copied = append(copied, "source-brief.md")
	}
	for _, file := range markdownFiles(filepath.Join(sourceDir, "updates"), false) {
		dstName := "source-" + file.Filename
		if ok, err := copyMarkdownFile(file.Path, filepath.Join(forkDir, "updates", dstName)); err != nil {
			return fmt.Errorf("copy source update %s: %w", file.Filename, err)
		} else if ok {
			copied = append(copied, filepath.Join("updates", dstName))
		}
	}
	for _, file := range auxFiles(sourceDir) {
		dstName := "source-" + file.Filename
		if ok, err := copyMarkdownFile(file.Path, filepath.Join(forkDir, dstName)); err != nil {
			return fmt.Errorf("copy source sidecar %s: %w", file.Filename, err)
		} else if ok {
			copied = append(copied, dstName)
		}
	}
	if transcript, ok, err := s.renderForkTranscript(source); err != nil {
		return fmt.Errorf("render source transcript: %w", err)
	} else if ok {
		if err := os.WriteFile(filepath.Join(forkDir, "source-transcript.md"), []byte(transcript), 0o644); err != nil {
			return fmt.Errorf("write source transcript: %w", err)
		}
		copied = append(copied, "source-transcript.md")
	}
	return os.WriteFile(filepath.Join(forkDir, "brief.md"), []byte(forkBrief(source, fork, reason, copied)), 0o644)
}

func copyMarkdownFile(src, dst string) (bool, error) {
	body, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(dst, body, 0o644)
}

func (s *Server) renderForkTranscript(source *productdb.Task) (string, bool, error) {
	if source == nil || !source.SessionID.Valid || strings.TrimSpace(source.SessionID.String) == "" {
		return "", false, nil
	}
	path, err := sessionJSONLPath(s.cfg.DB, source)
	if err != nil {
		return "", false, nil
	}
	entries, err := parseTranscriptFile(path)
	if err != nil {
		return "", false, err
	}
	if len(entries) == 0 {
		return "", false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Source transcript from %s\n\n", source.Slug)
	for _, entry := range entries {
		label := entry.Type
		if entry.Timestamp != "" {
			label += " " + entry.Timestamp
		}
		body := forkTranscriptEntryBody(entry)
		if body == "" {
			continue
		}
		fmt.Fprintf(&b, "## %s\n%s\n\n", label, body)
	}
	return b.String(), true, nil
}

func forkTranscriptEntryBody(entry TranscriptEntry) string {
	switch entry.Type {
	case "tool_use":
		return strings.TrimSpace(entry.ToolName + "\n" + entry.ToolInputSummary)
	case "tool_result":
		return strings.TrimSpace(entry.ToolResultText)
	default:
		return strings.TrimSpace(entry.Text)
	}
}

func forkBrief(source, fork *productdb.Task, reason string, copied []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", fork.Name)
	fmt.Fprintf(&b, "## What\nContinue `%s` in a forked `%s` session using the copied source context.\n\n", source.Slug, firstNonEmpty(fork.SessionProvider, "claude"))
	fmt.Fprintf(&b, "## Fork lineage\nForked from: %s\nReason: %s\nSource provider: %s\nTarget provider: %s\nSource work_dir: %s\n",
		source.Slug,
		firstNonEmpty(reason, "Provider handoff."),
		firstNonEmpty(source.SessionProvider, "claude"),
		firstNonEmpty(fork.SessionProvider, "claude"),
		source.WorkDir,
	)
	if source.WorktreePath.Valid && strings.TrimSpace(source.WorktreePath.String) != "" {
		fmt.Fprintf(&b, "Source worktree: %s\n", source.WorktreePath.String)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Context copied")
	if len(copied) == 0 {
		fmt.Fprintln(&b, "- No source context files were available to copy.")
	} else {
		for _, item := range copied {
			fmt.Fprintf(&b, "- %s\n", item)
		}
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Done when")
	fmt.Fprintln(&b, "- The forked provider session has consumed the copied source context and continued from the source task's latest state.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Out of scope")
	fmt.Fprintln(&b, "- Mutating the source task while creating this fork.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Open questions")
	fmt.Fprintln(&b, "- None.")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "---\n*This task was forked from `%s`; read the copied source files before starting implementation.*\n", source.Slug)
	return b.String()
}
