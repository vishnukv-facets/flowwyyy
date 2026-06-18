package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

type operatorAskRequest struct {
	TaskSlug string `json:"task_slug"`
	Question string `json:"question"`
}

func (s *Server) handleOperatorAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.DB == nil {
		writeError(w, errors.New("database is not configured"), http.StatusServiceUnavailable)
		return
	}
	var req operatorAskRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeError(w, fmt.Errorf("invalid operator ask payload: %w", err), http.StatusBadRequest)
		return
	}
	slug := strings.TrimSpace(req.TaskSlug)
	question := strings.TrimSpace(req.Question)
	if slug == "" {
		writeError(w, errors.New("task_slug is required"), http.StatusBadRequest)
		return
	}
	if question == "" {
		writeError(w, errors.New("question is required"), http.StatusBadRequest)
		return
	}
	if _, err := flowdb.GetTask(s.cfg.DB, slug); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, err, http.StatusNotFound)
			return
		}
		writeError(w, err, http.StatusInternalServerError)
		return
	}

	channel, threadTS, err := monitor.AskOperator(r.Context(), operatorAskMessage(slug, question))
	if err != nil {
		writeError(w, err, http.StatusBadGateway)
		return
	}
	threadKey := monitor.ThreadKey(channel, threadTS)
	if threadKey == "" {
		writeError(w, errors.New("operator ask returned an empty Slack thread key"), http.StatusBadGateway)
		return
	}
	if err := flowdb.AddTaskTag(s.cfg.DB, slug, monitor.SlackThreadTagPrefix+threadKey); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if _, err := flowdb.SetTaskWaitingOnIfClear(s.cfg.DB, slug, operatorWaitingNote(question)); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	s.publishUIChange("tasks")
	writeJSON(w, map[string]any{"ok": true, "channel": channel, "thread_ts": threadTS})
}

func operatorAskMessage(slug, question string) string {
	return fmt.Sprintf("Task `%s` needs your input:\n\n%s\n\nReply in this thread; flow will route your answer back to the task.", slug, question)
}

func operatorWaitingNote(question string) string {
	text := strings.Join(strings.Fields(question), " ")
	if len(text) > 160 {
		text = text[:160] + "..."
	}
	return monitor.OperatorQuestionWaitingPrefix + " " + text
}
