package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"flow/internal/flowbackup"
)

// backupStatus assembles the observable backup state, using the live scheduler
// when present and falling back to persisted state otherwise.
func (s *Server) backupStatus() BackupStatus {
	if s.backupSched != nil {
		return s.backupSched.status()
	}
	root := s.cfg.FlowRoot
	st := flowbackup.LoadSchedState(root)
	return BackupStatus{
		Enabled:          flowbackup.Enabled(),
		Schedule:         st.Schedule,
		LastRunAt:        st.LastRunAt,
		NextRunAt:        st.NextRunAt,
		LastPushAt:       st.LastPushAt,
		Commits:          flowbackup.CommitCount(root),
		DBSnapshots:      flowbackup.DBSnapshotCount(root),
		RemoteConfigured: flowbackup.RemoteConfigured(root),
		RemoteURL:        flowbackup.RemoteURL(root),
		History:          []BackupRunRecord{},
	}
}

// handleBackupStatus: GET observable state of the backup subsystem.
func (s *Server) handleBackupStatus(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	writeJSON(w, s.backupStatus())
}

// handleBackupLog: GET checkpoint history, optionally for one file (?file=) and
// limited (?limit=).
func (s *Server) handleBackupLog(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	file := strings.TrimSpace(r.URL.Query().Get("file"))
	limit := 100
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			limit = n
		}
	}
	commits, err := flowbackup.Log(s.cfg.FlowRoot, file, limit)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if commits == nil {
		commits = []flowbackup.Commit{}
	}
	writeJSON(w, commits)
}

// handleBackupShow: GET a file's content at a revision (?rev=&file=), or its diff
// against the current working copy when ?diff=1.
func (s *Server) handleBackupShow(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	q := r.URL.Query()
	rev := strings.TrimSpace(q.Get("rev"))
	file := strings.TrimSpace(q.Get("file"))
	if rev == "" || file == "" {
		writeError(w, errStr("rev and file are required"), http.StatusBadRequest)
		return
	}
	if q.Get("diff") == "1" || q.Get("diff") == "true" {
		out, err := flowbackup.Diff(s.cfg.FlowRoot, rev, file)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"file": file, "rev": rev, "diff": out})
		return
	}
	body, err := flowbackup.Show(s.cfg.FlowRoot, rev, file)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"file": file, "rev": rev, "content": string(body)})
}

// handleBackupRestore: POST {file, rev} rolls a file back to a prior version.
func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		File string `json:"file"`
		Rev  string `json:"rev"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.File) == "" || strings.TrimSpace(req.Rev) == "" {
		writeError(w, errStr("file and rev are required"), http.StatusBadRequest)
		return
	}
	if err := flowbackup.Restore(s.cfg.FlowRoot, req.File, req.Rev); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "file": req.File, "rev": req.Rev})
}

// handleBackupNow: POST forces a checkpoint + db snapshot now.
func (s *Server) handleBackupNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	committed, err := flowbackup.Checkpoint(s.cfg.FlowRoot, "manual backup (UI)")
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	snap, _ := flowbackup.SnapshotDB(s.cfg.FlowRoot)
	writeJSON(w, map[string]any{"ok": true, "committed": committed, "db_snapshot": snap != ""})
}

// errStr is a tiny error helper for request validation.
type errStr string

func (e errStr) Error() string { return string(e) }
