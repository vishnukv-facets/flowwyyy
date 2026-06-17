package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func stubSlackTextSend(t *testing.T, fn func(channel, threadTS, text, identity string) error) {
	t.Helper()
	orig := slackTextSendFn
	t.Cleanup(func() { slackTextSendFn = orig })
	slackTextSendFn = fn
}

func stubSlackFileSend(t *testing.T, fn func(channel, threadTS, comment, filePath, identity string) error) {
	t.Helper()
	orig := slackFileSendFn
	t.Cleanup(func() { slackFileSendFn = orig })
	slackFileSendFn = fn
}

// Non-POST methods are rejected.
func TestHandleSlackSendMethodGuard(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleSlackSend(rec, httptest.NewRequest(http.MethodGet, "/api/slack/send", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// Malformed JSON -> 400.
func TestHandleSlackSendBadPayload(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/slack/send", strings.NewReader("{not json"))
	s.handleSlackSend(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// Missing channel/text -> 400 (validated before SendAsBot).
func TestHandleSlackSendMissingFields(t *testing.T) {
	s := &Server{}
	for _, body := range []string{`{"text":"hi"}`, `{"channel":"D1"}`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/slack/send", strings.NewReader(body))
		s.handleSlackSend(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: code = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestHandleSlackSendForwardsThreadTS(t *testing.T) {
	var gotChannel, gotThreadTS, gotText, gotIdentity string
	stubSlackTextSend(t, func(channel, threadTS, text, identity string) error {
		gotChannel, gotThreadTS, gotText, gotIdentity = channel, threadTS, text, identity
		return nil
	})
	stubSlackFileSend(t, func(channel, threadTS, comment, filePath, identity string) error {
		t.Fatal("file send must not be used for text payload")
		return nil
	})

	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/slack/send",
		strings.NewReader(`{"channel":"C1","thread_ts":"1234.000100","text":"hi","as":"bot"}`))
	s.handleSlackSend(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if gotChannel != "C1" || gotThreadTS != "1234.000100" || gotText != "hi" || gotIdentity != "bot" {
		t.Errorf("forwarded (%q,%q,%q,%q)", gotChannel, gotThreadTS, gotText, gotIdentity)
	}
}

func TestHandleSlackSendFileForwardsThreadTS(t *testing.T) {
	var gotThreadTS string
	stubSlackTextSend(t, func(channel, threadTS, text, identity string) error {
		t.Fatal("text send must not be used for file payload")
		return nil
	})
	stubSlackFileSend(t, func(channel, threadTS, comment, filePath, identity string) error {
		gotThreadTS = threadTS
		return nil
	})

	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/slack/send",
		strings.NewReader(`{"channel":"C1","thread_ts":"1234.000100","text":"caption","file":"/tmp/x.pdf","as":"bot"}`))
	s.handleSlackSend(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if gotThreadTS != "1234.000100" {
		t.Errorf("thread_ts = %q, want 1234.000100", gotThreadTS)
	}
}

func TestHandleSlackSendSurfacesSendError(t *testing.T) {
	boom := errors.New("slack failed")
	stubSlackTextSend(t, func(channel, threadTS, text, identity string) error {
		return boom
	})

	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/slack/send",
		strings.NewReader(`{"channel":"D1","text":"hi"}`))
	s.handleSlackSend(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "slack failed") {
		t.Errorf("body = %q, want send error", rec.Body.String())
	}
}

// With FLOW_SLACK_WRITES_ENABLED unset, monitor.SendAsBot returns "writes
// disabled" -> handler decodes the request and returns 502. Stubless: exercises
// the real decode + method guard + SendAsBot error -> 502 path.
func TestHandleSlackSendWritesDisabled(t *testing.T) {
	t.Setenv("FLOW_SLACK_WRITES_ENABLED", "")
	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/slack/send",
		strings.NewReader(`{"channel":"D1","text":"hi"}`))
	s.handleSlackSend(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want %d (502); body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "writes disabled") {
		t.Errorf("body = %q, want it to surface the SendAsBot error", rec.Body.String())
	}
}
