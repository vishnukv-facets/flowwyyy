package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// QuoteView is the trimmed anime-quote payload the Mission Control greeting
// reads. Empty Quote means "none available" — the UI then hides the line.
type QuoteView struct {
	Quote     string `json:"quote"`
	Anime     string `json:"anime"`
	Character string `json:"character"`
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

// greetingBucket mirrors the frontend greeting() time-of-day buckets so the
// quote rotates in lockstep with the greeting headline.
func greetingBucket(t time.Time) string {
	switch h := t.Hour(); {
	case h >= 5 && h < 12:
		return "morning"
	case h >= 12 && h < 17:
		return "afternoon"
	case h >= 17 && h < 21:
		return "evening"
	default:
		return "night"
	}
}

// handleQuote returns a random anime quote for the current greeting bucket.
// The result is cached per (date + bucket): the external animechan API is hit
// at most once each time the greeting changes, never on every page load or
// refresh — that's the rate-limit guard. The frontend also keys its request
// by bucket, but the server cache is the real backstop across clients/reloads.
func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	if s.cfg.DisableQuote {
		writeJSON(w, QuoteView{})
		return
	}
	now := time.Now().In(time.Local)
	bucket := strings.TrimSpace(r.URL.Query().Get("bucket"))
	if bucket == "" {
		bucket = greetingBucket(now)
	}
	key := now.Format("2006-01-02") + ":" + bucket

	s.quoteMu.Lock()
	hit := s.quoteKey == key && s.quoteVal.Quote != ""
	cached := s.quoteVal
	s.quoteMu.Unlock()
	if hit {
		writeJSON(w, cached)
		return
	}

	q, err := fetchAnimeQuote(r.Context())
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
