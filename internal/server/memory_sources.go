package server

import (
	"errors"
	"flow/internal/flowdb"
	"flow/internal/memorysrc"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type memorySourceCandidate struct {
	id       string
	provider string
	scope    string
	kind     string
	label    string
	path     string
}

func (s *Server) uiAgentMemorySources(tasks []TaskView, projects []uiProject, playbooks []uiPlaybook, workdirs []uiWorkdir) []uiMemorySource {
	return s.uiAgentMemorySourcesWithContent(tasks, projects, playbooks, workdirs, false)
}

func (s *Server) uiAgentMemorySourcesWithContent(tasks []TaskView, projects []uiProject, playbooks []uiPlaybook, workdirs []uiWorkdir, includeContent bool) []uiMemorySource {
	candidates := memorysrc.AgentSources(memorySourceWorkdirs(tasks, projects, playbooks, workdirs))

	out := make([]uiMemorySource, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate.ID == "" {
			continue
		}
		if memorysrc.IsClaudeMDPath(candidate.Path) {
			continue
		}
		if seen[candidate.ID] {
			candidate.ID = candidate.ID + "-" + memorysrc.MemorySourceSlug(candidate.Path)
			if candidate.ID == "" || seen[candidate.ID] {
				continue
			}
		}
		seen[candidate.ID] = true
		out = append(out, buildMemorySource(memorySourceCandidate{
			id:       candidate.ID,
			provider: candidate.Provider,
			scope:    candidate.Scope,
			kind:     candidate.Kind,
			label:    candidate.Label,
			path:     candidate.Path,
		}, includeContent))
	}
	return out
}

func (s *Server) handleMemorySources(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	live, _ := s.cachedLiveAgentSessions()
	tasks, err := flowdb.ListTasks(s.cfg.DB, flowdb.TaskFilter{Kind: "", IncludeArchived: false})
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	taskViews, err := buildTaskViewsWithLive(s.cfg.DB, s.cfg.FlowRoot, tasks, live)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	projects, err := s.uiProjects()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	playbooks, err := s.uiPlaybooks()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	workdirs, err := s.uiWorkdirs()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.uiAgentMemorySourcesWithContent(taskViews, projects, playbooks, workdirs, true))
}

func memorySourceWorkdirs(tasks []TaskView, projects []uiProject, playbooks []uiPlaybook, workdirs []uiWorkdir) []string {
	seen := map[string]bool{}
	var out []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		path = filepath.Clean(path)
		if seen[path] {
			return
		}
		seen[path] = true
		out = append(out, path)
	}
	for _, task := range tasks {
		add(task.WorkDir)
	}
	for _, project := range projects {
		add(project.WorkDir)
	}
	for _, playbook := range playbooks {
		add(playbook.WorkDir)
	}
	for _, workdir := range workdirs {
		add(workdir.Path)
	}
	sort.Strings(out)
	return out
}

func claudeAutoMemoryDir(workdir string) string {
	return memorysrc.ClaudeAutoMemoryDir(workdir)
}

func isClaudeMDPath(path string) bool {
	return memorysrc.IsClaudeMDPath(path)
}

func claudeProjectKey(path string) string {
	return memorysrc.ClaudeProjectKey(path)
}

func buildMemorySource(candidate memorySourceCandidate, includeContent bool) uiMemorySource {
	src := uiMemorySource{
		ID:       candidate.id,
		Provider: candidate.provider,
		Scope:    candidate.scope,
		Kind:     candidate.kind,
		Label:    candidate.label,
		Path:     candidate.path,
		Status:   "missing",
	}
	info, err := os.Stat(candidate.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			src.Status = "unavailable"
			src.Error = err.Error()
		}
		return src
	}
	if info.IsDir() {
		src.Status = "unavailable"
		src.Error = "path is a directory"
		return src
	}
	src.MTime = info.ModTime().Format(time.RFC3339Nano)
	src.Size = info.Size()
	src.Format = "text"
	if strings.EqualFold(filepath.Ext(candidate.path), ".md") {
		src.Format = "markdown"
	}
	if !includeContent {
		src.Status = "available"
		src.Available = true
		return src
	}
	body, err := os.ReadFile(candidate.path)
	if err != nil {
		src.Status = "unavailable"
		src.Error = err.Error()
		return src
	}
	src.Status = "available"
	src.Available = true
	src.Content = string(body)
	return src
}
