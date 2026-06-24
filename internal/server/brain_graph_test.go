package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flow/internal/flowdb"
	"flow/internal/productdb"
)

func graphTestTime() time.Time {
	return time.Date(2026, 6, 12, 10, 0, 0, 0, time.FixedZone("IST", 19800))
}

func TestBrainGraphEmptyRoute(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/brain/graph", nil)
	s.handleBrainGraph(rec, req)

	assertBrainGraphEmptyResponse(t, rec)
}

func TestBrainGraphEmptyRouteRegistered(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/brain/graph", nil)
	s.Handler().ServeHTTP(rec, req)

	assertBrainGraphEmptyResponse(t, rec)
}

func TestBrainGraphTaskDetailIncludesTaskAndTranscriptRef(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root})
	if _, err := db.Exec(
		`INSERT INTO projects (slug, name, status, priority, work_dir, created_at, updated_at)
		 VALUES ('flow-manager', 'Flow Manager', 'active', 'high', ?, '2026-06-12T10:00:00+05:30', '2026-06-12T10:00:00+05:30')`,
		root,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	insertBrainGraphTask(t, db, "parent", "Parent", "backlog", nil)
	insertBrainGraphTask(t, db, "worker", "Worker", "backlog", strPtr("parent"))
	taskDir := filepath.Join(root, "tasks", "worker")
	updatesDir := filepath.Join(taskDir, "updates")
	if err := os.MkdirAll(updatesDir, 0o755); err != nil {
		t.Fatalf("mkdir updates: %v", err)
	}
	briefPath := filepath.Join(taskDir, "brief.md")
	if err := os.WriteFile(briefPath, []byte("# Worker\n"), 0o644); err != nil {
		t.Fatalf("write brief: %v", err)
	}
	if err := os.WriteFile(filepath.Join(updatesDir, "2026-06-12-progress.md"), []byte("progress\n"), 0o644); err != nil {
		t.Fatalf("write update: %v", err)
	}
	missingTranscript := filepath.Join(root, "missing", "session.jsonl")
	worktreePath := filepath.Join(root, "worktrees", "worker")
	if _, err := db.Exec(
		`UPDATE tasks
		 SET project_slug='flow-manager',
		     status='in-progress',
		     work_dir=?,
		     worktree_path=?,
		     session_provider='codex',
		     harness='codex',
		     permission_mode='bypass',
		     model='gpt-5.4-mini',
		     session_id='session-123',
		     session_path=?
		 WHERE slug='worker'`,
		root,
		worktreePath,
		missingTranscript,
	); err != nil {
		t.Fatalf("seed task detail: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/brain/graph/node/"+url.PathEscape("task:worker"), nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got BrainGraphNodeDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode detail: %v", err)
	}

	if got.ID != "task:worker" || got.Type != "task" || got.Task == nil {
		t.Fatalf("detail identity = %#v, want task detail", got)
	}
	if got.Task.Slug != "worker" || got.Task.Name != "Worker" || got.Task.Status != "in-progress" {
		t.Fatalf("task detail = %#v, want worker task metadata", got.Task)
	}
	if got.Task.ProjectSlug == nil || *got.Task.ProjectSlug != "flow-manager" {
		t.Fatalf("project_slug = %#v, want flow-manager", got.Task.ProjectSlug)
	}
	if got.Task.ParentSlug == nil || *got.Task.ParentSlug != "parent" {
		t.Fatalf("parent_slug = %#v, want parent", got.Task.ParentSlug)
	}
	if got.Task.WorkDir != root || got.Task.WorktreePath == nil || *got.Task.WorktreePath != worktreePath {
		t.Fatalf("work paths = work_dir=%q worktree=%#v", got.Task.WorkDir, got.Task.WorktreePath)
	}
	if got.Task.SessionProvider != "codex" || got.Task.Harness != "codex" || got.Task.PermissionMode != "bypass" {
		t.Fatalf("session metadata = %#v, want codex/bypass", got.Task)
	}
	if got.Task.Model == nil || *got.Task.Model != "gpt-5.4-mini" {
		t.Fatalf("model = %#v, want gpt-5.4-mini", got.Task.Model)
	}
	if got.Task.SessionID == nil || *got.Task.SessionID != "session-123" {
		t.Fatalf("session_id = %#v, want session-123", got.Task.SessionID)
	}
	if got.Task.Transcript == nil || got.Task.Transcript.Kind != "transcript" || got.Task.Transcript.RefID != "session-123" {
		t.Fatalf("transcript ref = %#v, want unavailable transcript ref", got.Task.Transcript)
	}
	if got.Task.Transcript.Available {
		t.Fatalf("transcript should be unavailable when stored file is missing: %#v", got.Task.Transcript)
	}
	if got.Task.BriefPath != briefPath {
		t.Fatalf("brief_path = %q, want %q", got.Task.BriefPath, briefPath)
	}
	if len(got.Task.Updates) != 1 || got.Task.Updates[0].Filename != "2026-06-12-progress.md" {
		t.Fatalf("updates = %#v, want recent update ref", got.Task.Updates)
	}
}

func TestBrainGraphExpandedTaskAddsEvidenceReferences(t *testing.T) {
	root, db := testRootDB(t)
	insertBrainGraphTask(t, db, "worker", "Worker", "backlog", nil)
	if _, err := db.Exec(`UPDATE tasks SET session_id='session-123' WHERE slug='worker'`); err != nil {
		t.Fatalf("seed session id: %v", err)
	}
	for _, tag := range []string{"gh-pr:Facets-cloud/flow-manager#33", "gh-issue:Facets-cloud/flow-manager#123"} {
		if err := flowdb.AddTaskTag(db, "worker", tag); err != nil {
			t.Fatalf("AddTaskTag(%s): %v", tag, err)
		}
	}

	got, err := BuildBrainGraph(db, root, BrainGraphFilters{Expand: map[string]bool{"task:worker": true}}, graphTestTime())
	if err != nil {
		t.Fatalf("BuildBrainGraph: %v", err)
	}

	transcript, ok := graphNodeByID(got, "transcript:worker")
	if !ok {
		t.Fatalf("missing transcript node: %#v", got.Nodes)
	}
	if transcript.Status != "available" || transcript.Ref == nil || transcript.Ref.Kind != "transcript" || transcript.Ref.ID != "session-123" {
		t.Fatalf("transcript node = %#v, want available transcript ref", transcript)
	}
	if !graphHasEdge(got, "external_ref", "task:worker", "transcript:worker") {
		t.Fatalf("missing transcript external_ref edge: %#v", got.Edges)
	}
	for _, tag := range []string{"gh-pr:Facets-cloud/flow-manager#33", "gh-issue:Facets-cloud/flow-manager#123"} {
		tag = productdb.NormalizeTag(tag)
		nodeID := brainGraphGitHubRefNodeID(tag)
		node, ok := graphNodeByID(got, nodeID)
		if !ok {
			t.Fatalf("missing github reference node %s: %#v", nodeID, got.Nodes)
		}
		if node.Status != "linked" || node.Ref == nil || node.Ref.Kind != "github" || node.Ref.ID != tag {
			t.Fatalf("github reference node = %#v, want linked github ref for %s", node, tag)
		}
		if !graphHasEdge(got, "external_ref", "task:worker", nodeID) {
			t.Fatalf("missing github external_ref edge to %s: %#v", nodeID, got.Edges)
		}
	}
}

func TestBrainGraphGitHubEvidenceDetailPreservesGraphNodeID(t *testing.T) {
	root, db := testRootDB(t)
	s := New(Config{DB: db, FlowRoot: root})
	insertBrainGraphTask(t, db, "ship", "Ship Feature", "backlog", nil)
	if err := flowdb.AddTaskTag(db, "ship", "gh-pr:Facets-cloud/flow-manager#44"); err != nil {
		t.Fatalf("AddTaskTag: %v", err)
	}
	view, err := BuildBrainGraph(db, root, BrainGraphFilters{Expand: map[string]bool{"task:ship": true}}, graphTestTime())
	if err != nil {
		t.Fatalf("BuildBrainGraph: %v", err)
	}
	var githubNode BrainGraphNode
	for _, node := range view.Nodes {
		if node.Type == "github_ref" {
			githubNode = node
			break
		}
	}
	if githubNode.ID == "" {
		t.Fatalf("graph nodes = %#v, want github evidence node", view.Nodes)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/brain/graph/node/"+url.PathEscape(githubNode.ID), nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got BrainGraphNodeDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode detail: %v", err)
	}

	if got.ID != githubNode.ID {
		t.Fatalf("detail id = %q, want selected graph node id %q", got.ID, githubNode.ID)
	}
	if got.Evidence == nil || got.Evidence.URL == nil || *got.Evidence.URL != "https://github.com/facets-cloud/flow-manager/pull/44" {
		t.Fatalf("github evidence = %#v, want pull request URL", got.Evidence)
	}
}

func TestBrainGraphGroupsTasksByOwnerTagAndInheritance(t *testing.T) {
	root, db := testRootDB(t)
	insertBrainGraphOwner(t, db, root, "brain-ui")
	insertBrainGraphTask(t, db, "parent", "Parent", "backlog", nil)
	insertBrainGraphTask(t, db, "child", "Child", "backlog", strPtr("parent"))
	insertBrainGraphTask(t, db, "other", "Other", "backlog", nil)
	if err := flowdb.AddTaskTag(db, "parent", "owner:brain-ui"); err != nil {
		t.Fatalf("AddTaskTag: %v", err)
	}

	got, err := BuildBrainGraph(db, root, BrainGraphFilters{}, graphTestTime())
	if err != nil {
		t.Fatalf("BuildBrainGraph: %v", err)
	}

	nodes := graphNodesByTask(got)
	if nodes["parent"].OwnerSlug != "brain-ui" {
		t.Fatalf("parent owner_slug = %q, want brain-ui", nodes["parent"].OwnerSlug)
	}
	if nodes["child"].OwnerSlug != "brain-ui" {
		t.Fatalf("child owner_slug = %q, want inherited brain-ui", nodes["child"].OwnerSlug)
	}
	if nodes["other"].OwnerSlug != "unowned" {
		t.Fatalf("other owner_slug = %q, want unowned", nodes["other"].OwnerSlug)
	}
	ownerCounts := map[string]int{}
	for _, owner := range got.Owners {
		ownerCounts[owner.Slug] = owner.TaskCount
	}
	if ownerCounts["brain-ui"] != 2 {
		t.Fatalf("brain-ui task count = %d, want 2", ownerCounts["brain-ui"])
	}
	if ownerCounts["unowned"] != 1 {
		t.Fatalf("unowned task count = %d, want 1", ownerCounts["unowned"])
	}
}

func TestBrainGraphInheritsOwnerFromParentHiddenByVisibilityFilters(t *testing.T) {
	root, db := testRootDB(t)
	insertBrainGraphOwner(t, db, root, "brain-ui")
	insertBrainGraphTask(t, db, "query-parent", "Query Parent", "backlog", nil)
	insertBrainGraphTask(t, db, "query-child", "Needle Child", "backlog", strPtr("query-parent"))
	insertBrainGraphTask(t, db, "status-parent", "Status Parent", "done", nil)
	insertBrainGraphTask(t, db, "status-child", "Status Child", "backlog", strPtr("status-parent"))
	insertBrainGraphTask(t, db, "done-parent", "Done Parent", "done", nil)
	insertBrainGraphTask(t, db, "done-child", "Done Child", "backlog", strPtr("done-parent"))
	for _, slug := range []string{"query-parent", "status-parent", "done-parent"} {
		if err := flowdb.AddTaskTag(db, slug, "owner:brain-ui"); err != nil {
			t.Fatalf("AddTaskTag(%s): %v", slug, err)
		}
	}

	tests := []struct {
		name      string
		filters   BrainGraphFilters
		childSlug string
	}{
		{name: "query", filters: BrainGraphFilters{Query: "needle"}, childSlug: "query-child"},
		{name: "status", filters: BrainGraphFilters{Status: "backlog", IncludeDone: true}, childSlug: "status-child"},
		{name: "include_done", filters: BrainGraphFilters{}, childSlug: "done-child"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildBrainGraph(db, root, tt.filters, graphTestTime())
			if err != nil {
				t.Fatalf("BuildBrainGraph: %v", err)
			}
			nodes := graphNodesByTask(got)
			node, ok := nodes[tt.childSlug]
			if !ok {
				t.Fatalf("child %s missing from graph nodes: %#v", tt.childSlug, got.Nodes)
			}
			if node.OwnerSlug != "brain-ui" {
				t.Fatalf("%s owner_slug = %q, want inherited brain-ui", tt.childSlug, node.OwnerSlug)
			}
		})
	}
}

func TestBrainGraphWarnsForUnknownOwnerTagsEvenWhenValidOwnerSelected(t *testing.T) {
	root, db := testRootDB(t)
	insertBrainGraphOwner(t, db, root, "brain-ui")
	insertBrainGraphTask(t, db, "mixed-owner", "Mixed Owner", "backlog", nil)
	for _, tag := range []string{"owner:brain-ui", "owner:missing-b", "owner:missing-a"} {
		if err := flowdb.AddTaskTag(db, "mixed-owner", tag); err != nil {
			t.Fatalf("AddTaskTag(%s): %v", tag, err)
		}
	}

	got, err := BuildBrainGraph(db, root, BrainGraphFilters{}, graphTestTime())
	if err != nil {
		t.Fatalf("BuildBrainGraph: %v", err)
	}

	nodes := graphNodesByTask(got)
	if nodes["mixed-owner"].OwnerSlug != "brain-ui" {
		t.Fatalf("mixed-owner owner_slug = %q, want brain-ui", nodes["mixed-owner"].OwnerSlug)
	}
	if !graphHasWarning(got, "unknown_owner", "task:mixed-owner", "owner:missing-a") {
		t.Fatalf("missing warning for owner:missing-a: %#v", got.Warnings)
	}
	if !graphHasWarning(got, "unknown_owner", "task:mixed-owner", "owner:missing-b") {
		t.Fatalf("missing warning for owner:missing-b: %#v", got.Warnings)
	}
}

func TestBrainGraphAddsParentAndDependencyEdges(t *testing.T) {
	root, db := testRootDB(t)
	insertBrainGraphTask(t, db, "parent", "Parent", "backlog", nil)
	insertBrainGraphTask(t, db, "child", "Child", "backlog", strPtr("parent"))
	insertBrainGraphTask(t, db, "dep", "Dependency", "done", nil)
	if _, err := db.Exec(
		`INSERT INTO task_dependencies (child_slug, parent_slug, created_at)
		 VALUES ('child', 'dep', ?)`,
		"2026-06-12T10:00:00+05:30",
	); err != nil {
		t.Fatal(err)
	}

	withoutDone, err := BuildBrainGraph(db, root, BrainGraphFilters{}, graphTestTime())
	if err != nil {
		t.Fatalf("BuildBrainGraph without done: %v", err)
	}
	if graphHasEdge(withoutDone, "depends_on", "task:dep", "task:child") {
		t.Fatalf("depends_on edge should be hidden when done dependency node is excluded: %#v", withoutDone.Edges)
	}

	got, err := BuildBrainGraph(db, root, BrainGraphFilters{IncludeDone: true}, graphTestTime())
	if err != nil {
		t.Fatalf("BuildBrainGraph: %v", err)
	}

	if !graphHasEdge(got, "parent", "task:parent", "task:child") {
		t.Fatalf("missing parent edge task:parent -> task:child: %#v", got.Edges)
	}
	if !graphHasEdge(got, "depends_on", "task:dep", "task:child") {
		t.Fatalf("missing depends_on edge task:dep -> task:child: %#v", got.Edges)
	}
}

func TestBrainGraphTaskNodesOnlyAdvertiseSupportedActions(t *testing.T) {
	root, db := testRootDB(t)
	insertBrainGraphTask(t, db, "ship", "Ship Feature", "backlog", nil)

	view, err := BuildBrainGraph(db, root, BrainGraphFilters{}, graphTestTime())
	if err != nil {
		t.Fatalf("BuildBrainGraph: %v", err)
	}
	node, ok := graphNodeByID(view, "task:ship")
	if !ok {
		t.Fatalf("missing task node: %#v", view.Nodes)
	}
	if strings.Join(node.Actions, ",") != "open_session,send_event,seed" {
		t.Fatalf("task actions = %#v, want session controls", node.Actions)
	}
	want := map[string]bool{"open_session": true, "send_event": true, "seed": true}
	got := map[string]bool{}
	for _, action := range view.SelectedActions {
		got[action.Key] = true
		if !action.Enabled || action.Risky || action.DisabledReason != "" {
			t.Fatalf("action spec %s = %#v, want enabled safe control", action.Key, action)
		}
	}
	for key := range want {
		if !got[key] {
			t.Fatalf("selected actions = %#v, want %s", view.SelectedActions, key)
		}
	}
	if len(view.SelectedActions) != len(want) {
		t.Fatalf("selected actions = %#v, want exactly session controls", view.SelectedActions)
	}
}

func assertBrainGraphEmptyResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got BrainGraphView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode graph: %v", err)
	}
	if got.Controller.Mode != "global_brain" {
		t.Fatalf("controller mode = %q, want global_brain", got.Controller.Mode)
	}
	if got.Counts.TotalTasks != 0 {
		t.Fatalf("total tasks = %d, want 0", got.Counts.TotalTasks)
	}
	if len(got.Owners) != 1 || got.Owners[0].Slug != "unowned" {
		t.Fatalf("owners = %#v, want only unowned boundary", got.Owners)
	}
}

func strPtr(s string) *string {
	return &s
}

func graphNodesByTask(view BrainGraphView) map[string]BrainGraphNode {
	out := map[string]BrainGraphNode{}
	for _, node := range view.Nodes {
		if node.TaskSlug != "" {
			out[node.TaskSlug] = node
		}
	}
	return out
}

func graphNodeByID(view BrainGraphView, id string) (BrainGraphNode, bool) {
	for _, node := range view.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return BrainGraphNode{}, false
}

func graphHasEdge(view BrainGraphView, edgeType, source, target string) bool {
	for _, edge := range view.Edges {
		if edge.Type == edgeType && edge.Source == source && edge.Target == target {
			return true
		}
	}
	return false
}

func graphHasWarning(view BrainGraphView, code, nodeID, messagePart string) bool {
	for _, warning := range view.Warnings {
		if warning.Code == code && warning.NodeID == nodeID && strings.Contains(warning.Message, messagePart) {
			return true
		}
	}
	return false
}

func insertBrainGraphOwner(t *testing.T, db *sql.DB, root, slug string) {
	t.Helper()
	now := "2026-06-12T10:00:00+05:30"
	if _, err := db.Exec(
		`INSERT INTO owners (slug, name, work_dir, status, every, harness, created_at, updated_at)
		 VALUES (?, ?, ?, 'active', '1h', 'claude', ?, ?)`,
		slug, slug, root, now, now,
	); err != nil {
		t.Fatal(err)
	}
}

func insertBrainGraphTask(t *testing.T, db *sql.DB, slug, name, status string, parentSlug *string) {
	t.Helper()
	now := "2026-06-12T10:00:00+05:30"
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, kind, parent_slug, priority, work_dir, created_at, updated_at)
		 VALUES (?, ?, ?, 'regular', ?, 'medium', ?, ?, ?)`,
		slug, name, status, parentSlug, t.TempDir(), now, now,
	); err != nil {
		t.Fatal(err)
	}
}
