package flowdb

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

type SearchScope string

const (
	SearchScopeBrief      SearchScope = "brief"
	SearchScopeUpdate     SearchScope = "update"
	SearchScopeTranscript SearchScope = "transcript"
)

type SearchDoc struct {
	Key         string
	Scope       SearchScope
	EntityType  string
	EntitySlug  string
	Title       string
	SourcePath  string
	SourceMTime string
	Content     string
}

type SearchResult struct {
	Type       string `json:"type"`
	Scope      string `json:"scope"`
	EntityType string `json:"entity_type"`
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	SourcePath string `json:"source_path"`
	Snippet    string `json:"snippet"`
	UpdatedAt  string `json:"updated_at"`
}

func DefaultSearchScopes() []SearchScope {
	return []SearchScope{SearchScopeBrief, SearchScopeUpdate}
}

func ParseSearchScopes(raw string) ([]SearchScope, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultSearchScopes(), nil
	}
	seen := map[SearchScope]bool{}
	var scopes []SearchScope
	add := func(scope SearchScope) {
		if !seen[scope] {
			seen[scope] = true
			scopes = append(scopes, scope)
		}
	}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '+'
	}) {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "":
			continue
		case "all":
			add(SearchScopeBrief)
			add(SearchScopeUpdate)
			add(SearchScopeTranscript)
		case "brief", "briefs":
			add(SearchScopeBrief)
		case "update", "updates":
			add(SearchScopeUpdate)
		case "transcript", "transcripts":
			add(SearchScopeTranscript)
		default:
			return nil, fmt.Errorf("invalid search scope %q (want briefs, updates, transcripts, or all)", part)
		}
	}
	if len(scopes) == 0 {
		return DefaultSearchScopes(), nil
	}
	return scopes, nil
}

func SearchScopesInclude(scopes []SearchScope, want SearchScope) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

func SyncSearchDocs(db *sql.DB, root string, includeTranscripts bool) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("flow root is empty")
	}
	docs, err := collectSearchDocs(db, root, includeTranscripts)
	if err != nil {
		return err
	}
	scopes := []SearchScope{SearchScopeBrief, SearchScopeUpdate}
	if includeTranscripts {
		scopes = append(scopes, SearchScopeTranscript)
	}
	return replaceSearchDocs(db, docs, scopes)
}

func SearchDocs(db *sql.DB, query string, scopes []SearchScope, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if len(scopes) == 0 {
		scopes = DefaultSearchScopes()
	}
	ftsQuery := ftsQuery(query)
	if ftsQuery == "" {
		return nil, nil
	}
	scopeArgs := make([]any, 0, len(scopes))
	for _, scope := range scopes {
		scopeArgs = append(scopeArgs, string(scope))
	}
	args := []any{ftsQuery}
	args = append(args, scopeArgs...)
	args = append(args, limit)

	rows, err := db.Query(fmt.Sprintf(`
		SELECT d.scope,
		       d.entity_type,
		       d.entity_slug,
		       d.title,
		       d.source_path,
		       snippet(search_docs_fts, -1, '[', ']', ' ... ', 18),
		       d.updated_at
		FROM search_docs_fts
		JOIN search_docs d ON d.id = search_docs_fts.rowid
		WHERE search_docs_fts MATCH ?
		  AND d.scope IN (%s)
		  AND (
		       (d.entity_type = 'task' AND EXISTS (
		            SELECT 1 FROM tasks t
		            WHERE t.slug = d.entity_slug AND t.archived_at IS NULL AND t.deleted_at IS NULL
		       ))
		    OR (d.entity_type = 'project' AND EXISTS (
		            SELECT 1 FROM projects p
		            WHERE p.slug = d.entity_slug AND p.archived_at IS NULL AND p.deleted_at IS NULL
		       ))
		    OR (d.entity_type = 'playbook' AND EXISTS (
		            SELECT 1 FROM playbooks pb
		            WHERE pb.slug = d.entity_slug AND pb.archived_at IS NULL AND pb.deleted_at IS NULL
		       ))
		  )
		ORDER BY bm25(search_docs_fts), d.updated_at DESC
		LIMIT ?`, sqlPlaceholders(len(scopes))), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.Scope, &r.EntityType, &r.Slug, &r.Name, &r.SourcePath, &r.Snippet, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Type = r.EntityType + "_" + r.Scope
		r.Snippet = cleanSearchSnippet(r.Snippet)
		out = append(out, r)
	}
	return out, rows.Err()
}

func collectSearchDocs(db *sql.DB, root string, includeTranscripts bool) ([]SearchDoc, error) {
	var docs []SearchDoc
	type entity struct {
		typ         string
		dir         string
		slug        string
		name        string
		sessionPath sql.NullString
	}
	load := func(query, typ, dir string, includeSession bool) error {
		rows, err := db.Query(query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e := entity{typ: typ, dir: dir}
			if includeSession {
				if err := rows.Scan(&e.slug, &e.name, &e.sessionPath); err != nil {
					return err
				}
			} else if err := rows.Scan(&e.slug, &e.name); err != nil {
				return err
			}
			entityDocs, err := collectEntityMarkdownDocs(root, e.dir, e.typ, e.slug, e.name)
			if err != nil {
				return err
			}
			docs = append(docs, entityDocs...)
			if includeTranscripts && e.typ == "task" && e.sessionPath.Valid && strings.TrimSpace(e.sessionPath.String) != "" {
				if doc, ok, err := transcriptSearchDoc(e.slug, e.name, e.sessionPath.String); err != nil {
					return err
				} else if ok {
					docs = append(docs, doc)
				}
			}
		}
		return rows.Err()
	}
	if err := load(`SELECT slug, name, session_path FROM tasks WHERE deleted_at IS NULL`, "task", "tasks", true); err != nil {
		return nil, err
	}
	if err := load(`SELECT slug, name FROM projects WHERE deleted_at IS NULL`, "project", "projects", false); err != nil {
		return nil, err
	}
	if err := load(`SELECT slug, name FROM playbooks WHERE deleted_at IS NULL`, "playbook", "playbooks", false); err != nil {
		return nil, err
	}
	return docs, nil
}

func collectEntityMarkdownDocs(root, dir, entityType, slug, name string) ([]SearchDoc, error) {
	entityDir := filepath.Join(root, dir, slug)
	var docs []SearchDoc
	if doc, ok, err := fileSearchDoc(entityType+":"+slug+":brief", SearchScopeBrief, entityType, slug, name+" brief", filepath.Join(entityDir, "brief.md")); err != nil {
		return nil, err
	} else if ok {
		docs = append(docs, doc)
	}
	updateDir := filepath.Join(entityDir, "updates")
	entries, err := os.ReadDir(updateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return docs, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		filename := entry.Name()
		doc, ok, err := fileSearchDoc(
			entityType+":"+slug+":update:"+filename,
			SearchScopeUpdate,
			entityType,
			slug,
			name+" update "+filename,
			filepath.Join(updateDir, filename),
		)
		if err != nil {
			return nil, err
		}
		if ok {
			docs = append(docs, doc)
		}
	}
	return docs, nil
}

func fileSearchDoc(key string, scope SearchScope, entityType, slug, title, path string) (SearchDoc, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return SearchDoc{}, false, nil
		}
		return SearchDoc{}, false, err
	}
	if info.IsDir() {
		return SearchDoc{}, false, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return SearchDoc{}, false, err
	}
	return SearchDoc{
		Key:         key,
		Scope:       scope,
		EntityType:  entityType,
		EntitySlug:  slug,
		Title:       title,
		SourcePath:  path,
		SourceMTime: info.ModTime().Format(timeFormatRFC3339Nano),
		Content:     string(body),
	}, true, nil
}

func transcriptSearchDoc(slug, name, path string) (SearchDoc, bool, error) {
	return fileSearchDoc("task:"+slug+":transcript:"+filepath.Base(path), SearchScopeTranscript, "task", slug, name+" transcript", path)
}

func replaceSearchDocs(db *sql.DB, docs []SearchDoc, scopes []SearchScope) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO search_docs
			(doc_key, scope, entity_type, entity_slug, title, source_path, source_mtime, content, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(doc_key) DO UPDATE SET
			scope = excluded.scope,
			entity_type = excluded.entity_type,
			entity_slug = excluded.entity_slug,
			title = excluded.title,
			source_path = excluded.source_path,
			source_mtime = excluded.source_mtime,
			content = excluded.content,
			updated_at = excluded.updated_at
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := NowISO()
	current := make(map[string]bool, len(docs))
	for _, doc := range docs {
		current[doc.Key] = true
		if _, err := stmt.Exec(doc.Key, string(doc.Scope), doc.EntityType, doc.EntitySlug, doc.Title, doc.SourcePath, doc.SourceMTime, doc.Content, now); err != nil {
			return err
		}
	}

	args := make([]any, 0, len(scopes))
	for _, scope := range scopes {
		args = append(args, string(scope))
	}
	rows, err := tx.Query(fmt.Sprintf(`SELECT doc_key FROM search_docs WHERE scope IN (%s)`, sqlPlaceholders(len(scopes))), args...)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return err
		}
		if !current[key] {
			stale = append(stale, key)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, key := range stale {
		if _, err := tx.Exec(`DELETE FROM search_docs WHERE doc_key = ?`, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func ftsQuery(query string) string {
	seen := map[string]bool{}
	var terms []string
	for _, token := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_')
	}) {
		token = strings.Trim(token, "_")
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		terms = append(terms, token+"*")
	}
	return strings.Join(terms, " ")
}

func sqlPlaceholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

func cleanSearchSnippet(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

const timeFormatRFC3339Nano = "2006-01-02T15:04:05.999999999Z07:00"
