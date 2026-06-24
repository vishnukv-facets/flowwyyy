package productdb

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var wikiLinkRe = regexp.MustCompile(`\[\[([^\]\n]+)\]\]`)

type TaskLink struct {
	FromSlug   string
	ToSlug     string
	FromKind   string
	SourceFile string
	CreatedAt  string
}

// SyncTaskLinks rebuilds the task backlink index from markdown source files.
// Markdown remains the source of truth; task_links is a cache for fast show/UI
// lookups and can be safely regenerated whenever a read surface needs freshness.
func SyncTaskLinks(db *sql.DB, root string) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("flow root is empty")
	}
	tasks, err := taskLinkTasks(db)
	if err != nil {
		return err
	}
	docs, err := collectTaskLinkDocs(root, tasks)
	if err != nil {
		return err
	}
	links := taskLinksFromDocs(docs, tasks)
	return replaceTaskLinks(db, links)
}

func TaskBacklinks(db *sql.DB, slug string) ([]TaskLink, error) {
	rows, err := db.Query(`
		SELECT from_slug, to_slug, from_kind, source_file, created_at
		FROM task_links
		WHERE to_slug = ?
		ORDER BY from_slug ASC, from_kind ASC, source_file ASC
	`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskLink
	for rows.Next() {
		var link TaskLink
		if err := rows.Scan(&link.FromSlug, &link.ToSlug, &link.FromKind, &link.SourceFile, &link.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, link)
	}
	return out, rows.Err()
}

type taskLinkTask struct {
	Slug string
	Name string
}

type taskLinkDoc struct {
	FromSlug   string
	FromKind   string
	SourceFile string
	Content    string
}

func taskLinkTasks(db *sql.DB) (map[string]taskLinkTask, error) {
	rows, err := db.Query(`SELECT slug, name FROM tasks WHERE deleted_at IS NULL ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := map[string]taskLinkTask{}
	for rows.Next() {
		var task taskLinkTask
		if err := rows.Scan(&task.Slug, &task.Name); err != nil {
			return nil, err
		}
		tasks[task.Slug] = task
	}
	return tasks, rows.Err()
}

func collectTaskLinkDocs(root string, tasks map[string]taskLinkTask) ([]taskLinkDoc, error) {
	slugs := make([]string, 0, len(tasks))
	for slug := range tasks {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	var docs []taskLinkDoc
	for _, slug := range slugs {
		taskDir := filepath.Join(root, "tasks", slug)
		briefPath := filepath.Join(taskDir, "brief.md")
		if doc, ok, err := readTaskLinkDoc(slug, "brief", briefPath); err != nil {
			return nil, err
		} else if ok {
			docs = append(docs, doc)
		}

		updateDir := filepath.Join(taskDir, "updates")
		entries, err := os.ReadDir(updateDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
				continue
			}
			updatePath := filepath.Join(updateDir, entry.Name())
			if doc, ok, err := readTaskLinkDoc(slug, "update", updatePath); err != nil {
				return nil, err
			} else if ok {
				docs = append(docs, doc)
			}
		}
	}
	return docs, nil
}

func readTaskLinkDoc(fromSlug, fromKind, path string) (taskLinkDoc, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return taskLinkDoc{}, false, nil
		}
		return taskLinkDoc{}, false, err
	}
	if info.IsDir() {
		return taskLinkDoc{}, false, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return taskLinkDoc{}, false, err
	}
	return taskLinkDoc{
		FromSlug:   fromSlug,
		FromKind:   fromKind,
		SourceFile: path,
		Content:    string(body),
	}, true, nil
}

func taskLinksFromDocs(docs []taskLinkDoc, tasks map[string]taskLinkTask) []TaskLink {
	seen := map[string]bool{}
	var links []TaskLink
	now := NowISO()
	for _, doc := range docs {
		for _, target := range extractWikiLinkTargets(doc.Content) {
			if _, ok := tasks[target]; !ok {
				continue
			}
			key := doc.FromSlug + "\x00" + target + "\x00" + doc.FromKind + "\x00" + doc.SourceFile
			if seen[key] {
				continue
			}
			seen[key] = true
			links = append(links, TaskLink{
				FromSlug:   doc.FromSlug,
				ToSlug:     target,
				FromKind:   doc.FromKind,
				SourceFile: doc.SourceFile,
				CreatedAt:  now,
			})
		}
	}
	sort.Slice(links, func(i, j int) bool {
		if links[i].ToSlug != links[j].ToSlug {
			return links[i].ToSlug < links[j].ToSlug
		}
		if links[i].FromSlug != links[j].FromSlug {
			return links[i].FromSlug < links[j].FromSlug
		}
		if links[i].FromKind != links[j].FromKind {
			return links[i].FromKind < links[j].FromKind
		}
		return links[i].SourceFile < links[j].SourceFile
	})
	return links
}

func extractWikiLinkTargets(content string) []string {
	matches := wikiLinkRe.FindAllStringSubmatch(content, -1)
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		target := strings.TrimSpace(match[1])
		if target == "" || strings.ContainsAny(target, "/\\|") {
			continue
		}
		out = append(out, target)
	}
	return out
}

func replaceTaskLinks(db *sql.DB, links []TaskLink) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM task_links`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO task_links
			(from_slug, to_slug, from_kind, source_file, created_at)
		VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, link := range links {
		if _, err := stmt.Exec(link.FromSlug, link.ToSlug, link.FromKind, link.SourceFile, link.CreatedAt); err != nil {
			return fmt.Errorf("insert task link %s -> %s: %w", link.FromSlug, link.ToSlug, err)
		}
	}
	return tx.Commit()
}
