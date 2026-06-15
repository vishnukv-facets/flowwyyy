package server

import (
	"database/sql"
	"errors"
	"flow/internal/flowdb"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type terminalAttachmentUploadResponse struct {
	Files      []FileRef `json:"files"`
	InsertText string    `json:"insert_text"`
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	if r.URL.Path != "/api/tasks" {
		http.NotFound(w, r)
		return
	}
	filter, err := taskFilterFromQuery(r.URL.Query())
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
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
}

func (s *Server) handleTaskRoute(w http.ResponseWriter, r *http.Request) {
	parts, ok := routeParts(w, r, "/api/tasks/")
	if !ok {
		return
	}
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	slug := parts[0]
	task, err := flowdb.GetTask(s.cfg.DB, slug)
	if err != nil {
		// Chats are task-less but share the /attachments upload path (the same
		// floating terminal hosts both). Resolve the slug as a chat so image
		// attach works in chat windows; synthesize a minimal task carrying just
		// the slug + provider, which is all the attachment path needs.
		if errors.Is(err, sql.ErrNoRows) && len(parts) == 2 && parts[1] == "attachments" {
			if chat, cerr := flowdb.GetChat(s.cfg.DB, slug); cerr == nil {
				s.handleTaskAttachments(w, r, &flowdb.Task{Slug: chat.Slug, SessionProvider: chat.Provider})
				return
			}
		}
		writeNotFoundOrError(w, err)
		return
	}
	// /attachments accepts writes. Everything else is GET-only.
	if len(parts) == 2 && parts[1] == "attachments" {
		s.handleTaskAttachments(w, r, task)
		return
	}
	if !getOnly(w, r) {
		return
	}
	switch {
	case len(parts) == 1:
		live, _ := s.cachedLiveAgentSessions()
		view, err := BuildTaskView(s.cfg.DB, s.cfg.FlowRoot, task, live)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, view)
	case len(parts) == 2 && parts[1] == "brief":
		serveMarkdown(w, filepath.Join(s.cfg.FlowRoot, "tasks", slug, "brief.md"))
	case len(parts) == 2 && parts[1] == "updates":
		writeJSON(w, markdownFiles(filepath.Join(s.cfg.FlowRoot, "tasks", slug, "updates"), true))
	case len(parts) == 3 && parts[1] == "updates":
		path, err := fileForEntity(s.cfg.FlowRoot, "tasks", slug, "updates", parts[2])
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		serveMarkdown(w, path)
	case len(parts) == 3 && parts[1] == "aux":
		path, err := fileForEntity(s.cfg.FlowRoot, "tasks", slug, "aux", parts[2])
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		serveMarkdown(w, path)
	case len(parts) == 2 && parts[1] == "auto-runs":
		s.handleTaskAutoRunList(w, r, slug)
	case len(parts) == 3 && parts[1] == "auto-runs" && parts[2] == "log":
		s.handleTaskAutoRunLog(w, r, slug)
	case len(parts) == 2 && parts[1] == "transcript":
		s.handleTaskTranscript(w, task)
	case len(parts) == 2 && parts[1] == "runs":
		s.handleTaskRuns(w, r, task)
	case len(parts) == 3 && parts[1] == "runs":
		s.handleTaskRunDetail(w, r, task, parts[2])
	case len(parts) == 2 && parts[1] == "bridge":
		agent, err := s.agentForTask(slug)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, agent)
	case len(parts) == 2 && parts[1] == "workspace":
		writeJSON(w, workspaceTree(task.WorkDir))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleTaskAttachments(w http.ResponseWriter, r *http.Request, task *flowdb.Task) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTerminalAttachmentUploadBytes)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	headers := r.MultipartForm.File["files"]
	if len(headers) == 0 {
		headers = r.MultipartForm.File["file"]
	}
	if len(headers) == 0 {
		writeError(w, errors.New("no files uploaded"), http.StatusBadRequest)
		return
	}
	files, err := s.saveTaskAttachmentFiles(task.Slug, headers)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	writeJSON(w, terminalAttachmentUploadResponse{
		Files:      files,
		InsertText: attachmentInsertText(task.SessionProvider, paths),
	})
}

// attachmentInsertText frames uploaded attachment paths for injection into the
// live agent's input box. The two providers detect an image-file path very
// differently, so this is deliberately provider-aware:
//
//   - Claude Code only runs its image-path → `[Image #N]` collapse on
//     BRACKETED-PASTE input (ESC[200~ … ESC[201~) — never on the
//     character-by-character typed bytes the terminal bridge actually delivers.
//     So we wrap each path as its own paste. The path must be UNQUOTED for the
//     detection to fire (bracketed paste already carries spaces verbatim, so the
//     shell-quoting we use elsewhere would instead break the match). An earlier
//     `@<path>` prefix triggered Claude's file-MENTION picker, which just left
//     the raw path sitting in the prompt — the opposite of attaching it.
//   - Codex collapses a plain typed path already, and its TUI is not guaranteed
//     to honor bracketed paste (the delimiters could land as literal garbage),
//     so we leave Codex on the bare shell-quoted path that works today.
func attachmentInsertText(provider string, paths []string) string {
	out := make([]string, len(paths))
	if strings.TrimSpace(provider) == "codex" {
		for i, p := range paths {
			out[i] = shellQuoteArg(p)
		}
		return strings.Join(out, " ")
	}
	for i, p := range paths {
		out[i] = "\x1b[200~" + p + "\x1b[201~"
	}
	return strings.Join(out, " ")
}

func (s *Server) saveTaskAttachmentFiles(taskSlug string, headers []*multipart.FileHeader) ([]FileRef, error) {
	if len(headers) == 0 {
		return nil, errors.New("no files uploaded")
	}
	if err := validateSlug(taskSlug); err != nil {
		return nil, err
	}
	destDir := filepath.Join(s.cfg.FlowRoot, "tasks", taskSlug, "attachments")
	if !strings.HasPrefix(filepath.Clean(destDir), filepath.Join(s.cfg.FlowRoot, "tasks")+string(os.PathSeparator)) {
		return nil, errors.New("invalid task attachment path")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	now := time.Now()
	files := make([]FileRef, 0, len(headers))
	for i, header := range headers {
		src, err := header.Open()
		if err != nil {
			return nil, err
		}
		name := uploadedAttachmentFilename(header.Filename, header.Header.Get("Content-Type"), now, i)
		path := uniqueAttachmentPath(destDir, name)
		dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			_ = src.Close()
			return nil, err
		}
		n, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		_ = src.Close()
		if copyErr != nil {
			_ = os.Remove(path)
			return nil, copyErr
		}
		if closeErr != nil {
			_ = os.Remove(path)
			return nil, closeErr
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		files = append(files, FileRef{
			Filename: filepath.Base(path),
			Path:     path,
			MTime:    now.Format(time.RFC3339),
			Size:     n,
		})
	}
	return files, nil
}

func uploadedAttachmentFilename(name, contentType string, now time.Time, index int) string {
	name = strings.TrimSpace(filepath.Base(name))
	if name == "." || name == "" {
		name = "attachment" + attachmentExtForContentType(contentType)
	}
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		allowed := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-' || r == '.'
		if allowed {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	clean := strings.Trim(b.String(), "-.")
	if clean == "" {
		clean = "attachment" + attachmentExtForContentType(contentType)
	}
	if len(clean) > 96 {
		ext := filepath.Ext(clean)
		base := strings.TrimSuffix(clean, ext)
		if len(ext) > 16 {
			ext = ""
		}
		if len(base) > 96-len(ext) {
			base = base[:96-len(ext)]
		}
		clean = strings.Trim(base, "-.") + ext
	}
	prefix := now.Format("20060102-150405")
	return fmt.Sprintf("%s-%02d-%s", prefix, index+1, clean)
}

func attachmentExtForContentType(contentType string) string {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch contentType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	default:
		return ""
	}
}

func uniqueAttachmentPath(dir, name string) string {
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return path
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s-%d%s", base, i, ext))
		if _, err := os.Stat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate
		}
	}
}

type autoRunFileEntry struct {
	File     string `json:"file"`
	Size     int64  `json:"size"`
	Modified string `json:"modified"`
}

const autoRunLogMaxBytes = 256 * 1024

func (s *Server) handleTaskAutoRunList(w http.ResponseWriter, _ *http.Request, slug string) {
	dir := filepath.Join(s.cfg.FlowRoot, "tasks", slug, "auto-runs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeJSON(w, []autoRunFileEntry{})
			return
		}
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	out := make([]autoRunFileEntry, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.IsDir() || filepath.Ext(e.Name()) != ".log" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, autoRunFileEntry{
			File:     e.Name(),
			Size:     info.Size(),
			Modified: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, out)
}

var autoRunLogNameRE = regexp.MustCompile(`^[0-9-]+\.log$`)

type autoRunLogResponse struct {
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (s *Server) handleTaskAutoRunLog(w http.ResponseWriter, r *http.Request, slug string) {
	name := r.URL.Query().Get("file")
	if !autoRunLogNameRE.MatchString(name) {
		writeError(w, errors.New("invalid file name"), http.StatusBadRequest)
		return
	}
	dir := filepath.Join(s.cfg.FlowRoot, "tasks", slug, "auto-runs")
	path := filepath.Clean(filepath.Join(dir, name))
	if !strings.HasPrefix(path, filepath.Clean(dir)+string(filepath.Separator)) {
		writeError(w, errors.New("invalid file path"), http.StatusBadRequest)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, errors.New("log not found"), http.StatusNotFound)
			return
		}
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	truncated := false
	if len(data) > autoRunLogMaxBytes {
		data = data[len(data)-autoRunLogMaxBytes:]
		truncated = true
	}
	writeJSON(w, autoRunLogResponse{Content: string(data), Truncated: truncated})
}

func (s *Server) handleTaskTranscript(w http.ResponseWriter, task *flowdb.Task) {
	path, err := sessionJSONLPath(s.cfg.DB, task)
	if err != nil {
		writeJSON(w, TranscriptResponse{Available: false, Message: err.Error(), Entries: []TranscriptEntry{}})
		return
	}
	entry, err := s.transcripts.get(path)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, TranscriptResponse{Available: true, Entries: entry.entries})
}
