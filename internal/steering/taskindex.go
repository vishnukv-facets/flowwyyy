// internal/steering/taskindex.go
package steering

import (
	"database/sql"
	"fmt"
	"strings"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// BuildTaskIndex renders a text index of the operator's ACTIVE tasks and
// projects for the Stage 2/3 prompts to suggest a matched_task /
// suggested_project. Done and deleted rows are excluded; ARCHIVED tasks are
// INCLUDED (archiving declutters the active list, it does not stop tracking —
// see the routing-includes-archived convention) so the deep-triage agent can
// still match a message to an archived-but-open task.
//
// Each task lists the filesystem paths to its brief and updates so the deep
// triage agent (a real claude -p with file tools) can READ the actual task
// context — brief + progress notes — instead of guessing from the name. Format:
//
//	Projects:
//	- goniyo: Goniyo
//	Tasks:
//	- kong-split [goniyo] (in-progress): Kong split
//	    brief: /…/tasks/kong-split/brief.md  updates: /…/tasks/kong-split/updates/
func BuildTaskIndex(db *sql.DB) (string, error) {
	projects, err := flowdb.ListProjects(db, flowdb.ProjectFilter{IncludeArchived: false})
	if err != nil {
		return "", fmt.Errorf("steering: list projects: %w", err)
	}
	tasks, err := flowdb.ListTasks(db, flowdb.TaskFilter{IncludeArchived: true})
	if err != nil {
		return "", fmt.Errorf("steering: list tasks: %w", err)
	}

	var b strings.Builder
	b.WriteString("Projects:\n")
	pCount := 0
	for _, p := range projects {
		if p.DeletedAt.Valid || p.ArchivedAt.Valid || p.Status == "done" {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", p.Slug, p.Name)
		pCount++
	}
	if pCount == 0 {
		b.WriteString("(none)\n")
	}

	b.WriteString("Tasks:\n")
	tCount := 0
	for _, tk := range tasks {
		if tk.DeletedAt.Valid || tk.Status == "done" {
			continue
		}
		project := ""
		if tk.ProjectSlug.Valid && tk.ProjectSlug.String != "" {
			project = " [" + tk.ProjectSlug.String + "]"
		}
		status := tk.Status
		if tk.ArchivedAt.Valid {
			status += ", archived"
		}
		fmt.Fprintf(&b, "- %s%s (%s): %s\n", tk.Slug, project, status, tk.Name)
		if dir := monitor.TaskDir(tk.Slug); dir != "" {
			fmt.Fprintf(&b, "    brief: %s/brief.md  updates: %s/updates/\n", dir, dir)
		}
		tCount++
	}
	if tCount == 0 {
		b.WriteString("(none)\n")
	}
	return b.String(), nil
}
