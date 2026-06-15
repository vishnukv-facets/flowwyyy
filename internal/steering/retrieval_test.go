package steering

import (
	"database/sql"
	"errors"
	"testing"

	"flow/internal/flowdb"
)

func TestRetrievalTermsORExpression(t *testing.T) {
	// Stopwords and <2-char tokens drop; the rest become OR'd prefix terms.
	if got, want := retrievalTerms("Did we ship the oauth migration?", ""), "ship* OR oauth* OR migration*"; got != want {
		t.Fatalf("retrievalTerms = %q, want %q", got, want)
	}
	// '-' must not leak into the expression (FTS5 would read it as a NOT operator).
	if got, want := retrievalTerms("gh-pr review"), "gh* OR pr* OR review*"; got != want {
		t.Fatalf("hyphen split = %q, want %q", got, want)
	}
	if got := retrievalTerms("the and but for"); got != "" {
		t.Fatalf("all-stopword query = %q, want empty", got)
	}
}

func TestRetrieveHistoryBoundsAndDegrades(t *testing.T) {
	c, _ := cascadeFixture(t)
	in := ClassifyInput{Source: "slack", Text: "did we ship the oauth migration"}
	pack := ThreadContext{Parent: &ContextMessage{Text: "release planning"}}
	t.Cleanup(func() { retrievalSearch = flowdb.SearchDocsMatch })

	// Caps fan-out at the retrieval limit even when the index returns more.
	t.Setenv("FLOW_STEERING_RETRIEVAL_LIMIT", "3")
	var gotExpr string
	retrievalSearch = func(_ *sql.DB, expr string, _ []flowdb.SearchScope, limit int) ([]flowdb.SearchResult, error) {
		gotExpr = expr
		rows := make([]flowdb.SearchResult, 0, limit)
		for i := 0; i < limit; i++ {
			rows = append(rows, flowdb.SearchResult{Type: "memory", Slug: "kb", Snippet: "x"})
		}
		return rows, nil
	}
	got := c.retrieveHistory(in, pack)
	if len(got) != 3 {
		t.Fatalf("retrieveHistory returned %d, want 3 (bounded)", len(got))
	}
	if want := "ship* OR oauth* OR migration* OR release* OR planning*"; gotExpr != want {
		t.Fatalf("search expr = %q, want %q", gotExpr, want)
	}

	// A search error degrades to nil — triage proceeds on thread + memory.
	retrievalSearch = func(*sql.DB, string, []flowdb.SearchScope, int) ([]flowdb.SearchResult, error) {
		return nil, errors.New("boom")
	}
	if got := c.retrieveHistory(in, pack); got != nil {
		t.Fatalf("error path returned %+v, want nil", got)
	}

	// Empty/stopword-only query never reaches the index.
	called := false
	retrievalSearch = func(*sql.DB, string, []flowdb.SearchScope, int) ([]flowdb.SearchResult, error) {
		called = true
		return nil, nil
	}
	if got := c.retrieveHistory(ClassifyInput{Text: "the and but"}, ThreadContext{}); got != nil || called {
		t.Fatalf("empty-term query: got=%+v called=%v, want nil and no search", got, called)
	}

	// Disabled via limit=0.
	t.Setenv("FLOW_STEERING_RETRIEVAL_LIMIT", "0")
	called = false
	if got := c.retrieveHistory(in, pack); got != nil || called {
		t.Fatalf("disabled retrieval: got=%+v called=%v, want nil and no search", got, called)
	}
}
