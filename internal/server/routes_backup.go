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
		OffsiteMode:      backupOffsiteMode(),
		TokenSet:         flowbackup.TokenConfigured(),
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

// handleBackupToken: POST {token} stores (or, when empty, clears) the personal
// GitHub token used to provision + push the offsite backup repo. The token lives
// in the OS keyring (never config.json) and is hydrated into FLOW_BACKUP_TOKEN.
// A non-empty token is validated against GitHub before it's accepted, so a typo
// is rejected immediately instead of silently failing on the next backup.
func (s *Server) handleBackupToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		if err := storeBackupSecret(""); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "token_set": false})
		return
	}
	// Store first (sets the env live), then validate by resolving the account.
	if err := storeBackupSecret(token); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	login := flowbackup.GitHubLogin()
	if login == "" {
		// Roll back the bad token so a failed attempt doesn't leave a dud behind.
		_ = storeBackupSecret("")
		writeError(w, errStr("that token didn't authenticate with GitHub — check it has the 'repo' scope (classic) or Administration+Contents (fine-grained) and try again"), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "token_set": true, "login": login})
}

// errStr is a tiny error helper for request validation.
type errStr string

func (e errStr) Error() string { return string(e) }
