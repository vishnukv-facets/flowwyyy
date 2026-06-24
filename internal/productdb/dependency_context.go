package productdb

import (
	"database/sql"
	"fmt"
	"strings"
)

// DependencyRef is an upstream task another task depends on, carrying just
// enough integration state to brief a freshly-spawned session about work that
// may not be merged into its base branch yet.
type DependencyRef struct {
	Slug         string
	Name         string
	Status       string
	PRRef        string // "owner/repo#n" from a gh-pr: tag, or "" if none linked
	WorktreePath string
}

// LoadDependencyRefs returns childSlug's task_dependencies parents with their
// linked PR (parsed from the gh-pr: tag) and worktree path. Deleted parents are
// excluded; order is dependency-creation order (stable).
func LoadDependencyRefs(db *sql.DB, childSlug string) ([]DependencyRef, error) {
	rows, err := db.Query(`
		SELECT t.slug, t.name, t.status, COALESCE(t.worktree_path, '')
		FROM task_dependencies d JOIN tasks t ON t.slug = d.parent_slug
		WHERE d.child_slug = ? AND t.deleted_at IS NULL
		ORDER BY d.created_at ASC, t.slug ASC`, childSlug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DependencyRef
	for rows.Next() {
		var r DependencyRef
		if err := rows.Scan(&r.Slug, &r.Name, &r.Status, &r.WorktreePath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		tags, err := GetTaskTags(db, out[i].Slug)
		if err != nil {
			continue
		}
		for _, tag := range tags {
			if ref, ok := strings.CutPrefix(tag, "gh-pr:"); ok {
				out[i].PRRef = ref
				break
			}
		}
	}
	return out, nil
}

// DependencyBootstrapNote renders an upstream-context briefing for childSlug's
// dependencies, or "" when it has none. A spawned session reads this in its
// bootstrap prompt to learn which upstream tasks it builds on and whether their
// work actually landed in a PR — the gap that otherwise leaves a dependent task
// without the upstream changes (they may be unmerged on a sibling branch).
func DependencyBootstrapNote(db *sql.DB, childSlug string) string {
	refs, err := LoadDependencyRefs(db, childSlug)
	if err != nil || len(refs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Upstream dependencies — their changes may NOT be in your base branch yet. Review each before building on it:\n")
	for _, r := range refs {
		fmt.Fprintf(&b, "  - %s (%s) — %s", r.Name, r.Slug, r.Status)
		switch {
		case r.PRRef != "":
			b.WriteString("; PR " + r.PRRef)
		case r.Status == "done":
			b.WriteString("; NO PR — its commits are not in any PR (likely unmerged on its branch")
			if r.WorktreePath != "" {
				b.WriteString("/worktree " + r.WorktreePath)
			}
			b.WriteString("), so inspect its diff before relying on it")
		default:
			b.WriteString("; not finished yet")
		}
		b.WriteByte('\n')
	}
	b.WriteString("Run `flow show task <slug>` on any of these to read its brief, updates, and git close-out.")
	return b.String()
}
