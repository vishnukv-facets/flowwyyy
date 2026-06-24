package server

import (
	"flow/internal/briefing"
	"flow/internal/productdb"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	live, _ := s.cachedLiveAgentSessions()
	tasks, err := productdb.ListTasks(s.cfg.DB, productdb.TaskFilter{IncludeArchived: false, Kind: ""})
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	taskViews := make([]TaskView, 0, len(tasks))
	for _, task := range tasks {
		view, err := BuildTaskView(s.cfg.DB, s.cfg.FlowRoot, task, live)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		taskViews = append(taskViews, view)
	}
	out := OverviewView{
		LiveSessions:        []TaskView{},
		InFlight:            []TaskView{},
		HighPriorityBacklog: []TaskView{},
		Waiting:             []TaskView{},
		Stale:               []TaskView{},
		ActivePlaybooks:     []PlaybookView{},
	}
	for _, task := range taskViews {
		if task.Live {
			out.LiveSessions = append(out.LiveSessions, task)
		}
		if task.Status == "in-progress" && task.Kind == "regular" {
			out.InFlight = append(out.InFlight, task)
		}
		if task.Status == "backlog" && task.Priority == "high" {
			out.HighPriorityBacklog = append(out.HighPriorityBacklog, task)
		}
		if task.WaitingOn != nil {
			out.Waiting = append(out.Waiting, task)
		}
		if task.StaleDays != nil {
			out.Stale = append(out.Stale, task)
		}
	}
	pbs, err := productdb.ListPlaybooks(s.cfg.DB, productdb.PlaybookFilter{})
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	pbViews, err := BuildPlaybookViews(s.cfg.DB, s.cfg.FlowRoot, pbs)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	for _, pb := range pbViews {
		if pb.RunCount7d > 0 {
			out.ActivePlaybooks = append(out.ActivePlaybooks, pb)
		}
	}
	now := time.Now()
	brief, err := briefing.Build(s.cfg.DB, s.cfg.FlowRoot, briefing.Options{
		Now:             now,
		Since:           time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()),
		Limit:           20,
		WaitingSessions: s.waitingSessionsForBriefing(),
	})
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	out.Briefing = brief
	writeJSON(w, out)
}

// waitingSessionsForBriefing resolves the live agents paused for the operator's
// input from the same snapshot the session cards render (uiData.Agents), so the
// briefing's "Needs you" tier and the Live-sessions panel never disagree about
// what is waiting. Returns nil on any snapshot error — the briefing degrades to
// its DB-derived rows rather than failing the whole overview.
func (s *Server) waitingSessionsForBriefing() []briefing.WaitingSession {
	data, err := s.cachedUIData()
	if err != nil {
		return nil
	}
	var out []briefing.WaitingSession
	for _, a := range data.Agents {
		if a.Status != "waiting" {
			continue
		}
		project := ""
		if a.Project != nil {
			project = *a.Project
		}
		out = append(out, briefing.WaitingSession{
			TaskSlug: a.Slug,
			Name:     a.Name,
			Project:  project,
			Detail:   waitingSessionDetail(a),
		})
	}
	return out
}

func waitingSessionDetail(a uiAgent) string {
	if a.WaitingFor != nil {
		if why := strings.TrimSpace(a.WaitingFor.Why); why != "" {
			return "agent is waiting: " + why
		}
	}
	if mins := a.LastActivitySec / 60; mins >= 1 {
		return fmt.Sprintf("agent is paused for your input · idle %dm", mins)
	}
	return "agent is paused for your input"
}
