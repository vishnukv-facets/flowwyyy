package server

import (
	"encoding/json"
	"errors"
	"flow/internal/flowdb"
	"flow/internal/memorysrc"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// memorySourceAllowSet is the set of memory files flow recognizes in its
// registered workdirs (the same scanner that powers AGENT_MEMORY_SOURCES,
// minus CLAUDE.md which the UI never shows). A write is only honored for a
// path in this set, so /api/memory can never be coerced into writing an
// arbitrary file on disk.
func (s *Server) memorySourceAllowSet() map[string]bool {
	set := map[string]bool{}
	regs, err := flowdb.ListWorkdirs(s.cfg.DB)
	if err != nil {
		return set
	}
	dirs := make([]string, 0, len(regs))
	for _, w := range BuildWorkdirViews(s.cfg.DB, regs) {
		dirs = append(dirs, w.Path)
	}
	for _, c := range memorysrc.AgentSources(dirs) {
		if c.Path == "" || memorysrc.IsClaudeMDPath(c.Path) {
			continue
		}
		if abs, err := filepath.Abs(c.Path); err == nil {
			set[filepath.Clean(abs)] = true
		}
	}
	return set
}

// handleMemoryWrite persists an edit to an agent-memory file (AGENTS.md, etc.).
// Body: {"path": "<absolute path>", "text": "<markdown>"}. The path must be an
// existing, editable memory source (see memorySourceAllowSet).
func (s *Server) handleMemoryWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Path string `json:"path"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024)).Decode(&req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	path := filepath.Clean(strings.TrimSpace(req.Path))
	if path == "" || !filepath.IsAbs(path) {
		writeError(w, errors.New("invalid memory path"), http.StatusBadRequest)
		return
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		writeError(w, errors.New("memory file not found"), http.StatusBadRequest)
		return
	}
	if !s.memorySourceAllowSet()[path] {
		writeError(w, errors.New("path is not an editable memory source"), http.StatusForbidden)
		return
	}
	if err := os.WriteFile(path, []byte(req.Text), 0o644); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "path": path})
}
