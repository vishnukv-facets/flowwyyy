// internal/steering/stream.go
package steering

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
)

// streamSink receives incremental model-output text (deltas) for a stage that is
// being streamed live. It is carried on the context (not a signature parameter)
// so adding streaming touched no classifier/triage call sites.
type streamSink func(delta string)

type streamSinkKey struct{}

func withStreamSink(ctx context.Context, sink streamSink) context.Context {
	return context.WithValue(ctx, streamSinkKey{}, sink)
}

func streamSinkFrom(ctx context.Context) streamSink {
	sink, _ := ctx.Value(streamSinkKey{}).(streamSink)
	return sink
}

// streamingEnabled gates the streaming exec path. Default on; FLOW_STEERING_STREAM=0
// reverts every stage to the proven one-shot `claude -p` exec.
func streamingEnabled() bool {
	return steeringEnvBool("FLOW_STEERING_STREAM", true)
}

// runClaudeStreaming runs `claude -p` in stream-json mode, forwarding assistant
// text/thinking deltas to sink as they arrive and returning the final response
// text for the SAME downstream JSON parsing the one-shot path feeds. Callers must
// treat any error (or empty/garbage output) as a signal to fall back to the
// one-shot exec — streaming is a presentation layer, never load-bearing for the
// verdict.
func runClaudeStreaming(ctx context.Context, args []string, prompt string, sink streamSink) (string, error) {
	full := append([]string{"-p", prompt}, args...)
	full = append(full, "--output-format", "stream-json", "--verbose", "--include-partial-messages")
	cmd := exec.CommandContext(ctx, "claude", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	final, parseErr := parseClaudeStream(stdout, sink)
	waitErr := cmd.Wait()
	if parseErr != nil {
		return "", parseErr
	}
	if waitErr != nil {
		return "", commandError("steering: deep triage claude -p (stream)", waitErr, stderr.Bytes())
	}
	return final, nil
}

// parseClaudeStream reads claude's stream-json NDJSON, forwarding each text/think
// delta to sink and reconstructing the final response. The "result" event's
// result field is authoritative when present (so the verdict text is correct even
// if delta reassembly is imperfect); otherwise the accumulated deltas are used.
// Split out from runClaudeStreaming so it can be unit-tested without claude.
func parseClaudeStream(r io.Reader, sink streamSink) (string, error) {
	sc := bufio.NewScanner(r)
	// Stream-json lines (esp. full assistant/result events) can be large.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var streamed strings.Builder
	var result string
	haveResult := false
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var env struct {
			Type   string          `json:"type"`
			Event  json.RawMessage `json:"event"`
			Result *string         `json:"result"`
		}
		if json.Unmarshal(line, &env) != nil {
			continue // tolerate non-JSON noise rather than failing the whole run
		}
		switch env.Type {
		case "stream_event":
			if t := streamEventText(env.Event); t != "" {
				streamed.WriteString(t)
				if sink != nil {
					sink(t)
				}
			}
		case "result":
			if env.Result != nil {
				result = *env.Result
				haveResult = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if haveResult && strings.TrimSpace(result) != "" {
		return result, nil
	}
	return streamed.String(), nil
}

// streamEventText pulls the text (or thinking) delta out of a partial-message
// stream_event. Empty for non-text events (message_start, content_block_stop…).
func streamEventText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var ev struct {
		Delta struct {
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return ""
	}
	if ev.Delta.Text != "" {
		return ev.Delta.Text
	}
	return ev.Delta.Thinking
}
