package server

import (
	"database/sql"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"flow/internal/productdb"
)

func parseBrainGraphFilters(r *http.Request) BrainGraphFilters {
	q := r.URL.Query()
	expand := map[string]bool{}
	for _, raw := range strings.Split(q.Get("expand"), ",") {
		raw = strings.TrimSpace(raw)
		if raw != "" {
			expand[raw] = true
		}
	}
	return BrainGraphFilters{
		Project:     strings.TrimSpace(q.Get("project")),
		Owner:       strings.TrimSpace(q.Get("owner")),
		Status:      strings.TrimSpace(q.Get("status")),
		IncludeDone: q.Get("include_done") == "1" || q.Get("include_done") == "true",
		Expand:      expand,
		Query:       strings.TrimSpace(q.Get("q")),
	}
}

func BuildBrainGraph(db *sql.DB, root string, filters BrainGraphFilters, now time.Time) (BrainGraphView, error) {
	view := BrainGraphView{
		GeneratedAt: now.Format(time.RFC3339),
		Freshness:   "fresh",
		Controller: BrainGraphController{
			Mode:        "global_brain",
			DisplayName: "Global Brain",
			Status:      "ready",
		},
		Owners: []BrainGraphOwnerView{{
			ID:     "owner:unowned",
			Slug:   "unowned",
			Name:   "Unowned",
			Status: "active",
		}},
		Nodes:           []BrainGraphNode{},
		Edges:           []BrainGraphEdge{},
		SelectedActions: defaultBrainGraphActions(),
		Warnings:        []BrainGraphWarning{},
	}
	allTasks, err := productdb.ListTasks(db, productdb.TaskFilter{Kind: ""})
	if err != nil {
		return view, err
	}
	visibleTasks := filterBrainGraphTasks(allTasks, filters)
	slugs := taskSlugs(allTasks)
	tagsByTask, err := productdb.GetTaskTagsBatch(db, slugs)
	if err != nil {
		return view, err
	}
	owners, err := productdb.ListOwners(db, productdb.OwnerFilter{})
	if err != nil {
		return view, err
	}
	ownerBySlug, taskOwners, warnings := resolveBrainGraphOwners(allTasks, owners, tagsByTask)
	appendOwnerBoundaries(&view, owners)

	visible := make(map[string]bool, len(visibleTasks))
	for _, task := range visibleTasks {
		ownerSlug := taskOwners[task.Slug]
		if ownerSlug == "" {
			ownerSlug = "unowned"
		}
		if filters.Owner != "" && filters.Owner != ownerSlug {
			continue
		}
		node := brainGraphTaskNode(task, ownerSlug, tagsByTask[task.Slug], filters)
		view.Nodes = append(view.Nodes, node)
		visible[task.Slug] = true
		view.Counts.TotalTasks++
		if task.Status == "done" {
			view.Counts.Done++
		}
		if task.Status == "in-progress" || nullStringValue(task.AutoRunStatus) == "running" {
			view.Counts.Running++
		}
	}
	view.Warnings = append(view.Warnings, visibleBrainGraphWarnings(warnings, visible)...)
	for _, node := range view.Nodes {
		ownerSlug := node.OwnerSlug
		if ownerSlug == "" {
			ownerSlug = "unowned"
		}
		for i := range view.Owners {
			if view.Owners[i].Slug != ownerSlug {
				continue
			}
			view.Owners[i].TaskCount++
			if node.Status == "in-progress" {
				view.Owners[i].RunningCount++
			}
			break
		}
		if _, ok := ownerBySlug[ownerSlug]; !ok && ownerSlug != "unowned" {
			view.Warnings = append(view.Warnings, BrainGraphWarning{
				Code:    "missing_owner_boundary",
				Message: "task is assigned to an owner boundary that is not present: " + ownerSlug,
				NodeID:  node.ID,
			})
		}
	}
	appendBrainGraphEvidenceNodes(&view, visibleTasks, visible, tagsByTask, filters)
	for _, task := range visibleTasks {
		if !visible[task.Slug] || !task.ParentSlug.Valid || strings.TrimSpace(task.ParentSlug.String) == "" {
			continue
		}
		parentSlug := strings.TrimSpace(task.ParentSlug.String)
		if !visible[parentSlug] {
			continue
		}
		view.Edges = append(view.Edges, BrainGraphEdge{
			ID:     "parent:" + parentSlug + ":" + task.Slug,
			Type:   "parent",
			Source: "task:" + parentSlug,
			Target: "task:" + task.Slug,
		})
	}
	deps, err := listBrainGraphDependencies(db)
	if err != nil {
		return view, err
	}
	for _, dep := range deps {
		if !visible[dep.parentSlug] || !visible[dep.childSlug] {
			continue
		}
		view.Edges = append(view.Edges, BrainGraphEdge{
			ID:     "depends_on:" + dep.parentSlug + ":" + dep.childSlug,
			Type:   "depends_on",
			Source: "task:" + dep.parentSlug,
			Target: "task:" + dep.childSlug,
		})
	}
	view.Counts.Owners = len(view.Owners)
	view.Counts.Warnings = len(view.Warnings)
	return view, nil
}

func visibleBrainGraphWarnings(warnings []BrainGraphWarning, visible map[string]bool) []BrainGraphWarning {
	out := make([]BrainGraphWarning, 0, len(warnings))
	for _, warning := range warnings {
		if warning.NodeID == "" {
			out = append(out, warning)
			continue
		}
		taskSlug, ok := strings.CutPrefix(warning.NodeID, "task:")
		if ok && visible[taskSlug] {
			out = append(out, warning)
		}
	}
	return out
}

func filterBrainGraphTasks(tasks []*productdb.Task, filters BrainGraphFilters) []*productdb.Task {
	query := strings.ToLower(strings.TrimSpace(filters.Query))
	out := make([]*productdb.Task, 0, len(tasks))
	for _, task := range tasks {
		if filters.Project != "" && nullStringValue(task.ProjectSlug) != filters.Project {
			continue
		}
		if filters.Status != "" && task.Status != filters.Status {
			continue
		}
		if !filters.IncludeDone && task.Status == "done" {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(task.Slug), query) && !strings.Contains(strings.ToLower(task.Name), query) {
			continue
		}
		out = append(out, task)
	}
	return out
}

func taskSlugs(tasks []*productdb.Task) []string {
	slugs := make([]string, 0, len(tasks))
	for _, task := range tasks {
		slugs = append(slugs, task.Slug)
	}
	return slugs
}

func resolveBrainGraphOwners(tasks []*productdb.Task, owners []*productdb.Owner, tagsByTask map[string][]string) (map[string]*productdb.Owner, map[string]string, []BrainGraphWarning) {
	ownerBySlug := make(map[string]*productdb.Owner, len(owners))
	for _, owner := range owners {
		ownerBySlug[owner.Slug] = owner
	}
	taskBySlug := make(map[string]*productdb.Task, len(tasks))
	for _, task := range tasks {
		taskBySlug[task.Slug] = task
	}
	resolved := make(map[string]string, len(tasks))
	resolving := map[string]bool{}
	warnedUnknown := map[string]bool{}
	var warnings []BrainGraphWarning

	var resolve func(*productdb.Task) string
	resolve = func(task *productdb.Task) string {
		if owner, ok := resolved[task.Slug]; ok {
			return owner
		}
		if resolving[task.Slug] {
			resolved[task.Slug] = "unowned"
			return "unowned"
		}
		resolving[task.Slug] = true
		defer delete(resolving, task.Slug)

		ownerTags := brainGraphOwnerTags(tagsByTask[task.Slug])
		if len(ownerTags) > 0 {
			validOwner := ""
			for _, ownerSlug := range ownerTags {
				if _, ok := ownerBySlug[ownerSlug]; ok {
					if validOwner == "" {
						validOwner = ownerSlug
					}
					continue
				}
				warningKey := task.Slug + "\x00" + ownerSlug
				if warnedUnknown[warningKey] {
					continue
				}
				warnings = append(warnings, BrainGraphWarning{
					Code:    "unknown_owner",
					Message: "task has owner tag with no matching owner: owner:" + ownerSlug,
					NodeID:  "task:" + task.Slug,
				})
				warnedUnknown[warningKey] = true
			}
			if validOwner != "" {
				resolved[task.Slug] = validOwner
				return validOwner
			}
			resolved[task.Slug] = "unowned"
			return "unowned"
		}
		if task.ParentSlug.Valid {
			parentSlug := strings.TrimSpace(task.ParentSlug.String)
			if parentSlug != "" {
				if parent, ok := taskBySlug[parentSlug]; ok {
					owner := resolve(parent)
					resolved[task.Slug] = owner
					return owner
				}
			}
		}
		resolved[task.Slug] = "unowned"
		return "unowned"
	}
	for _, task := range tasks {
		resolve(task)
	}
	return ownerBySlug, resolved, warnings
}

func brainGraphOwnerTags(tags []string) []string {
	var owners []string
	for _, tag := range tags {
		if owner, ok := strings.CutPrefix(tag, "owner:"); ok {
			owner = strings.TrimSpace(owner)
			if owner != "" {
				owners = append(owners, owner)
			}
		}
	}
	sort.Strings(owners)
	return owners
}

func appendOwnerBoundaries(view *BrainGraphView, owners []*productdb.Owner) {
	for _, owner := range owners {
		view.Owners = append(view.Owners, BrainGraphOwnerView{
			ID:     "owner:" + owner.Slug,
			Slug:   owner.Slug,
			Name:   owner.Name,
			Status: owner.Status,
		})
	}
}

func brainGraphTaskNode(task *productdb.Task, ownerSlug string, tags []string, filters BrainGraphFilters) BrainGraphNode {
	nodeID := "task:" + task.Slug
	return BrainGraphNode{
		ID:             nodeID,
		Type:           "task",
		OwnerSlug:      ownerSlug,
		TaskSlug:       task.Slug,
		ParentTaskSlug: nullStringValue(task.ParentSlug),
		Label:          task.Name,
		Status:         task.Status,
		Priority:       task.Priority,
		Provider:       task.SessionProvider,
		Harness:        task.Harness,
		PermissionMode: task.PermissionMode,
		Model:          nullStringValue(task.Model),
		Summary:        brainGraphTaskSummary(task),
		Expanded:       filters.Expand[nodeID] || filters.Expand[task.Slug],
		Ref: &BrainGraphRef{
			Kind: "task",
			ID:   task.Slug,
		},
		Badges:  append([]string(nil), tags...),
		Actions: []string{"open_session", "send_event", "seed"},
		Metadata: map[string]string{
			"kind": task.Kind,
		},
	}
}

func appendBrainGraphEvidenceNodes(view *BrainGraphView, tasks []*productdb.Task, visible map[string]bool, tagsByTask map[string][]string, filters BrainGraphFilters) {
	emittedRefs := map[string]bool{}
	emittedEdges := map[string]bool{}
	for _, task := range tasks {
		if !visible[task.Slug] || !brainGraphTaskExpanded(task, filters) {
			continue
		}
		if sessionID := strings.TrimSpace(task.SessionID.String); task.SessionID.Valid && sessionID != "" {
			nodeID := "transcript:" + task.Slug
			if !emittedRefs[nodeID] {
				view.Nodes = append(view.Nodes, BrainGraphNode{
					ID:       nodeID,
					Type:     "transcript_ref",
					TaskSlug: task.Slug,
					Label:    "Transcript: " + task.Slug,
					Status:   "available",
					Ref: &BrainGraphRef{
						Kind: "transcript",
						ID:   sessionID,
					},
					Metadata: map[string]string{
						"task_slug":  task.Slug,
						"session_id": sessionID,
					},
				})
				emittedRefs[nodeID] = true
			}
			appendBrainGraphExternalRefEdge(view, emittedEdges, task.Slug, nodeID)
		}
		for _, tag := range tagsByTask[task.Slug] {
			if !brainGraphGitHubTag(tag) {
				continue
			}
			nodeID := brainGraphGitHubRefNodeID(tag)
			if !emittedRefs[nodeID] {
				view.Nodes = append(view.Nodes, BrainGraphNode{
					ID:       nodeID,
					Type:     "github_ref",
					TaskSlug: task.Slug,
					Label:    tag,
					Status:   "linked",
					Ref: &BrainGraphRef{
						Kind: "github",
						ID:   tag,
						URL:  brainGraphGitHubRefURL(tag),
					},
					Metadata: map[string]string{
						"task_slug": task.Slug,
						"tag":       tag,
					},
				})
				emittedRefs[nodeID] = true
			}
			appendBrainGraphExternalRefEdge(view, emittedEdges, task.Slug, nodeID)
		}
	}
}

func brainGraphTaskExpanded(task *productdb.Task, filters BrainGraphFilters) bool {
	if task == nil {
		return false
	}
	nodeID := "task:" + task.Slug
	return filters.Expand[nodeID] || filters.Expand[task.Slug]
}

func brainGraphGitHubTag(tag string) bool {
	tag = strings.TrimSpace(tag)
	return strings.HasPrefix(tag, "gh-pr:") || strings.HasPrefix(tag, "gh-issue:")
}

func brainGraphGitHubRefNodeID(tag string) string {
	return "github:" + url.PathEscape(productdb.NormalizeTag(tag))
}

func brainGraphGitHubRefURL(tag string) string {
	tag = productdb.NormalizeTag(tag)
	kind, rest, ok := strings.Cut(tag, ":")
	if !ok {
		return ""
	}
	repo, number, ok := strings.Cut(rest, "#")
	if !ok || strings.TrimSpace(repo) == "" || strings.TrimSpace(number) == "" {
		return ""
	}
	switch kind {
	case "gh-pr":
		return "https://github.com/" + repo + "/pull/" + number
	case "gh-issue":
		return "https://github.com/" + repo + "/issues/" + number
	default:
		return ""
	}
}

func appendBrainGraphExternalRefEdge(view *BrainGraphView, emitted map[string]bool, taskSlug, target string) {
	edgeID := "external_ref:" + taskSlug + ":" + target
	if emitted[edgeID] {
		return
	}
	view.Edges = append(view.Edges, BrainGraphEdge{
		ID:     edgeID,
		Type:   "external_ref",
		Source: "task:" + taskSlug,
		Target: target,
	})
	emitted[edgeID] = true
}

func brainGraphTaskSummary(task *productdb.Task) string {
	var parts []string
	if task.ProjectSlug.Valid && strings.TrimSpace(task.ProjectSlug.String) != "" {
		parts = append(parts, "project:"+strings.TrimSpace(task.ProjectSlug.String))
	}
	if task.WaitingOn.Valid && strings.TrimSpace(task.WaitingOn.String) != "" {
		parts = append(parts, "waiting:"+strings.TrimSpace(task.WaitingOn.String))
	}
	if task.AutoRunStatus.Valid && strings.TrimSpace(task.AutoRunStatus.String) != "" {
		parts = append(parts, "auto:"+strings.TrimSpace(task.AutoRunStatus.String))
	}
	return strings.Join(parts, " ")
}

type brainGraphDependency struct {
	childSlug  string
	parentSlug string
}

func listBrainGraphDependencies(db *sql.DB) ([]brainGraphDependency, error) {
	rows, err := db.Query(`
		SELECT child_slug, parent_slug
		FROM task_dependencies
		ORDER BY parent_slug, child_slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []brainGraphDependency
	for rows.Next() {
		var dep brainGraphDependency
		if err := rows.Scan(&dep.childSlug, &dep.parentSlug); err != nil {
			return nil, err
		}
		out = append(out, dep)
	}
	return out, rows.Err()
}

func defaultBrainGraphActions() []BrainGraphActionSpec {
	return []BrainGraphActionSpec{
		{Key: "open_session", Label: "Open session", Enabled: true},
		{Key: "send_event", Label: "Send event", Enabled: true},
		{Key: "seed", Label: "Seed input", Enabled: true},
	}
}

func (s *Server) handleBrainGraph(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	view, err := BuildBrainGraph(s.cfg.DB, s.cfg.FlowRoot, parseBrainGraphFilters(r), time.Now())
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, view)
}
