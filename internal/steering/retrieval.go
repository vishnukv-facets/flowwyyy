package steering

import (
	"os"
	"strconv"
	"strings"
	"unicode"

	"flow/internal/flowdb"
)

// retrievalSearch is the seam the steerer uses to pull related context from the
// FTS index (layer 3). It points at flowdb.SearchDocsMatch in production; tests
// swap it to assert bounding/degradation without seeding a real index.
var retrievalSearch = flowdb.SearchDocsMatch

// retrievalStopwords are the high-frequency words that survive a length filter
// but carry no retrieval signal. Kept small and inline; it mirrors the server's
// ask-flow term extractor (which lives in a package the steerer can't import).
var retrievalStopwords = map[string]bool{
	"about": true, "am": true, "an": true, "and": true, "are": true, "as": true,
	"at": true, "be": true, "but": true, "by": true, "can": true, "did": true,
	"do": true, "does": true, "for": true, "from": true, "had": true, "has": true,
	"have": true, "how": true, "if": true, "in": true, "into": true, "is": true,
	"it": true, "its": true, "just": true, "me": true, "my": true, "no": true,
	"not": true, "of": true, "on": true, "or": true, "our": true, "out": true,
	"please": true, "should": true, "so": true, "that": true, "the": true,
	"their": true, "them": true, "then": true, "there": true, "this": true,
	"to": true, "up": true, "us": true, "was": true, "we": true, "were": true,
	"what": true, "when": true, "which": true, "who": true, "why": true,
	"will": true, "with": true, "would": true, "you": true, "your": true,
}

const retrievalMaxTerms = 12

// retrievalTerms turns free message text into a bounded FTS5 MATCH expression
// with OR semantics ("t1* OR t2* …"). OR (not the AND ftsQuery builds) is what
// gives a whole-message query real recall against the KB and past task notes.
// ponytail: length+stopword heuristic; upgrade to tf-idf or embeddings if recall
// proves poor (brief open question — FTS first per spec §7).
func retrievalTerms(texts ...string) string {
	seen := map[string]bool{}
	var terms []string
	for _, text := range texts {
		// Tokenize exactly like ftsQuery so every term is a bare [a-z0-9_] word —
		// no '-' that FTS5 would read as a NOT operator and choke on.
		for _, tok := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
			return !(unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_')
		}) {
			tok = strings.Trim(tok, "_")
			if len(tok) < 2 || retrievalStopwords[tok] || seen[tok] {
				continue
			}
			seen[tok] = true
			terms = append(terms, tok+"*")
			if len(terms) >= retrievalMaxTerms {
				return strings.Join(terms, " OR ")
			}
		}
	}
	return strings.Join(terms, " OR ")
}

// retrieveHistory pulls a small, bounded set of related prior context (KB facts,
// task briefs/updates) for the deep triager via the existing FTS5 index — layer
// 3 of context assembly. FTS first per spec §7; embeddings deferred. Best-effort:
// an empty query, a search error, or no hits all return nil so triage proceeds on
// the thread + memory layers alone. It reads the server-maintained index
// (warmed at boot, refreshed by search/ask-flow) and never owns a sync.
func (c *Cascade) retrieveHistory(in ClassifyInput, pack ThreadContext) []RetrievedDoc {
	if c.DB == nil {
		return nil
	}
	limit := retrievalLimit()
	if limit <= 0 {
		return nil // operator disabled retrieval (FLOW_STEERING_RETRIEVAL_LIMIT=0)
	}
	parts := []string{in.Text}
	if pack.Parent != nil {
		parts = append(parts, pack.Parent.Text)
	}
	expr := retrievalTerms(parts...)
	if expr == "" {
		return nil
	}
	results, err := retrievalSearch(c.DB, expr, flowdb.DefaultSearchScopes(), limit)
	if err != nil {
		c.log("retrieval: search %q: %v", expr, err)
		return nil
	}
	if len(results) == 0 {
		return nil
	}
	out := make([]RetrievedDoc, 0, len(results))
	for _, r := range results {
		out = append(out, RetrievedDoc{Type: r.Type, Slug: r.Slug, Name: r.Name, Snippet: r.Snippet})
	}
	return out
}

func retrievalLimit() int {
	if v := strings.TrimSpace(os.Getenv("FLOW_STEERING_RETRIEVAL_LIMIT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 5
}
