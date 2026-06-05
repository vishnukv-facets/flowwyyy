// internal/steering/classifier.go
package steering

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// classifierRunner shells out to the cheap Claude model for the Stage 1/2
// triage classifiers and returns its stdout. Tests swap this var (the same
// package-level function-var seam as app.claudeRunner / iterm.Runner) to
// return canned JSON without invoking claude.
var classifierRunner = func(ctx context.Context, prompt string) (string, error) {
	cmd := exec.CommandContext(ctx, "claude", "-p", prompt,
		"--model", classifierModel(), "--dangerously-skip-permissions")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("steering: classifier claude -p: %w", err)
	}
	return string(out), nil
}

// classifierModel returns the cheap model id used for Stage 1/2. Override via
// FLOW_STEERING_CLASSIFIER_MODEL; defaults to a Haiku-class model.
func classifierModel() string {
	if m := strings.TrimSpace(os.Getenv("FLOW_STEERING_CLASSIFIER_MODEL")); m != "" {
		return m
	}
	return "claude-haiku-4-5"
}

// ClassifyInput is the compact per-event payload handed to the cheap stages.
type ClassifyInput struct {
	ThreadKey string `json:"thread_key"`
	Source    string `json:"source"`
	Author    string `json:"author"`
	Text      string `json:"text"`
}

// RelevanceVerdict is one Stage 1 result.
type RelevanceVerdict struct {
	ThreadKey   string `json:"thread_key"`
	Relevant    bool   `json:"relevant"`
	Category    string `json:"category"`
	UrgencyHint string `json:"urgency_hint"`
}

// Stage1Relevance runs the cheap batched relevance gate over inputs ("is this
// plausibly something the operator needs to see?"). It returns exactly one
// verdict per input, matched back by thread_key. A thread_key present in
// inputs but absent from the model output fails CLOSED (Relevant=false): we
// never surface what the gate didn't explicitly bless.
func Stage1Relevance(ctx context.Context, inputs []ClassifyInput) ([]RelevanceVerdict, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(inputs)
	if err != nil {
		return nil, fmt.Errorf("steering: marshal stage1 inputs: %w", err)
	}
	payloadJSON := string(payload)
	raw, err := runClassifier(ctx, "stage1", stage1Prime(), stage1Payload(payloadJSON), "stage1-v1")
	if err != nil {
		return nil, err
	}
	jsonText, err := extractJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("steering: stage1 parse: %w", err)
	}
	var verdicts []RelevanceVerdict
	if err := json.Unmarshal([]byte(jsonText), &verdicts); err != nil {
		return nil, fmt.Errorf("steering: stage1 unmarshal: %w", err)
	}
	byKey := make(map[string]RelevanceVerdict, len(verdicts))
	for _, v := range verdicts {
		byKey[v.ThreadKey] = v
	}
	out := make([]RelevanceVerdict, len(inputs))
	for i, in := range inputs {
		if v, ok := byKey[in.ThreadKey]; ok {
			out[i] = v
		} else {
			out[i] = RelevanceVerdict{ThreadKey: in.ThreadKey, Relevant: false}
		}
	}
	return out, nil
}

// Stage2Score runs the cheap context-aware scorer for a single survivor. It
// receives the event plus a compact task/project index (BuildTaskIndex) so it
// can propose a matched task / project, and returns a Verdict.
func Stage2Score(ctx context.Context, in ClassifyInput, taskIndex string) (Verdict, error) {
	raw, err := runClassifier(ctx, "stage2", stage2Prime(taskIndex), stage2Payload(in), "stage2:"+shortHash(taskIndex))
	if err != nil {
		return Verdict{}, err
	}
	return parseVerdict(raw, in.Source, in.ThreadKey)
}

// parseVerdict extracts and validates a Verdict from raw model output. An
// unrecognized suggested_action normalizes to ActionDrop (fail-safe: never
// act on a verdict we can't classify). Source/ThreadKey default to the passed
// values when the model omits them.
func parseVerdict(raw, source, threadKey string) (Verdict, error) {
	jsonText, err := extractJSON(raw)
	if err != nil {
		return Verdict{}, fmt.Errorf("steering: verdict parse: %w", err)
	}
	var v Verdict
	if err := json.Unmarshal([]byte(jsonText), &v); err != nil {
		return Verdict{}, fmt.Errorf("steering: verdict unmarshal: %w", err)
	}
	if a, ok := ParseAction(string(v.SuggestedAction)); ok {
		v.SuggestedAction = a
	} else {
		v.SuggestedAction = ActionDrop
	}
	if v.Source == "" {
		v.Source = source
	}
	if v.ThreadKey == "" {
		v.ThreadKey = threadKey
	}
	return v, nil
}

// extractJSON returns the first balanced JSON value (object or array) found in
// s, tolerating surrounding prose or ```json fences that a model may emit.
// Brackets inside JSON strings are ignored. Returns an error if no JSON value
// is present.
func extractJSON(s string) (string, error) {
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return "", fmt.Errorf("no JSON value found")
	}
	open := s[start]
	close := byte('}')
	if open == '[' {
		close = ']'
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced JSON starting at %d", start)
}

// stage1Prime is the static instruction block reused across session turns.
// stage1Payload is the compact per-call tail. By construction:
//
//	stage1Prime() + "\n\n" + stage1Payload(x) == stage1Prompt(x)
func stage1Prime() string {
	return `MODE: stage1-relevance

You are a fast relevance gate for an operator's incoming messages. For EACH event below, decide whether it is plausibly something the operator personally needs to see (a question for them, a request, a decision, an escalation, a blocker) versus noise (chit-chat, FYIs, resolved threads, bot output).

Always refer to people and channels by name; never output raw platform IDs (e.g. Slack user IDs like U0123, channel IDs like C0123).

Respond with ONLY a minified JSON array, no prose and no code fences. One object per event:
[{"thread_key":"<copy of input thread_key>","relevant":true|false,"category":"<short label>","urgency_hint":"urgent|normal|low"}]`
}

func stage1Payload(inputsJSON string) string {
	return "Events (JSON array):\n" + inputsJSON
}

// stage1Prompt is the composed prompt kept for compatibility with any other
// callers. It is byte-identical to stage1Prime()+"\n\n"+stage1Payload(x).
func stage1Prompt(inputsJSON string) string {
	return stage1Prime() + "\n\n" + stage1Payload(inputsJSON)
}

// stage2Prime is the static instruction block for Stage 2 (varies by task
// index). stage2Payload is the compact per-call message tail. By construction:
//
//	stage2Prime(ti) + "\n\n" + stage2Payload(in) == stage2Prompt(in, ti)
func stage2Prime(taskIndex string) string {
	return `MODE: stage2-score

You are scoring one incoming message for an operator. Using the message and the operator's current task/project index, decide the best single action and how confident you are.

Allowed suggested_action values: make_task, forward, reply, afk_reply, digest_only, drop.
- make_task: this should become a tracked task.
- forward: it belongs to an existing task (set matched_task to that slug).
- reply: it needs a reply from the operator.
- digest_only: noteworthy but not actionable now.
- drop: not worth surfacing.

Always refer to people and channels by name; never output raw platform IDs (e.g. Slack user IDs like U0123, channel IDs like C0123).

Respond with ONLY a minified JSON object, no prose, no code fences:
{"suggested_action":"...","matched_task":"<slug or empty>","suggested_project":"<slug or empty>","suggested_priority":"high|medium|low","urgency":"urgent|normal|low","confidence":0.0,"summary":"<= 140 chars","reason":"<why>"}

Operator task/project index:
` + taskIndex
}

func stage2Payload(in ClassifyInput) string {
	payload, _ := json.Marshal(in)
	return "Message (JSON):\n" + string(payload)
}

// stage2Prompt is the composed prompt kept for compatibility with any other
// callers. It is byte-identical to stage2Prime(ti)+"\n\n"+stage2Payload(in).
func stage2Prompt(in ClassifyInput, taskIndex string) string {
	return stage2Prime(taskIndex) + "\n\n" + stage2Payload(in)
}
