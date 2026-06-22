package server

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"flow/internal/steering"
)

const personaMaxBytes = 32 * 1024

func (s *Server) personaFilePath() string {
	root := strings.TrimSpace(s.cfg.FlowRoot)
	if root == "" {
		root = "."
	}
	return filepath.Join(root, "persona.md")
}

// handlePersona serves GET/PUT /api/persona — the operator's outbound "voice"
// markdown. The steerer injects it into drafting + send prompts so replies read
// like the operator, not a bot (see internal/steering/persona.go). GET returns
// the text (empty when unset, not 404, so the editor opens blank); PUT replaces
// it, checkpointed first so an edit is reversible via the backup history.
func (s *Server) handlePersona(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		body, err := os.ReadFile(s.personaFilePath())
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		// Return ONLY the editable voice (guidance HTML comments stripped) — the
		// notes are shown statically in the UI, not inside the editable box. No
		// saved voice → pre-fill the built-in default so the operator sees (and
		// can tweak) the tone that's actually in effect, not a blank box.
		voice := steering.StripPersonaComments(string(body))
		if voice == "" {
			voice = steering.DefaultVoiceText()
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(voice))
	case http.MethodPut:
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, personaMaxBytes))
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		path := s.personaFilePath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		s.backupCheckpoint("before persona edit")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
