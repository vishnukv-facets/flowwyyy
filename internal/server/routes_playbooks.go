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

func (s *Server) handlePlaybooks(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	if r.URL.Path != "/api/playbooks" {
		http.NotFound(w, r)
		return
	}
	filter := flowdb.PlaybookFilter{
		Project:         r.URL.Query().Get("project"),
		IncludeArchived: boolQuery(r.URL.Query(), "include_archived"),
		IncludeDeleted:  boolQuery(r.URL.Query(), "include_deleted"),
		DeletedOnly:     boolQuery(r.URL.Query(), "deleted"),
	}
	if filter.DeletedOnly {
		filter.IncludeArchived = true
	}
	pbs, err := flowdb.ListPlaybooks(s.cfg.DB, filter)
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
}

func (s *Server) handlePlaybookRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parts, ok := routeParts(w, r, "/api/playbooks/")
	if !ok {
		return
	}
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	slug := parts[0]
	pb, err := flowdb.GetPlaybook(s.cfg.DB, slug)
	if err != nil {
		writeNotFoundOrError(w, err)
		return
	}
	switch {
	case len(parts) == 1:
		if !getOnly(w, r) {
			return
		}
		view, err := BuildPlaybookView(s.cfg.DB, s.cfg.FlowRoot, pb)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, view)
	case len(parts) == 2 && parts[1] == "brief":
		if r.Method == http.MethodPut {
			s.savePlaybookBrief(w, r, pb)
			return
		}
		if !getOnly(w, r) {
			return
		}
		serveMarkdown(w, filepath.Join(s.cfg.FlowRoot, "playbooks", pb.Slug, "brief.md"))
	case len(parts) == 2 && parts[1] == "updates":
		if !getOnly(w, r) {
			return
		}
		writeJSON(w, markdownFiles(filepath.Join(s.cfg.FlowRoot, "playbooks", slug, "updates"), true))
	case len(parts) == 3 && parts[1] == "updates":
		if !getOnly(w, r) {
			return
		}
		path, err := fileForEntity(s.cfg.FlowRoot, "playbooks", slug, "updates", parts[2])
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		serveMarkdown(w, path)
	case len(parts) == 3 && parts[1] == "aux":
		if !getOnly(w, r) {
			return
		}
		path, err := fileForEntity(s.cfg.FlowRoot, "playbooks", slug, "aux", parts[2])
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		serveMarkdown(w, path)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) savePlaybookBrief(w http.ResponseWriter, r *http.Request, pb *flowdb.Playbook) {
	path := filepath.Join(s.cfg.FlowRoot, "playbooks", pb.Slug, "brief.md")
	cleanBase := filepath.Join(filepath.Clean(s.cfg.FlowRoot), "playbooks") + string(os.PathSeparator)
	cleanPath := filepath.Clean(path)
	if !strings.HasPrefix(cleanPath, cleanBase) {
		writeError(w, errors.New("invalid playbook brief path"), http.StatusBadRequest)
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
	s.backupCheckpoint("before playbook brief edit " + pb.Slug)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	now := flowdb.NowISO()
	if _, err := s.cfg.DB.Exec(`UPDATE playbooks SET updated_at = ? WHERE slug = ?`, now, pb.Slug); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "slug": pb.Slug, "updated_at": now})
}
