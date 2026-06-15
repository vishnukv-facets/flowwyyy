package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errFileChangedOnDisk = errors.New("file changed on disk since it was loaded; reload before saving")

func (s *Server) handleKB(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	if r.URL.Path != "/api/kb" {
		http.NotFound(w, r)
		return
	}
	views := []KBFileView{}
	for _, path := range kbFiles(s.cfg.FlowRoot) {
		views = append(views, BuildKBFileView(path))
	}
	writeJSON(w, views)
}

// handleKBDream serves the KB "dreaming" hygiene worker's state (GET) and
// triggers an out-of-band dream pass (POST). The POST is operator-initiated
// from the Knowledge page; it spawns a headless agent pass, so it's a no-op
// while one is already running.
func (s *Server) handleKBDream(w http.ResponseWriter, r *http.Request) {
	if s.kbDreamer == nil {
		writeJSON(w, KBDreamStatus{History: []KBDreamRecord{}})
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		writeJSON(w, s.kbDreamer.dreamStatus())
	case http.MethodPost:
		started := s.kbDreamer.triggerDream()
		st := s.kbDreamer.dreamStatus()
		w.Header().Set("Content-Type", "application/json")
		if !started {
			w.WriteHeader(http.StatusConflict)
		}
		writeJSON(w, st)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleKBFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parts, ok := routeParts(w, r, "/api/kb/")
	if !ok {
		return
	}
	if len(parts) != 1 || !validFilename(parts[0]) {
		writeError(w, errors.New("invalid KB filename"), http.StatusBadRequest)
		return
	}
	allowed := false
	for _, path := range kbFiles(s.cfg.FlowRoot) {
		if filepath.Base(path) == parts[0] {
			allowed = true
			break
		}
	}
	if !allowed {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.cfg.FlowRoot, "kb", parts[0])
	if r.Method == http.MethodPut {
		s.saveKBFile(w, r, path, parts[0])
		return
	}
	serveMarkdown(w, path)
}

func (s *Server) saveKBFile(w http.ResponseWriter, r *http.Request, path, name string) {
	// Confine writes to the kb/ dir even if the (already-validated) name somehow
	// resolves elsewhere — defense in depth, matching saveProjectBrief.
	cleanBase := filepath.Join(filepath.Clean(s.cfg.FlowRoot), "kb") + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(path), cleanBase) {
		writeError(w, errors.New("invalid KB path"), http.StatusBadRequest)
		return
	}
	if err := requireUnmodifiedFile(path, r.URL.Query().Get("mtime")); err != nil {
		writeError(w, err, statusForMTimeError(err))
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
	if err := os.WriteFile(path, body, 0o644); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "name": name, "mtime": BuildKBFileView(path).MTime})
}

func requireUnmodifiedFile(path, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	want, err := time.Parse(time.RFC3339Nano, expected)
	if err != nil {
		return fmt.Errorf("invalid mtime %q: %w", expected, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.ModTime().Equal(want) {
		return errFileChangedOnDisk
	}
	return nil
}

func statusForMTimeError(err error) int {
	if errors.Is(err, errFileChangedOnDisk) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}
