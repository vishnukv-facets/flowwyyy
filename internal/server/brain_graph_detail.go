package server

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"flow/internal/productdb"
)

func (s *Server) handleBrainGraphNodeDetail(w http.ResponseWriter, r *http.Request) {
	parts, ok := routeParts(w, r, "/api/brain/graph/node/")
	if !ok {
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	if !getOnly(w, r) {
		return
	}
	detail, err := BuildBrainGraphNodeDetail(s.cfg.DB, s.cfg.FlowRoot, parts[0])
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, detail)
}

func BuildBrainGraphNodeDetail(db *sql.DB, root, nodeID string) (BrainGraphNodeDetail, error) {
	nodeID = strings.TrimSpace(nodeID)
	switch {
	case strings.HasPrefix(nodeID, "task:"):
		slug := strings.TrimPrefix(nodeID, "task:")
		task, err := productdb.GetTask(db, slug)
		if err != nil {
			return BrainGraphNodeDetail{}, err
		}
		return BrainGraphNodeDetail{
			ID:   "task:" + task.Slug,
			Type: "task",
			Task: brainGraphTaskDetail(root, task),
		}, nil
	case strings.HasPrefix(nodeID, "transcript:"):
		slug := strings.TrimPrefix(nodeID, "transcript:")
		task, err := productdb.GetTask(db, slug)
		if err != nil {
			return BrainGraphNodeDetail{}, err
		}
		evidence := brainGraphTranscriptDetail(task)
		if evidence == nil {
			return BrainGraphNodeDetail{}, sql.ErrNoRows
		}
		return BrainGraphNodeDetail{
			ID:       "transcript:" + task.Slug,
			Type:     "transcript_ref",
			Evidence: evidence,
		}, nil
	case strings.HasPrefix(nodeID, "github:"):
		return brainGraphGitHubEvidenceDetail(db, strings.TrimPrefix(nodeID, "github:"))
	default:
		return BrainGraphNodeDetail{}, sql.ErrNoRows
	}
}

func brainGraphTaskDetail(root string, task *productdb.Task) *BrainGraphTaskDetail {
	briefPath := filepath.Join(root, "tasks", task.Slug, "brief.md")
	updates := markdownFiles(filepath.Join(root, "tasks", task.Slug, "updates"), true)
	if updates == nil {
		updates = []FileRef{}
	}
	if len(updates) > 5 {
		updates = updates[:5]
	}
	return &BrainGraphTaskDetail{
		Slug:            task.Slug,
		Name:            task.Name,
		Status:          task.Status,
		Priority:        task.Priority,
		ProjectSlug:     nullStringPtr(task.ProjectSlug),
		ParentSlug:      nullStringPtr(task.ParentSlug),
		WorkDir:         task.WorkDir,
		WorktreePath:    nullStringPtr(task.WorktreePath),
		SessionProvider: task.SessionProvider,
		Harness:         task.Harness,
		PermissionMode:  task.PermissionMode,
		Model:           nullStringPtr(task.Model),
		SessionID:       nullStringPtr(task.SessionID),
		SessionPath:     nullStringPtr(task.SessionPath),
		Transcript:      brainGraphTranscriptDetail(task),
		BriefPath:       briefPath,
		Updates:         updates,
	}
}

func brainGraphTranscriptDetail(task *productdb.Task) *BrainGraphEvidenceDetail {
	if task == nil || !task.SessionID.Valid || strings.TrimSpace(task.SessionID.String) == "" {
		return nil
	}
	path := nullStringPtr(task.SessionPath)
	available := false
	message := "transcript path not captured"
	if path != nil {
		if _, err := os.Stat(*path); err == nil {
			available = true
			message = ""
		} else {
			message = "transcript file not found"
		}
	}
	return &BrainGraphEvidenceDetail{
		Kind:      "transcript",
		TaskSlug:  task.Slug,
		RefID:     task.SessionID.String,
		Path:      path,
		Available: available,
		Message:   message,
	}
}

func brainGraphGitHubEvidenceDetail(db *sql.DB, escapedTag string) (BrainGraphNodeDetail, error) {
	tag, err := url.PathUnescape(escapedTag)
	if err != nil {
		return BrainGraphNodeDetail{}, err
	}
	tag = productdb.NormalizeTag(tag)
	var taskSlug string
	err = db.QueryRow(`SELECT task_slug FROM task_tags WHERE tag = ? ORDER BY task_slug LIMIT 1`, tag).Scan(&taskSlug)
	if err != nil {
		return BrainGraphNodeDetail{}, err
	}
	urlValue := brainGraphGitHubRefURL(tag)
	evidence := &BrainGraphEvidenceDetail{
		Kind:      "github",
		TaskSlug:  taskSlug,
		RefID:     tag,
		URL:       stringPtrIfNotEmpty(urlValue),
		Available: true,
	}
	return BrainGraphNodeDetail{
		ID:       brainGraphGitHubRefNodeID(tag),
		Type:     "github_ref",
		Evidence: evidence,
	}, nil
}

func stringPtrIfNotEmpty(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}
