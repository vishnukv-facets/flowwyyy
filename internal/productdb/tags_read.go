package productdb

// tags_read.go is productdb's flowdb-free READ over task_tags aggregate views
// (Bucket O — official flow owns task_tags and the `flow update task --tag`
// verb). Per-task tag reads live in read.go (GetTaskTags/GetTaskTagsBatch);
// tag WRITES go through `flow` exec, never here.

import (
	"database/sql"
	"fmt"
)

// TagCount is the (tag, task-count) pair returned by ListAllTags (twin of
// flowdb.TagCount).
type TagCount struct {
	Tag   string
	Count int
}

// ListAllTags returns every distinct tag in use, with the number of
// non-archived, non-deleted tasks that carry it. Sorted by count descending,
// then tag ascending — most-used tags first. Twin of flowdb.ListAllTags.
func ListAllTags(db *sql.DB) ([]TagCount, error) {
	rows, err := db.Query(`
		SELECT t.tag, COUNT(*) AS n
		FROM task_tags t
		JOIN tasks tk ON tk.slug = t.task_slug
		WHERE tk.archived_at IS NULL AND tk.deleted_at IS NULL
		GROUP BY t.tag
		ORDER BY n DESC, t.tag ASC`)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()
	var out []TagCount
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}
