package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flow/internal/flowdb"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxTerminalAttachmentUploadBytes = 50 << 20

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	writeJSON(w, HealthView{OK: true, Version: s.cfg.Version, FlowRoot: s.cfg.FlowRoot})
}

func (s *Server) handleWorkdirs(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	workdirs, err := flowdb.ListWorkdirs(s.cfg.DB)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, BuildWorkdirViews(s.cfg.DB, workdirs))
}

func (s *Server) handleTags(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	tags, err := flowdb.ListAllTags(s.cfg.DB)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if tags == nil {
		tags = []flowdb.TagCount{}
	}
	writeJSON(w, tags)
}

func (s *Server) handleWebSocketPlaceholder(w http.ResponseWriter, r *http.Request) {
	writeError(w, errors.New("websocket live updates are not implemented in this build; the UI uses live fetches and refresh"), http.StatusNotImplemented)
}

func taskFilterFromQuery(q url.Values) (flowdb.TaskFilter, error) {
	filter := flowdb.TaskFilter{
		Status:          q.Get("status"),
		Project:         q.Get("project"),
		Priority:        q.Get("priority"),
		Tag:             flowdb.NormalizeTag(q.Get("tag")),
		IncludeArchived: boolQuery(q, "include_archived"),
		IncludeDeleted:  boolQuery(q, "include_deleted"),
		DeletedOnly:     boolQuery(q, "deleted"),
	}
	if filter.DeletedOnly {
		filter.IncludeArchived = true
	}
	kind := q.Get("kind")
	switch kind {
	case "", "all":
		filter.Kind = ""
	default:
		filter.Kind = kind
	}
	if q.Get("playbook") != "" {
		filter.PlaybookSlug = q.Get("playbook")
	}
	if filter.Status == "" && !boolQuery(q, "include_done") && !filter.DeletedOnly {
		filter.ExcludeDone = true
	}
	if since := q.Get("since"); since != "" && since != "all" {
		t, err := parseSince(since, time.Now())
		if err != nil {
			return filter, err
		}
		filter.Since = t.Format(time.RFC3339)
	}
	return filter, nil
}

func serveMarkdown(w http.ResponseWriter, path string) {
	body, err := readMarkdown(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, err, http.StatusNotFound)
			return
		}
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func routeParts(w http.ResponseWriter, r *http.Request, prefix string) ([]string, bool) {
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if rest == r.URL.Path {
		http.NotFound(w, r)
		return nil, false
	}
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return nil, true
	}
	raw := strings.Split(rest, "/")
	parts := make([]string, 0, len(raw))
	for _, part := range raw {
		decoded, err := url.PathUnescape(part)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return nil, false
		}
		parts = append(parts, decoded)
	}
	return parts, true
}

func getOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeNotFoundOrError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, err, http.StatusNotFound)
		return
	}
	writeError(w, err, http.StatusInternalServerError)
}

func boolQuery(q url.Values, key string) bool {
	v := strings.ToLower(strings.TrimSpace(q.Get(key)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
