package server

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// QuoteView is the trimmed quote payload the Mission Control greeting reads.
// It backs both sources: anime quotes set Anime+Character, stoic quotes set
// Author. Empty Quote means "none available" — the UI then hides the line.
type QuoteView struct {
	Quote     string `json:"quote"`
	Anime     string `json:"anime"`
	Character string `json:"character"`
	Author    string `json:"author"`
}

// animechanResponse mirrors https://api.animechan.io/v1/quotes/random.
type animechanResponse struct {
	Status string `json:"status"`
	Data   struct {
		Content   string `json:"content"`
		Anime     struct{ Name string `json:"name"` } `json:"anime"`
		Character struct{ Name string `json:"name"` } `json:"character"`
	} `json:"data"`
}

// quoteBucket is the cache key for the anime quote: one bucket per clock hour
// (e.g. "2026-05-31-16"). A new quote is therefore fetched at most once an hour
// and the same quote is served for every refresh within that hour. The date is
// part of the key so 16:00 today and 16:00 tomorrow get different quotes.
func quoteBucket(t time.Time) string {
	return t.Format("2006-01-02-15")
}

// handleQuote returns a random anime quote for the current hour bucket. The
// result is cached per bucket: the external animechan API is hit at most once
// per hour, never on every page load or refresh — that's the rate-limit guard.
// The frontend keys its request by the same hour bucket; the server cache is
// the real backstop across clients/reloads.
func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	if s.cfg.DisableQuote || !missionQuoteEnabled() {
		writeJSON(w, QuoteView{})
		return
	}
	now := time.Now().In(time.Local)
	// The bucket param is the client's hour key; fall back to ours when absent.
	bucket := strings.TrimSpace(r.URL.Query().Get("bucket"))
	if bucket == "" {
		bucket = quoteBucket(now)
	}
	key := bucket

	s.quoteMu.Lock()
	hit := s.quoteKey == key && s.quoteVal.Quote != ""
	cached := s.quoteVal
	s.quoteMu.Unlock()
	if hit {
		writeJSON(w, cached)
		return
	}

	q, err := fetchGreetingQuote(r.Context())
	if err != nil || q.Quote == "" {
		// Don't fail the dashboard over a flaky third-party API — serve the last
		// good quote if we have one, else an empty payload the UI quietly hides.
		writeJSON(w, cached)
		return
	}

	s.quoteMu.Lock()
	s.quoteKey = key
	s.quoteVal = q
	s.quoteMu.Unlock()
	writeJSON(w, q)
}

var animeQuoteEndpoint = "https://api.animechan.io/v1/quotes/random"

// animeQuoteClient is a package var so tests can stub the transport.
var animeQuoteClient = &http.Client{Timeout: 6 * time.Second}

func fetchAnimeQuote(ctx context.Context) (QuoteView, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, animeQuoteEndpoint, nil)
	if err != nil {
		return QuoteView{}, err
	}
	resp, err := animeQuoteClient.Do(req)
	if err != nil {
		return QuoteView{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return QuoteView{}, nil
	}
	var ar animechanResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return QuoteView{}, err
	}
	return QuoteView{
		Quote:     strings.TrimSpace(ar.Data.Content),
		Anime:     strings.TrimSpace(ar.Data.Anime.Name),
		Character: strings.TrimSpace(ar.Data.Character.Name),
	}, nil
}

// stoicResponse mirrors https://stoic.tekloon.net/stoic-quote:
//   {"data":{"author":"Rumi","quote":"Don't grieve. ..."}}
type stoicResponse struct {
	Data struct {
		Author string `json:"author"`
		Quote  string `json:"quote"`
	} `json:"data"`
}

var stoicQuoteEndpoint = "https://stoic.tekloon.net/stoic-quote"

// stoicQuoteClient is a package var so tests can stub the transport.
var stoicQuoteClient = &http.Client{Timeout: 6 * time.Second}

func fetchStoicQuote(ctx context.Context) (QuoteView, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, stoicQuoteEndpoint, nil)
	if err != nil {
		return QuoteView{}, err
	}
	resp, err := stoicQuoteClient.Do(req)
	if err != nil {
		return QuoteView{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return QuoteView{}, nil
	}
	var sr stoicResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return QuoteView{}, err
	}
	return QuoteView{
		Quote:  strings.TrimSpace(sr.Data.Quote),
		Author: strings.TrimSpace(sr.Data.Author),
	}, nil
}

// quoteSources lists the greeting-quote fetchers. fetchGreetingQuote tries them
// in a per-call randomized order, so the hourly greeting alternates between
// anime and stoic quotes and falls through to the next source when one is
// empty/flaky. Package var so tests can pin the set for determinism.
var quoteSources = []func(context.Context) (QuoteView, error){
	fetchAnimeQuote,
	fetchStoicQuote,
}

// fetchGreetingQuote returns one quote from a randomly-ordered source, skipping
// sources that error or return empty. The first non-empty wins; the random
// order is what mixes anime and stoic across hours. Returns the first error
// only if every source failed (so handleQuote can fall back to its cache).
func fetchGreetingQuote(ctx context.Context) (QuoteView, error) {
	var firstErr error
	for _, i := range rand.Perm(len(quoteSources)) {
		q, err := quoteSources[i](ctx)
		if err == nil && q.Quote != "" {
			return q, nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return QuoteView{}, firstErr
}
