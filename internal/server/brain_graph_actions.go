package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"flow/internal/productdb"
)

const maxBrainGraphActionBodyBytes = 64 * 1024

type brainGraphActionNode struct {
	ID       string
	Type     string
	TaskSlug string
}

func (s *Server) handleBrainGraphAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req BrainGraphActionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBrainGraphActionBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	resp, status := s.runBrainGraphAction(req)
	writeJSONStatus(w, resp, status)
}

func (s *Server) runBrainGraphAction(req BrainGraphActionRequest) (BrainGraphActionResponse, int) {
	req.Action = strings.ToLower(strings.TrimSpace(req.Action))
	req.NodeID = strings.TrimSpace(req.NodeID)
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.Actor = strings.TrimSpace(req.Actor)
	if req.Actor == "" {
		req.Actor = "operator"
	}
	base := BrainGraphActionResponse{
		Action: req.Action,
		NodeID: req.NodeID,
	}
	if req.Action == "" {
		base.Message = "action is required"
		return base, http.StatusBadRequest
	}
	if req.NodeID == "" {
		base.Message = "node_id is required"
		return base, http.StatusBadRequest
	}

	node, err := resolveBrainGraphActionNode(s.cfg.DB, req.NodeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			base.Message = "graph node not found: " + req.NodeID
			return base, http.StatusNotFound
		}
		base.Message = err.Error()
		return base, http.StatusInternalServerError
	}

	switch req.Action {
	case "open_session", "resume":
		if node.Type != "task" {
			base.Message = req.Action + " is available for task nodes only"
			return base, http.StatusBadRequest
		}
		resp, status := s.openBrowserTerminalBridge(node.TaskSlug, "")
		base.OK = resp.OK
		base.Message = resp.Message
		base.Output = resp.Output
		base.ActionResponse = &resp
		if resp.OK {
			s.publishUIChange("brain-graph")
		}
		return base, status
	case "send_event", "seed":
		if node.Type != "task" {
			base.Message = req.Action + " is available for task nodes only"
			return base, http.StatusBadRequest
		}
		return s.runBrainGraphSessionEventAction(base, req, node)
	default:
		base.Message = "unknown graph action " + req.Action
		return base, http.StatusBadRequest
	}
}

func resolveBrainGraphActionNode(db *sql.DB, nodeID string) (brainGraphActionNode, error) {
	nodeID = strings.TrimSpace(nodeID)
	if slug, ok := strings.CutPrefix(nodeID, "task:"); ok {
		slug = strings.TrimSpace(slug)
		task, err := productdb.GetTask(db, slug)
		if err != nil {
			return brainGraphActionNode{}, err
		}
		if !brainGraphTaskIsActionable(task) {
			return brainGraphActionNode{}, sql.ErrNoRows
		}
		return brainGraphActionNode{
			ID:       "task:" + task.Slug,
			Type:     "task",
			TaskSlug: task.Slug,
		}, nil
	}
	return brainGraphActionNode{}, sql.ErrNoRows
}

func brainGraphTaskIsActionable(task *productdb.Task) bool {
	return task != nil && !task.ArchivedAt.Valid && !task.DeletedAt.Valid
}

func (s *Server) runBrainGraphSessionEventAction(base BrainGraphActionResponse, req BrainGraphActionRequest, node brainGraphActionNode) (BrainGraphActionResponse, int) {
	if req.Prompt == "" {
		base.Message = "prompt is required for " + req.Action
		return base, http.StatusBadRequest
	}
	deliveredPrompt := brainGraphSessionEventPrompt(req.Action, node.TaskSlug, req.Prompt)
	resp, status := s.nudgeSession(node.TaskSlug, deliveredPrompt)
	base.OK = resp.OK
	base.Message = resp.Message
	base.Output = resp.Output
	base.ActionResponse = &resp
	if resp.OK {
		s.publishUIChange("brain-graph")
	}
	return base, status
}

func brainGraphSessionEventPrompt(action, taskSlug, prompt string) string {
	label := "event"
	if action == "seed" {
		label = "seed input"
	}
	return fmt.Sprintf("Flow Graph %s for %s:\n\n%s", label, taskSlug, prompt)
}
