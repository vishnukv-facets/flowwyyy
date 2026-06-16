// internal/steering/triage.go
package steering

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
)

// deepTriageRunner shells out to the capable (default) Claude model for the
// Stage 3 deep triage: it reads full context (e.g. via the Slack MCP), drafts
// a reply, and emits a final Verdict. Tests swap this var. Unlike the cheap
// classifier it does NOT pin --model, so the operator's default (capable)
// model is used.
var deepTriageRunner = func(ctx context.Context, prompt string) (string, error) {
	// When a stage stream sink is on the context (live triage view) and streaming
	// is enabled, run in stream-json mode so the operator watches the verdict form.
	// Any failure — exec, parse, or empty/garbage output — falls through to the
	// proven one-shot exec, so streaming can never break the verdict.
	if sink := streamSinkFrom(ctx); sink != nil && streamingEnabled() {
		if out, err := runClaudeStreaming(ctx, []string{"--dangerously-skip-permissions"}, prompt, sink); err == nil && strings.ContainsAny(out, "{[") {
			return out, nil
		}
	}
	cmd := exec.CommandContext(ctx, "claude", "-p", prompt, "--dangerously-skip-permissions")
	out, err := cmd.Output()
	if err != nil {
		return "", commandError("steering: deep triage claude -p", err, out)
	}
	return string(out), nil
}

// DeepTriage runs Stage 3 on a single survivor. Callers that already fetched a
// deterministic thread context should use DeepTriageWithContext; this wrapper is
// kept for narrow tests and older call sites and passes an event-only fallback
// pack rather than asking the model to fetch context itself.
func DeepTriage(ctx context.Context, in ClassifyInput, taskIndex string) (Verdict, error) {
	return DeepTriageWithContext(ctx, in, taskIndex, contextFromClassifyInput(in))
}

// DeepTriageWithContext runs Stage 3 with the explicit context pack assembled
// by Go. The draft is SURFACED only — P1 never auto-sends it.
func DeepTriageWithContext(ctx context.Context, in ClassifyInput, taskIndex string, pack ThreadContext) (Verdict, error) {
	return DeepTriageWithContextAndHints(ctx, in, taskIndex, pack, nil)
}

// DeepTriageWithContextAndHints runs Stage 3 with deterministic source context
// plus task-impact hints assembled by Go.
func DeepTriageWithContextAndHints(ctx context.Context, in ClassifyInput, taskIndex string, pack ThreadContext, hints []TaskImpactHint) (Verdict, error) {
	return DeepTriageIncremental(ctx, in, taskIndex, pack, hints, IncrementalContext{})
}

// DeepTriageIncremental runs Stage 3 with the full assembled context: the
// current-thread pack (layer 1), the thread's prior running understanding
// (layer 2), and retrieved cross-conversation/KB history (layer 3). When
// inc.Prior is set, the prompt frames the call as an INCREMENTAL update of the
// prior decision rather than a cold re-classification — so repeated events on a
// thread produce stable decisions. A zero IncrementalContext reproduces the
// prior cold behavior for older call sites.
func DeepTriageIncremental(ctx context.Context, in ClassifyInput, taskIndex string, pack ThreadContext, hints []TaskImpactHint, inc IncrementalContext) (Verdict, error) {
	raw, err := deepTriageRunner(ctx, deepTriagePromptIncremental(in, taskIndex, pack, hints, inc))
	if err != nil {
		return Verdict{}, err
	}
	return parseVerdict(raw, in.Source, in.ThreadKey)
}

func deepTriagePrompt(in ClassifyInput, taskIndex string) string {
	return deepTriagePromptWithContext(in, taskIndex, contextFromClassifyInput(in))
}

func deepTriagePromptWithContext(in ClassifyInput, taskIndex string, pack ThreadContext) string {
	return deepTriagePromptWithContextAndHints(in, taskIndex, pack, nil)
}

func deepTriagePromptWithContextAndHints(in ClassifyInput, taskIndex string, pack ThreadContext, hints []TaskImpactHint) string {
	return deepTriagePromptIncremental(in, taskIndex, pack, hints, IncrementalContext{})
}

func deepTriagePromptIncremental(in ClassifyInput, taskIndex string, pack ThreadContext, hints []TaskImpactHint, inc IncrementalContext) string {
	if hints == nil {
		hints = []TaskImpactHint{}
	}
	inc = modelFacingIncremental(in.Source, inc)
	payload, _ := json.Marshal(modelFacingClassifyInput(in))
	contextPayload, _ := json.Marshal(modelFacingThreadContext(pack))
	hintPayload, _ := json.Marshal(modelFacingTaskImpactHints(in.Source, hints))

	incrementalDirective := ""
	priorBlock := ""
	correctionsDirective := ""
	correctionsBlock := ""
	if inc.Prior != nil {
		priorPayload, _ := json.Marshal(inc.Prior)
		incrementalDirective = "\nINCREMENTAL UPDATE — this thread already has a running understanding from earlier events (see \"Prior running understanding\" below). Do NOT cold-classify the latest message. START from your prior decision and treat the new message as a DELTA: keep the prior suggested_action, matched_task, and confidence unless the new message materially changes them; never flip-flop on noise or a re-delivery. Factor in any operator actions or operator replies already recorded on the thread. In \"reason\", state what changed since the prior decision, or that nothing material changed and you are holding it.\n"
		priorBlock = "\n\nPrior running understanding (JSON) — your last decision on this thread; update it incrementally:\n" + string(priorPayload)
		if len(inc.Prior.Corrections) > 0 {
			correctionsPayload, _ := json.Marshal(inc.Prior.Corrections)
			correctionsDirective = "\nOPERATOR CORRECTIONS — the operator has explicitly corrected your understanding of THIS thread (see \"Operator corrections\" below). Treat these as AUTHORITATIVE GROUND TRUTH: they override your own inference, the prior decision, and any retrieved context where they conflict. Re-decide in light of them and reflect them in \"reason\".\n"
			correctionsBlock = "\n\nOperator corrections (JSON) — authoritative operator-supplied context for this thread; obey them over your own reading:\n" + string(correctionsPayload)
		}
	}
	retrievedBlock := ""
	if len(inc.Retrieved) > 0 {
		retrievedPayload, _ := json.Marshal(inc.Retrieved)
		retrievedBlock = "\n\nRetrieved related context (JSON) — KB facts and past task notes the system pulled to help resolve references to things decided earlier or in other conversations. Treat as supporting evidence, not the thread itself:\n" + string(retrievedPayload)
	}

	return `MODE: stage3-deep
` + incrementalDirective + correctionsDirective + `
You are the deep-triage step of an operator's attention router. A cheap gate has already decided this message is worth a closer look. Go has already fetched the surrounding source context into the context pack below. Treat that context pack as the primary source of truth; do not rely on fetching Slack/GitHub context yourself. If fetch_status is "error" or "unavailable", proceed from the fallback event context and lower confidence when the missing context matters.

Do the following, then emit a single verdict:

1. Read the context pack's source permalink, parent message, replies/comments, participants, timestamps, and pre-summary.
2. Decide whether this message belongs to an EXISTING task (set matched_task) or warrants a new one. Do NOT decide from the task name alone — for any plausibly related task, use your file tools to READ that task's brief.md AND the progress notes in its updates/ directory (paths are given in the index below) before judging. Set matched_task ONLY when there is CONCRETE linkage, not mere topical similarity: the same Slack thread/DM or the same participants, an explicit reference to that task's specific work (its PR/issue/branch, customer, service, or component), or an unmistakable continuation of the exact thing that task is doing. A shared theme alone is NOT enough — many unrelated efforts share vocabulary ("a migration", "a deploy", "a release", "a retention/grace period", "an env cutover"). If the only connection is thematic, or the channel/participants/specifics differ from the task and you cannot confirm the link from the context pack, do NOT set matched_task: use digest_only (FYI, no task) or make_task. Treat missing or unresolvable disambiguating context (e.g. an unknown channel, an ambiguous participant) as evidence AGAINST a match — lower confidence and prefer digest_only over forcing a forward. A genuine cross-thread continuation of a task's work (same effort, different thread/DM) still matches — but only when that concrete linkage is present, never when it is merely assumed. When you do set matched_task with a forward, your confidence must reflect the strength of the linkage: reserve high confidence for concrete links and keep thematic guesses low.
3. If a reply from the operator is appropriate, draft it in the operator's voice. DO NOT SEND ANYTHING — the draft is surfaced for the operator's approval only.
4. Read task-impact hints. Availability/FYI events are not automatically actionable. If hints show the sender or named participant is blocking/reviewing/assigned to/affecting active work, set matched_task to the strongest affected task and explain impact. Use "forward" when the affected task/session should know about the update. Use "digest_only" when there is no affected task and no reply needed.
5. Consider "capture_kb": if the message's lasting value is a DECISION, a PLAN, or an org/process/product fact the operator should remember long-term — and there is no action for them to take — prefer capture_kb over make_task. capture_kb and make_task are mutually exclusive: choose make_task when there is work to do, capture_kb when the value is the durable knowledge itself.

` + confidenceRubric() + `

Always refer to people and channels by name; never output raw platform IDs (e.g. Slack user IDs like U0123, channel IDs like C0123).
Do not mention context fetch failures, API/token/channel access errors, fetch_status, fetch_error, or missing source context in summary, draft, or reason. Those fields are internal audit details; base the verdict on the visible fallback event context and lower confidence only when missing context materially changes the decision.

Respond with ONLY a minified JSON object (no prose, fences allowed but optional):
{"suggested_action":"make_task|capture_kb|forward|reply|afk_reply|digest_only|drop","matched_task":"<slug or empty>","suggested_project":"<slug or empty>","suggested_priority":"high|medium|low","urgency":"urgent|normal|low","confidence":0.0,"summary":"<= 140 chars","draft":"<reply text, if any>","reason":"<why>"}

Operator task/project index:
` + taskIndex + `

Task-impact hints (JSON):
` + string(hintPayload) + correctionsBlock + priorBlock + retrievedBlock + `

Context pack (JSON):
` + string(contextPayload) + `

Message (JSON):
` + string(payload)
}

func modelFacingClassifyInput(in ClassifyInput) ClassifyInput {
	out := in
	if out.Source == "slack" {
		out.Author = modelFacingSlackText(out.Author, "Slack participant")
		out.ThreadKey = modelFacingSlackThreadKey(out.ThreadKey)
		out.Text = modelFacingSlackText(out.Text, "Slack participant")
	}
	return out
}

func modelFacingThreadContext(pack ThreadContext) ThreadContext {
	out := pack
	out.FetchError = ""
	if out.Source == "slack" {
		out.ThreadKey = modelFacingSlackThreadKey(out.ThreadKey)
		out.Summary = modelFacingSlackText(out.Summary, "Slack participant")
		out.Participants = modelFacingSlackList(out.Participants)
		out.Permalink = ""
	}
	if pack.Parent != nil {
		parent := modelFacingContextMessage(out.Source, *pack.Parent)
		out.Parent = &parent
	}
	if len(pack.Messages) > 0 {
		out.Messages = make([]ContextMessage, 0, len(pack.Messages))
		for _, msg := range pack.Messages {
			out.Messages = append(out.Messages, modelFacingContextMessage(out.Source, msg))
		}
	}
	return out
}

func modelFacingContextMessage(source string, msg ContextMessage) ContextMessage {
	if source != "slack" {
		return msg
	}
	msg.Author = modelFacingSlackText(msg.Author, "Slack participant")
	msg.Text = modelFacingSlackText(msg.Text, "Slack participant")
	msg.Permalink = ""
	return msg
}

// modelFacingIncremental sanitizes the prior-understanding (layer 2) and
// retrieved-context (layer 3) layers before they reach the model: Slack IDs are
// stripped (consistent with modelFacingThreadContext), operator replies are
// capped to the most recent few and truncated, and retrieved snippets are
// length-bounded so retrieval can't blow the prompt budget. Returns a copy; the
// stored ThreadState is never mutated.
func modelFacingIncremental(source string, inc IncrementalContext) IncrementalContext {
	slack := source == "slack"
	if inc.Prior != nil {
		p := *inc.Prior
		if slack {
			p.Reason = modelFacingSlackText(p.Reason, "Slack participant")
			p.Summary = modelFacingSlackText(p.Summary, "Slack participant")
		}
		p.OperatorActions = copyShortList(slack, p.OperatorActions, 0)
		p.OperatorReplies = copyShortList(slack, p.OperatorReplies, 3)
		p.Corrections = copyShortList(slack, p.Corrections, 0)
		inc.Prior = &p
	}
	if len(inc.Retrieved) > 0 {
		out := make([]RetrievedDoc, 0, len(inc.Retrieved))
		for _, d := range inc.Retrieved {
			d.Snippet = truncate(d.Snippet, 240)
			if slack {
				d.Snippet = modelFacingSlackText(d.Snippet, "Slack participant")
				d.Name = modelFacingSlackText(d.Name, "Slack participant")
			}
			out = append(out, d)
		}
		inc.Retrieved = out
	}
	return inc
}

// copyShortList returns a fresh, optionally Slack-sanitized copy of a short
// string list, keeping only the last keepLast entries (0 = keep all) and
// truncating each. Used for operator action/reply labels fed to the model.
func copyShortList(slack bool, in []string, keepLast int) []string {
	if len(in) == 0 {
		return nil
	}
	if keepLast > 0 && len(in) > keepLast {
		in = in[len(in)-keepLast:]
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = truncate(strings.TrimSpace(s), 160)
		if slack {
			s = modelFacingSlackText(s, "Slack participant")
		}
		out = append(out, s)
	}
	return out
}

func modelFacingTaskImpactHints(source string, hints []TaskImpactHint) []TaskImpactHint {
	if len(hints) == 0 {
		return []TaskImpactHint{}
	}
	out := make([]TaskImpactHint, len(hints))
	copy(out, hints)
	if source != "slack" {
		return out
	}
	for i := range out {
		out[i].TaskName = modelFacingSlackText(out[i].TaskName, "Slack participant")
		out[i].Reason = modelFacingSlackText(out[i].Reason, "Slack participant")
		out[i].Evidence = modelFacingSlackText(out[i].Evidence, "Slack participant")
	}
	return out
}

func modelFacingSlackList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		cleaned := modelFacingSlackText(value, "Slack participant")
		if cleaned == "" || seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		out = append(out, cleaned)
	}
	return out
}

func modelFacingSlackThreadKey(threadKey string) string {
	if strings.TrimSpace(threadKey) == "" {
		return ""
	}
	return "slack-thread"
}

func modelFacingSlackText(text, userReplacement string) string {
	text = operatorSlackUserIDRE.ReplaceAllString(text, userReplacement)
	text = operatorSlackChannelRE.ReplaceAllString(text, "Slack channel")
	return text
}

func contextFromClassifyInput(in ClassifyInput) ThreadContext {
	pack := ThreadContext{
		Source:      in.Source,
		ThreadKey:   in.ThreadKey,
		FetchStatus: "unavailable",
		FetchError:  "deterministic context pack was not provided",
	}
	if in.Text != "" || in.Author != "" {
		pack.Parent = &ContextMessage{Kind: "event", Author: in.Author, Text: in.Text}
	}
	pack.Participants, pack.Timestamps = deriveContextMeta(pack.Parent, pack.Messages)
	pack.Summary = summarizeThreadContext(pack.Source, pack.Parent, pack.Messages)
	return pack
}
