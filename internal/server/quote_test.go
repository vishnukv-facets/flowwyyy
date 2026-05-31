package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

const sampleQuoteJSON = `{
  "status": "success",
  "data": {
    "content": "The final door is about to open!",
    "anime": { "id": 575, "name": "Mobile Suit Gundam SEED", "altName": "Kidou Senshi Gundam SEED" },
    "character": { "id": 1486, "name": "Rau Le Creuset" }
  }
}`

func decodeQuote(t *testing.T, body []byte) QuoteView {
	t.Helper()
	var q QuoteView
	if err := json.Unmarshal(body, &q); err != nil {
		t.Fatalf("decode quote: %v (body=%s)", err, body)
	}
	return q
}

func TestHandleQuoteCachesPerBucket(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleQuoteJSON))
	}))
	defer upstream.Close()

	origEndpoint, origClient := animeQuoteEndpoint, animeQuoteClient
	animeQuoteEndpoint = upstream.URL
	animeQuoteClient = upstream.Client()
	defer func() { animeQuoteEndpoint, animeQuoteClient = origEndpoint, origClient }()

	s := &Server{}
	get := func(bucket string) QuoteView {
		req := httptest.NewRequest(http.MethodGet, "/api/quote?bucket="+bucket, nil)
		rec := httptest.NewRecorder()
		s.handleQuote(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		return decodeQuote(t, rec.Body.Bytes())
	}

	first := get("morning")
	if first.Quote == "" || first.Character != "Rau Le Creuset" || first.Anime != "Mobile Suit Gundam SEED" {
		t.Fatalf("unexpected quote: %#v", first)
	}
	// Second call, same bucket → served from cache, no extra upstream hit.
	get("morning")
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected 1 upstream hit for repeated same-bucket calls, got %d", got)
	}
	// Different bucket → quote refreshes, one more upstream hit.
	get("evening")
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("expected 2 upstream hits after bucket change, got %d", got)
	}
}

func TestHandleQuoteServesEmptyWhenUpstreamFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	origEndpoint, origClient := animeQuoteEndpoint, animeQuoteClient
	animeQuoteEndpoint = upstream.URL
	animeQuoteClient = upstream.Client()
	defer func() { animeQuoteEndpoint, animeQuoteClient = origEndpoint, origClient }()

	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/quote?bucket=night", nil)
	rec := httptest.NewRecorder()
	s.handleQuote(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a flaky quote API must not fail the dashboard; status = %d", rec.Code)
	}
	if got := decodeQuote(t, rec.Body.Bytes()); got.Quote != "" {
		t.Fatalf("expected empty quote on upstream failure, got %#v", got)
	}
}

func TestHandleQuoteDisabledSkipsUpstream(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(sampleQuoteJSON))
	}))
	defer upstream.Close()

	origEndpoint, origClient := animeQuoteEndpoint, animeQuoteClient
	animeQuoteEndpoint = upstream.URL
	animeQuoteClient = upstream.Client()
	defer func() { animeQuoteEndpoint, animeQuoteClient = origEndpoint, origClient }()

	s := &Server{cfg: Config{DisableQuote: true}}
	req := httptest.NewRequest(http.MethodGet, "/api/quote?bucket=morning", nil)
	rec := httptest.NewRecorder()
	s.handleQuote(rec, req)
	if got := decodeQuote(t, rec.Body.Bytes()); got.Quote != "" {
		t.Fatalf("expected empty quote when disabled, got %#v", got)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("disabled quote must not call upstream, got %d hits", got)
	}
}
