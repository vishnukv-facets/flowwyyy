package flowdb

import (
	"database/sql"
	"errors"
	"flow/internal/memorysrc"
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
	SearchScopeMemory     SearchScope = "memory"
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
	return []SearchScope{SearchScopeBrief, SearchScopeUpdate, SearchScopeMemory}
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
			add(SearchScopeMemory)
		case "brief", "briefs":
			add(SearchScopeBrief)
		case "update", "updates":
			add(SearchScopeUpdate)
		case "transcript", "transcripts":
			add(SearchScopeTranscript)
		case "memory", "memories":
			add(SearchScopeMemory)
		default:
			return nil, fmt.Errorf("invalid search scope %q (want briefs, updates, memories, transcripts, or all)", part)
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
	scopes := DefaultSearchScopes()
	if includeTranscripts {
		scopes = append(scopes, SearchScopeTranscript)
	}
	return SyncSearchDocsForScopes(db, root, scopes)
}

func SyncSearchDocsForScopes(db *sql.DB, root string, scopes []SearchScope) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("flow root is empty")
	}
	scopes = normalizeSearchScopes(scopes)
	docs, err := collectSearchDocs(db, root, scopes)
	if err != nil {
		return err
	}
	return replaceSearchDocs(db, docs, scopes)
}

func SearchDocs(db *sql.DB, query string, scopes []SearchScope, limit int) ([]SearchResult, error) {
	return SearchDocsMatch(db, ftsQuery(query), scopes, limit)
}

// SearchDocsMatch runs a pre-built FTS5 MATCH expression against the search
// indexes. SearchDocs feeds it ftsQuery's AND-of-prefix-terms; callers that need
// OR recall (e.g. the steerer retrieving related context from a whole message)
// build their own `t1* OR t2* …` expression and pass it directly. The expression
// is always bound as a `MATCH ?` parameter, never interpolated.
func SearchDocsMatch(db *sql.DB, fts string, scopes []SearchScope, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if len(scopes) == 0 {
		scopes = DefaultSearchScopes()
	}
	if strings.TrimSpace(fts) == "" {
		return nil, nil
	}
	// Transcripts live in their own FTS index (see schema): partition the
	// requested scopes so each set queries the right index. Non-transcript
	// scopes hit the tiny search_docs_fts (instant); transcripts hit the heavy
	// search_docs_tx_fts only when explicitly requested. The common case is one
	// partition; mixed scopes (CLI `--in all`) run both and merge.
	var main, transcript []SearchScope
	for _, scope := range scopes {
		if scope == SearchScopeTranscript {
			transcript = append(transcript, scope)
		} else {
			main = append(main, scope)
		}
	}
	var out []SearchResult
	if len(main) > 0 {
		rows, err := searchDocsInIndex(db, "search_docs_fts", fts, main, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	if len(transcript) > 0 {
		rows, err := searchDocsInIndex(db, "search_docs_tx_fts", fts, transcript, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// searchDocsInIndex runs the ranked FTS query against one of the two FTS
// indexes (search_docs_fts or search_docs_tx_fts) for the given scopes. The
// index name is interpolated directly — it's never user input, only one of the
// two literals above.
func searchDocsInIndex(db *sql.DB, index, fts string, scopes []SearchScope, limit int) ([]SearchResult, error) {
	args := []any{fts}
	for _, scope := range scopes {
		args = append(args, string(scope))
	}
	args = append(args, limit)

	rows, err := db.Query(fmt.Sprintf(`
		SELECT d.scope,
		       d.entity_type,
		       d.entity_slug,
		       d.title,
		       d.source_path,
		       snippet(%[1]s, -1, '[', ']', ' ... ', 18),
		       d.updated_at
		FROM %[1]s
		JOIN search_docs d ON d.id = %[1]s.rowid
		WHERE %[1]s MATCH ?
		  AND d.scope IN (%[2]s)
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
		    OR d.entity_type = 'memory'
		  )
		ORDER BY bm25(%[1]s), d.updated_at DESC
		LIMIT ?`, index, sqlPlaceholders(len(scopes))), args...)
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
		if r.EntityType == "memory" {
			r.Type = "memory"
		}
		r.Snippet = cleanSearchSnippet(r.Snippet)
		out = append(out, r)
	}
	return out, rows.Err()
}

func collectSearchDocs(db *sql.DB, root string, scopes []SearchScope) ([]SearchDoc, error) {
	var docs []SearchDoc
	includeBriefs := SearchScopesInclude(scopes, SearchScopeBrief)
	includeUpdates := SearchScopesInclude(scopes, SearchScopeUpdate)
	includeTranscripts := SearchScopesInclude(scopes, SearchScopeTranscript)
	includeMemories := SearchScopesInclude(scopes, SearchScopeMemory)
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
			if includeBriefs || includeUpdates {
				entityDocs, err := collectEntityMarkdownDocs(root, e.dir, e.typ, e.slug, e.name, includeBriefs, includeUpdates)
				if err != nil {
					return err
				}
				docs = append(docs, entityDocs...)
			}
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
	if includeBriefs || includeUpdates || includeTranscripts {
		if err := load(`SELECT slug, name, session_path FROM tasks WHERE deleted_at IS NULL`, "task", "tasks", true); err != nil {
			return nil, err
		}
		if includeBriefs || includeUpdates {
			if err := load(`SELECT slug, name FROM projects WHERE deleted_at IS NULL`, "project", "projects", false); err != nil {
				return nil, err
			}
			if err := load(`SELECT slug, name FROM playbooks WHERE deleted_at IS NULL`, "playbook", "playbooks", false); err != nil {
				return nil, err
			}
		}
	}
	if includeMemories {
		memoryDocs, err := collectMemorySearchDocs(db, root)
		if err != nil {
			return nil, err
		}
		docs = append(docs, memoryDocs...)
	}
	return docs, nil
}

func collectEntityMarkdownDocs(root, dir, entityType, slug, name string, includeBriefs, includeUpdates bool) ([]SearchDoc, error) {
	entityDir := filepath.Join(root, dir, slug)
	var docs []SearchDoc
	if includeBriefs {
		briefPath := filepath.Join(entityDir, "brief.md")
		if doc, ok, err := fileSearchDoc(entityType+":"+slug+":brief", SearchScopeBrief, entityType, slug, name+" brief", briefPath); err != nil {
			return nil, err
		} else if ok {
			docs = append(docs, doc)
		} else {
			// No brief.md (common for Slack/GitHub-originated tasks) — still index
			// the name so the entity is findable by title, not just brief body.
			docs = append(docs, SearchDoc{
				Key:        entityType + ":" + slug + ":brief",
				Scope:      SearchScopeBrief,
				EntityType: entityType,
				EntitySlug: slug,
				Title:      name + " brief",
				SourcePath: briefPath,
				Content:    name,
			})
		}
	}
	if !includeUpdates {
		return docs, nil
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

func collectMemorySearchDocs(db *sql.DB, root string) ([]SearchDoc, error) {
	workdirs, err := memorySearchWorkdirs(db)
	if err != nil {
		return nil, err
	}
	sources := memorysrc.AllSources(root, workdirs)
	docs := make([]SearchDoc, 0, len(sources))
	for _, source := range sources {
		if strings.TrimSpace(source.ID) == "" {
			continue
		}
		doc, ok, err := fileSearchDoc(
			"memory:"+source.ID,
			SearchScopeMemory,
			"memory",
			source.ID,
			source.Label,
			source.Path,
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

func memorySearchWorkdirs(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`
		SELECT work_dir FROM tasks WHERE deleted_at IS NULL
		UNION
		SELECT work_dir FROM projects WHERE deleted_at IS NULL
		UNION
		SELECT work_dir FROM playbooks WHERE deleted_at IS NULL
		UNION
		SELECT path FROM workdirs
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		if strings.TrimSpace(path) != "" {
			out = append(out, path)
		}
	}
	return out, rows.Err()
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

func normalizeSearchScopes(scopes []SearchScope) []SearchScope {
	if len(scopes) == 0 {
		scopes = DefaultSearchScopes()
	}
	seen := map[SearchScope]bool{}
	out := make([]SearchScope, 0, len(scopes))
	for _, scope := range scopes {
		if scope == "" || seen[scope] {
			continue
		}
		seen[scope] = true
		out = append(out, scope)
	}
	if len(out) == 0 {
		return DefaultSearchScopes()
	}
	return out
}

const timeFormatRFC3339Nano = "2006-01-02T15:04:05.999999999Z07:00"
