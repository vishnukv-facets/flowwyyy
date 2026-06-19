package server

import (
	"errors"
	"flow/internal/flowdb"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	if r.URL.Path != "/api/projects" {
		http.NotFound(w, r)
		return
	}
	filter := flowdb.ProjectFilter{
		Status:          r.URL.Query().Get("status"),
		IncludeArchived: boolQuery(r.URL.Query(), "include_archived"),
		IncludeDeleted:  boolQuery(r.URL.Query(), "include_deleted"),
		DeletedOnly:     boolQuery(r.URL.Query(), "deleted"),
	}
	if filter.DeletedOnly {
		filter.IncludeArchived = true
	}
	projects, err := flowdb.ListProjects(s.cfg.DB, filter)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	views, err := BuildProjectViews(s.cfg.DB, s.cfg.FlowRoot, projects)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, views)
}

func (s *Server) handleProjectRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parts, ok := routeParts(w, r, "/api/projects/")
	if !ok {
		return
	}
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	slug := parts[0]
	project, err := flowdb.GetProject(s.cfg.DB, slug)
	if err != nil {
		writeNotFoundOrError(w, err)
		return
	}
	switch {
	case len(parts) == 1:
		if !getOnly(w, r) {
			return
		}
		view, err := BuildProjectView(s.cfg.DB, s.cfg.FlowRoot, project)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, view)
	case len(parts) == 2 && parts[1] == "brief":
		if r.Method == http.MethodPut {
			s.saveProjectBrief(w, r, project)
			return
		}
		if !getOnly(w, r) {
			return
		}
		serveMarkdown(w, filepath.Join(s.cfg.FlowRoot, "projects", slug, "brief.md"))
	case len(parts) == 2 && parts[1] == "updates":
		if !getOnly(w, r) {
			return
		}
		writeJSON(w, markdownFiles(filepath.Join(s.cfg.FlowRoot, "projects", slug, "updates"), true))
	case len(parts) == 3 && parts[1] == "updates":
		if !getOnly(w, r) {
			return
		}
		path, err := fileForEntity(s.cfg.FlowRoot, "projects", slug, "updates", parts[2])
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		serveMarkdown(w, path)
	case len(parts) == 2 && parts[1] == "tasks":
		if !getOnly(w, r) {
			return
		}
		filter := flowdb.TaskFilter{
			Project:        slug,
			IncludeDeleted: boolQuery(r.URL.Query(), "include_deleted"),
			DeletedOnly:    boolQuery(r.URL.Query(), "deleted"),
			ExcludeDone:    !boolQuery(r.URL.Query(), "include_done") && !boolQuery(r.URL.Query(), "deleted"),
		}
		if filter.DeletedOnly {
			filter.IncludeArchived = true
		}
		tasks, err := flowdb.ListTasks(s.cfg.DB, filter)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		live, _ := s.cachedLiveAgentSessions()
		views, err := buildTaskViewsWithLive(s.cfg.DB, s.cfg.FlowRoot, tasks, live)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, views)
	case len(parts) == 2 && parts[1] == "playbooks":
		if !getOnly(w, r) {
			return
		}
		pbs, err := flowdb.ListPlaybooks(s.cfg.DB, flowdb.PlaybookFilter{Project: slug})
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		views, err := BuildPlaybookViews(s.cfg.DB, s.cfg.FlowRoot, pbs)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, views)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) saveProjectBrief(w http.ResponseWriter, r *http.Request, project *flowdb.Project) {
	path := filepath.Join(s.cfg.FlowRoot, "projects", project.Slug, "brief.md")
	cleanBase := filepath.Join(filepath.Clean(s.cfg.FlowRoot), "projects") + string(os.PathSeparator)
	cleanPath := filepath.Clean(path)
	if !strings.HasPrefix(cleanPath, cleanBase) {
		writeError(w, errors.New("invalid project brief path"), http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	s.backupCheckpoint("before project brief edit " + project.Slug)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	now := flowdb.NowISO()
	if _, err := s.cfg.DB.Exec(`UPDATE projects SET updated_at = ? WHERE slug = ?`, now, project.Slug); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "slug": project.Slug, "updated_at": now})
}
