package server

import (
	"flow/internal/flowdb"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	resp := SearchResponse{Query: q}
	if q == "" {
		writeJSON(w, resp)
		return
	}
	scopes, err := flowdb.ParseSearchScopes(r.URL.Query().Get("in"))
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, fmt.Errorf("invalid limit %q", raw), http.StatusBadRequest)
			return
		}
		limit = n
	}
	s.syncSearchThrottled(scopes)
	results, err := flowdb.SearchDocs(s.cfg.DB, q, scopes, limit)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	for _, result := range results {
		mapped := ftsSearchResult(result)
		switch result.Scope {
		case string(flowdb.SearchScopeUpdate):
			resp.Updates = append(resp.Updates, mapped)
		case string(flowdb.SearchScopeTranscript):
			resp.Transcripts = append(resp.Transcripts, mapped)
		case string(flowdb.SearchScopeMemory):
			resp.Memories = append(resp.Memories, mapped)
		default:
			switch result.EntityType {
			case "task":
				resp.Tasks = append(resp.Tasks, mapped)
			case "project":
				resp.Projects = append(resp.Projects, mapped)
			case "playbook":
				resp.Playbooks = append(resp.Playbooks, mapped)
			}
		}
	}
	// FTS matches brief/update/transcript bodies, but many tasks (Slack/GitHub
	// originated) have no brief and only carry their term in the title. Merge a
	// direct name match so entities are always findable by name.
	s.appendNameMatches(&resp, q, limit)
	writeJSON(w, resp)
}

// appendNameMatches prepends tasks/projects/playbooks whose name contains the
// query (case-insensitive LIKE), deduped against the FTS results already in
// resp. This guarantees title hits regardless of FTS body indexing.
func (s *Server) appendNameMatches(resp *SearchResponse, q string, limit int) {
	like := "%" + q + "%"
	for _, e := range []struct {
		table, etype string
		bucket       *[]SearchResult
	}{
		{"tasks", "task", &resp.Tasks},
		{"projects", "project", &resp.Projects},
		{"playbooks", "playbook", &resp.Playbooks},
	} {
		seen := map[string]bool{}
		for _, r := range *e.bucket {
			seen[r.Slug] = true
		}
		rows, err := s.cfg.DB.Query(
			fmt.Sprintf(`SELECT slug, name FROM %s WHERE name LIKE ? AND deleted_at IS NULL AND archived_at IS NULL ORDER BY updated_at DESC LIMIT ?`, e.table),
			like, limit,
		)
		if err != nil {
			continue
		}
		var added []SearchResult
		for rows.Next() {
			var slug, name string
			if rows.Scan(&slug, &name) != nil {
				continue
			}
			if seen[slug] {
				continue
			}
			seen[slug] = true
			added = append(added, SearchResult{Type: e.etype, Scope: "name", Slug: slug, Name: name, URL: searchResultURL(e.etype, slug)})
		}
		rows.Close()
		*e.bucket = append(added, *e.bucket...)
	}
}

// searchSyncThrottle is the minimum gap between background refreshes of an
// already-built scope. Search content (briefs, updates, transcripts, memories)
// changes on the order of minutes, not keystrokes, and new entities are always
// findable by name via appendNameMatches regardless of index freshness — so a
// generous window keeps the index current without burning CPU rebuilding it on
// every search.
const searchSyncThrottle = 30 * time.Second

// syncSearchThrottled keeps the FTS index fresh without ever paying the rebuild
// cost on the hot path. A full filesystem walk + FTS rebuild takes seconds, and
// the palette fires one /api/search per keystroke; doing the rebuild inline
// made the first keystroke of each window pay the whole cost (~7s observed) and
// pegged the CPU while typing.
//
//   - A scope that's never been indexed (cold) is built SYNCHRONOUSLY, so the
//     query that needs it returns correct results rather than an empty list.
//     This is a one-time cost per scope; in production the boot warm-up pays it
//     before any user search.
//   - An already-built scope that's merely stale is refreshed in the BACKGROUND
//     and the request returns immediately against the existing index.
//
// searchSyncing serializes the actual rebuild (only one at a time) so rapid
// typing can't stack goroutines or race a second SQLite writer — the original
// source of intermittent 500s. A refresh failure is non-fatal: the scope's
// timestamp is left unchanged so we retry next call.
func (s *Server) syncSearchThrottled(scopes []flowdb.SearchScope) {
	s.searchSyncMu.Lock()
	now := time.Now()
	var stale []flowdb.SearchScope
	cold := false
	for _, sc := range scopes {
		last, ok := s.searchSyncAt[string(sc)]
		if !ok {
			stale = append(stale, sc)
			cold = true
		} else if now.Sub(last) >= searchSyncThrottle {
			stale = append(stale, sc)
		}
	}
	if len(stale) == 0 || s.searchSyncing {
		s.searchSyncMu.Unlock()
		return
	}
	s.searchSyncing = true
	s.searchSyncMu.Unlock()

	doSync := func() {
		err := flowdb.SyncSearchDocsForScopes(s.cfg.DB, s.cfg.FlowRoot, stale)
		s.searchSyncMu.Lock()
		if err == nil {
			if s.searchSyncAt == nil {
				s.searchSyncAt = make(map[string]time.Time)
			}
			at := time.Now()
			for _, sc := range stale {
				s.searchSyncAt[string(sc)] = at
			}
		}
		s.searchSyncing = false
		s.searchSyncMu.Unlock()
	}

	if cold {
		doSync() // synchronous: the caller's query depends on this scope existing
	} else {
		go doSync() // warm refresh: never block the search request
	}
}

// warmSearchIndex builds the default-scope FTS index once at boot (briefs,
// updates, memories) so the first ⌘K query hits a fresh index instead of
// triggering an inline rebuild. Transcripts are deliberately excluded: they're
// huge and opt-in (the Transcripts chip), so we build them lazily on first use
// rather than paying that cost — and holding a long write transaction — at
// every startup. Routes through syncSearchThrottled so an early search that
// arrives first simply wins the in-flight guard and this becomes a no-op.
func (s *Server) warmSearchIndex() {
	if s.cfg.DB == nil || strings.TrimSpace(s.cfg.FlowRoot) == "" {
		return
	}
	s.syncSearchThrottled(flowdb.DefaultSearchScopes())
}

func ftsSearchResult(result flowdb.SearchResult) SearchResult {
	return SearchResult{
		Type:       result.Type,
		Scope:      result.Scope,
		Slug:       result.Slug,
		Name:       result.Name,
		URL:        searchResultURL(result.EntityType, result.Slug),
		Snippet:    result.Snippet,
		SourcePath: result.SourcePath,
	}
}

func searchResultURL(entityType, slug string) string {
	switch entityType {
	case "task":
		return "/session/" + url.PathEscape(slug)
	case "project":
		return "/project/" + url.PathEscape(slug)
	case "playbook":
		return "/playbook/" + url.PathEscape(slug)
	case "memory":
		return "/memories"
	default:
		return "/"
	}
}
