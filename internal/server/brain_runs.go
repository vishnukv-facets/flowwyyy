package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"flow/internal/productdb"
)

func (s *Server) handleTaskRuns(w http.ResponseWriter, r *http.Request, task *productdb.Task) {
	if !getOnly(w, r) {
		return
	}
	root, err := productdb.TaskFamilyRoot(s.cfg.DB, task.Slug)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	familySlug := root
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, fmt.Errorf("invalid limit %q", raw), http.StatusBadRequest)
			return
		}
		if n > 100 {
			n = 100
		}
		limit = n
	}
	runs, err := productdb.ListBrainRunsForFamily(s.cfg.DB, familySlug, limit)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	views := make([]BrainRunView, 0, len(runs))
	for _, run := range runs {
		view := brainRunViewFromDB(run)
		if t, err := productdb.GetTask(s.cfg.DB, run.TaskSlug); err == nil {
			attachTaskToBrainRunView(&view, t)
		}
		views = append(views, view)
	}
	writeJSON(w, BrainRunsResponse{TaskSlug: task.Slug, FamilySlug: familySlug, Runs: views})
}

func (s *Server) handleTaskRunDetail(w http.ResponseWriter, r *http.Request, task *productdb.Task, runID string) {
	if !getOnly(w, r) {
		return
	}
	root, err := productdb.TaskFamilyRoot(s.cfg.DB, task.Slug)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	familySlug := root
	run, err := productdb.GetBrainRun(s.cfg.DB, runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if run.FamilySlug != familySlug {
		http.NotFound(w, r)
		return
	}
	view := brainRunViewFromDB(run)
	if t, err := productdb.GetTask(s.cfg.DB, run.TaskSlug); err == nil {
		attachTaskToBrainRunView(&view, t)
	}
	writeJSON(w, view)
}

func brainRunViewFromDB(run *productdb.BrainRun) BrainRunView {
	if run == nil {
		return BrainRunView{}
	}
	return BrainRunView{
		RunID:          run.RunID,
		FamilySlug:     run.FamilySlug,
		TaskSlug:       run.TaskSlug,
		PlanID:         nullStringPtr(run.PlanID),
		Role:           run.Role,
		Provider:       run.Provider,
		RequestedModel: nullStringPtr(run.RequestedModel),
		RequestedTier:  nullStringPtr(run.RequestedTier),
		ResolvedModel:  nullStringPtr(run.ResolvedModel),
		PermissionMode: run.PermissionMode,
		Status:         run.Status,
		PID:            nullInt64Ptr(run.PID),
		SessionID:      nullStringPtr(run.SessionID),
		LogPath:        nullStringPtr(run.LogPath),
		InputSummary:   nullStringPtr(run.InputSummary),
		OutputJSON:     rawJSONFromNullString(run.OutputJSON),
		EvidenceJSON:   rawJSONFromNullString(run.EvidenceJSON),
		ErrorText:      nullStringPtr(run.ErrorText),
		StartedAt:      nullStringPtr(run.StartedAt),
		FinishedAt:     nullStringPtr(run.FinishedAt),
		CreatedAt:      run.CreatedAt,
		UpdatedAt:      run.UpdatedAt,
		Legacy:         run.Legacy,
	}
}

func attachTaskToBrainRunView(view *BrainRunView, task *productdb.Task) {
	if view == nil || task == nil {
		return
	}
	view.TaskName = task.Name
	view.TaskStatus = task.Status
}

func nullInt64Ptr(ni sql.NullInt64) *int64 {
	if !ni.Valid {
		return nil
	}
	return &ni.Int64
}

func rawJSONFromNullString(ns sql.NullString) json.RawMessage {
	if !ns.Valid {
		return nil
	}
	raw := strings.TrimSpace(ns.String)
	if raw == "" {
		return nil
	}
	return json.RawMessage([]byte(raw))
}
