package steering

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
	"flow/internal/productdb"
)

// fakeHistory is a stand-in SlackHistory that returns canned messages per
// channel and records each History call's args.
type fakeHistory struct {
	byChannel map[string][]monitor.SlackMessage
	errByChan map[string]error
	calls     []struct {
		Channel, Oldest string
		Limit           int
	}
}

func (f *fakeHistory) History(ctx context.Context, ch, oldest string, limit int) ([]monitor.SlackMessage, error) {
	f.calls = append(f.calls, struct {
		Channel, Oldest string
		Limit           int
	}{ch, oldest, limit})
	if f.errByChan != nil && f.errByChan[ch] != nil {
		return nil, f.errByChan[ch]
	}
	return f.byChannel[ch], nil
}

// fakeIMs is a stand-in SlackIMLister returning a fixed set of DM channel ids.
type fakeIMs struct {
	ids []string
	err error
}

func (f *fakeIMs) ListIMs(ctx context.Context) ([]string, error) { return f.ids, f.err }

// fakeReplies is a stand-in SlackThreadReplies returning canned replies per
// thread and recording each Replies call's args.
type fakeReplies struct {
	byThread    map[string][]monitor.SlackMessage
	errByThread map[string]error
	calls       []struct {
		Channel, ThreadTS, Oldest string
		Limit                     int
	}
}

func (f *fakeReplies) Replies(ctx context.Context, ch, threadTS, oldest string, limit int) ([]monitor.SlackMessage, error) {
	f.calls = append(f.calls, struct {
		Channel, ThreadTS, Oldest string
		Limit                     int
	}{ch, threadTS, oldest, limit})
	if f.errByThread != nil && f.errByThread[threadTS] != nil {
		return nil, f.errByThread[threadTS]
	}
	return f.byThread[threadTS], nil
}

func backfillTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func watchOne(ch string) func() WatchConfig {
	return func() WatchConfig { return WatchConfig{WatchedChannels: map[string]bool{ch: true}} }
}

var fixedNow = time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)

func TestBackfillColdStartLookback(t *testing.T) {
	db := backfillTestDB(t)
	fake := &fakeHistory{byChannel: map[string][]monitor.SlackMessage{
		"C1": {
			{TS: "200.000000", User: "U1", Text: "newer"},
			{TS: "100.000000", User: "U2", Text: "older"},
		},
	}}
	var got [][]monitor.InboundEvent
	observe := func(ctx context.Context, evs []monitor.InboundEvent) error {
		got = append(got, evs)
		return nil
	}
	bf := NewSteeringBackfill(db, observe, fake, nil, nil, watchOne("C1"), time.Minute, time.Hour, 50)
	bf.now = func() time.Time { return fixedNow }

	bf.runOnce(context.Background())

	if len(fake.calls) != 1 {
		t.Fatalf("History calls = %d, want 1", len(fake.calls))
	}
	wantOldest := slackTSFromTime(fixedNow.Add(-time.Hour))
	if fake.calls[0].Oldest != wantOldest {
		t.Fatalf("Oldest = %q, want %q", fake.calls[0].Oldest, wantOldest)
	}
	if len(got) != 1 {
		t.Fatalf("observe batches = %d, want 1", len(got))
	}
	if len(got[0]) != 2 {
		t.Fatalf("events in batch = %d, want 2", len(got[0]))
	}
	wm, err := productdb.GetSteeringWatermark(db, "C1")
	if err != nil {
		t.Fatalf("GetSteeringWatermark: %v", err)
	}
	if wm != "200.000000" {
		t.Fatalf("watermark = %q, want %q", wm, "200.000000")
	}
}

func TestBackfillWarmOnlyNewer(t *testing.T) {
	db := backfillTestDB(t)
	if err := productdb.SetSteeringWatermark(db, "C1", "150.000000", fixedNow.Format(time.RFC3339)); err != nil {
		t.Fatalf("SetSteeringWatermark: %v", err)
	}
	fake := &fakeHistory{byChannel: map[string][]monitor.SlackMessage{
		"C1": {
			{TS: "200.000000", User: "U1", Text: "newer"},
			{TS: "100.000000", User: "U2", Text: "older"},
		},
	}}
	var got [][]monitor.InboundEvent
	observe := func(ctx context.Context, evs []monitor.InboundEvent) error {
		got = append(got, evs)
		return nil
	}
	bf := NewSteeringBackfill(db, observe, fake, nil, nil, watchOne("C1"), time.Minute, time.Hour, 50)
	bf.now = func() time.Time { return fixedNow }

	bf.runOnce(context.Background())

	if len(got) != 1 {
		t.Fatalf("observe batches = %d, want 1", len(got))
	}
	if len(got[0]) != 1 {
		t.Fatalf("events = %d, want 1 (only the newer message)", len(got[0]))
	}
	if got[0][0].TS != "200.000000" {
		t.Fatalf("event TS = %q, want %q", got[0][0].TS, "200.000000")
	}
	wm, err := productdb.GetSteeringWatermark(db, "C1")
	if err != nil {
		t.Fatalf("GetSteeringWatermark: %v", err)
	}
	if wm != "200.000000" {
		t.Fatalf("watermark = %q, want %q", wm, "200.000000")
	}
}

func TestBackfillCapWarning(t *testing.T) {
	db := backfillTestDB(t)
	fake := &fakeHistory{byChannel: map[string][]monitor.SlackMessage{
		"C1": {
			{TS: "200.000000", User: "U1", Text: "a"},
			{TS: "100.000000", User: "U2", Text: "b"},
		},
	}}
	observe := func(ctx context.Context, evs []monitor.InboundEvent) error { return nil }
	bf := NewSteeringBackfill(db, observe, fake, nil, nil, watchOne("C1"), time.Minute, time.Hour, 2)
	bf.now = func() time.Time { return fixedNow }

	var logs []string
	bf.SetLogger(func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })

	bf.runOnce(context.Background())

	found := false
	for _, l := range logs {
		if strings.Contains(l, "hit cap") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a log line containing %q, got %v", "hit cap", logs)
	}
}

func TestBackfillSkipsInaccessibleChannelDuringCooldown(t *testing.T) {
	db := backfillTestDB(t)
	fake := &fakeHistory{errByChan: map[string]error{"C1": errors.New("not_in_channel")}}
	observe := func(ctx context.Context, evs []monitor.InboundEvent) error { return nil }
	bf := NewSteeringBackfill(db, observe, fake, nil, nil, watchOne("C1"), time.Minute, time.Hour, 50)
	now := fixedNow
	bf.now = func() time.Time { return now }

	bf.runOnce(context.Background())
	bf.runOnce(context.Background())

	if len(fake.calls) != 1 {
		t.Fatalf("history calls during cooldown = %d, want 1", len(fake.calls))
	}
	now = now.Add(backfillInaccessibleCooldown + time.Second)
	bf.runOnce(context.Background())
	if len(fake.calls) != 2 {
		t.Fatalf("history calls after cooldown = %d, want 2", len(fake.calls))
	}
}

func TestBackfillSkipsSystemSubtypes(t *testing.T) {
	db := backfillTestDB(t)
	fake := &fakeHistory{byChannel: map[string][]monitor.SlackMessage{
		"C1": {
			{TS: "400.000000", User: "U9", SubType: "channel_join"},
			{TS: "300.000000", User: "U1", Text: "hi"},
		},
	}}
	var got [][]monitor.InboundEvent
	observe := func(ctx context.Context, evs []monitor.InboundEvent) error {
		got = append(got, evs)
		return nil
	}
	bf := NewSteeringBackfill(db, observe, fake, nil, nil, watchOne("C1"), time.Minute, time.Hour, 50)
	bf.now = func() time.Time { return fixedNow }

	bf.runOnce(context.Background())

	if len(got) != 1 {
		t.Fatalf("observe batches = %d, want 1", len(got))
	}
	if len(got[0]) != 1 {
		t.Fatalf("events = %d, want 1 (system subtype dropped)", len(got[0]))
	}
	if got[0][0].TS != "300.000000" {
		t.Fatalf("event TS = %q, want %q", got[0][0].TS, "300.000000")
	}
	wm, err := productdb.GetSteeringWatermark(db, "C1")
	if err != nil {
		t.Fatalf("GetSteeringWatermark: %v", err)
	}
	if wm != "400.000000" {
		t.Fatalf("watermark = %q, want %q (advance past filtered system msg)", wm, "400.000000")
	}
}

// The reported failure mode: a reply lands in a watched-channel thread while the
// laptop is asleep. A top-level history sweep never surfaces it (replies aren't
// top-level messages), so the backfill must follow active threads — parents with
// new replies — and recover the reply. The thread root must not be double-counted.
func TestBackfillFollowsActiveThreadReplies(t *testing.T) {
	db := backfillTestDB(t)
	if err := productdb.SetSteeringWatermark(db, "C1", "100.000000", fixedNow.Format(time.RFC3339)); err != nil {
		t.Fatalf("SetSteeringWatermark: %v", err)
	}
	// The thread parent is newer than the (stale, pre-gap) watermark, so it's in
	// the catch-up window; its latest reply is newer still.
	hist := &fakeHistory{byChannel: map[string][]monitor.SlackMessage{
		"C1": {
			{TS: "150.000000", ThreadTS: "150.000000", ReplyCount: 1, LatestReply: "200.000000", User: "U1", Text: "GCP node type?"},
		},
	}}
	reps := &fakeReplies{byThread: map[string][]monitor.SlackMessage{
		"150.000000": {
			{TS: "150.000000", ThreadTS: "150.000000", User: "U1", Text: "GCP node type?"},    // thread root — already seen as top-level
			{TS: "200.000000", ThreadTS: "150.000000", User: "U2", Text: "go with core_nano"}, // missed during sleep
		},
	}}
	var got [][]monitor.InboundEvent
	observe := func(ctx context.Context, evs []monitor.InboundEvent) error { got = append(got, evs); return nil }
	bf := NewSteeringBackfill(db, observe, hist, nil, nil, watchOne("C1"), time.Minute, time.Hour, 50)
	bf.SetRepliesClient(reps)
	bf.now = func() time.Time { return fixedNow }

	bf.runOnce(context.Background())

	var evs []monitor.InboundEvent
	for _, b := range got {
		evs = append(evs, b...)
	}
	var sawParent, sawReply bool
	for _, e := range evs {
		if e.TS == "150.000000" {
			sawParent = true
		}
		if e.TS == "200.000000" {
			sawReply = true
			if e.ThreadTS != "150.000000" {
				t.Fatalf("reply ThreadTS = %q, want 150.000000", e.ThreadTS)
			}
			if !strings.Contains(e.Text, "core_nano") {
				t.Fatalf("reply text = %q, want it to carry the reply body", e.Text)
			}
		}
	}
	if !sawParent {
		t.Fatalf("parent message not observed; events=%+v", evs)
	}
	if !sawReply {
		t.Fatalf("missed thread reply not recovered; events=%+v", evs)
	}
	if len(evs) != 2 {
		t.Fatalf("observed %d events, want 2 (parent + reply, root deduped)", len(evs))
	}
	if len(reps.calls) != 1 || reps.calls[0].ThreadTS != "150.000000" || reps.calls[0].Channel != "C1" {
		t.Fatalf("replies calls = %+v, want one for C1 thread 150.000000", reps.calls)
	}
}

// A thread whose latest reply is no newer than the watermark was already caught
// up: the sweep must NOT spend a conversations.replies call on it.
func TestBackfillSkipsRepliesWhenNoneNewer(t *testing.T) {
	db := backfillTestDB(t)
	if err := productdb.SetSteeringWatermark(db, "C1", "250.000000", fixedNow.Format(time.RFC3339)); err != nil {
		t.Fatalf("SetSteeringWatermark: %v", err)
	}
	hist := &fakeHistory{byChannel: map[string][]monitor.SlackMessage{
		"C1": {
			// Old thread parent, fully caught up: latest reply (240) predates the
			// watermark (250), so there is nothing new to fetch.
			{TS: "100.000000", ThreadTS: "100.000000", ReplyCount: 3, LatestReply: "240.000000", User: "U1", Text: "old thread"},
		},
	}}
	reps := &fakeReplies{byThread: map[string][]monitor.SlackMessage{
		"100.000000": {{TS: "240.000000", ThreadTS: "100.000000", User: "U2", Text: "stale"}},
	}}
	var got [][]monitor.InboundEvent
	observe := func(ctx context.Context, evs []monitor.InboundEvent) error { got = append(got, evs); return nil }
	bf := NewSteeringBackfill(db, observe, hist, nil, nil, watchOne("C1"), time.Minute, time.Hour, 50)
	bf.SetRepliesClient(reps)
	bf.now = func() time.Time { return fixedNow }

	bf.runOnce(context.Background())

	if len(reps.calls) != 0 {
		t.Fatalf("replies calls = %d, want 0 (no replies newer than watermark)", len(reps.calls))
	}
	if len(got) != 0 {
		t.Fatalf("observe batches = %d, want 0 (nothing new)", len(got))
	}
}

func TestBackfillDMsViaIMLister(t *testing.T) {
	db := backfillTestDB(t)
	fake := &fakeHistory{byChannel: map[string][]monitor.SlackMessage{
		"D1": {
			{TS: "500.000000", User: "U_dm", Text: "dm hi"},
		},
	}}
	ims := &fakeIMs{ids: []string{"D1"}}
	var got [][]monitor.InboundEvent
	observe := func(ctx context.Context, evs []monitor.InboundEvent) error {
		got = append(got, evs)
		return nil
	}
	cfg := func() WatchConfig { return WatchConfig{} }
	bf := NewSteeringBackfill(db, observe, nil, fake, ims, cfg, time.Minute, time.Hour, 50)
	bf.now = func() time.Time { return fixedNow }

	bf.runOnce(context.Background())

	if len(got) != 1 {
		t.Fatalf("observe batches = %d, want 1", len(got))
	}
	if len(got[0]) != 1 {
		t.Fatalf("events = %d, want 1", len(got[0]))
	}
	if got[0][0].ChannelType != "im" {
		t.Fatalf("ChannelType = %q, want %q", got[0][0].ChannelType, "im")
	}
	if got[0][0].Channel != "D1" {
		t.Fatalf("Channel = %q, want %q", got[0][0].Channel, "D1")
	}
}

func TestBackfillDMsFallsBackToKnownWatermarksWhenListIMsFails(t *testing.T) {
	db := backfillTestDB(t)
	if err := productdb.SetSteeringWatermark(db, "D03LH2RCZMG", "1780915520.729909", fixedNow.Format(time.RFC3339)); err != nil {
		t.Fatalf("SetSteeringWatermark: %v", err)
	}
	fake := &fakeHistory{byChannel: map[string][]monitor.SlackMessage{
		"D03LH2RCZMG": {
			{
				TS:   "1780916901.021529",
				User: "U03LK2CCE68",
				Files: []monitor.SlackFile{{
					Name:       "PHASE2-PHASE3-EXECUTION-PLAN.md",
					Title:      "PHASE2-PHASE3-EXECUTION-PLAN.md",
					PrettyType: "Markdown (raw)",
				}},
			},
		},
	}}
	ims := &fakeIMs{err: errors.New("missing_scope")}
	var got [][]monitor.InboundEvent
	observe := func(ctx context.Context, evs []monitor.InboundEvent) error {
		got = append(got, evs)
		return nil
	}
	cfg := func() WatchConfig { return WatchConfig{} }
	bf := NewSteeringBackfill(db, observe, nil, fake, ims, cfg, time.Minute, time.Hour, 50)
	bf.now = func() time.Time { return fixedNow }

	bf.runOnce(context.Background())

	if len(fake.calls) != 1 || fake.calls[0].Channel != "D03LH2RCZMG" {
		t.Fatalf("history calls = %+v, want fallback call for known DM", fake.calls)
	}
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("observed = %+v, want one file-share event", got)
	}
	ev := got[0][0]
	if ev.Channel != "D03LH2RCZMG" || ev.ChannelType != "im" {
		t.Fatalf("channel = %q/%q, want D03LH2RCZMG/im", ev.Channel, ev.ChannelType)
	}
	if !strings.Contains(ev.Text, "PHASE2-PHASE3-EXECUTION-PLAN.md") || !strings.Contains(ev.Text, "Markdown") {
		t.Fatalf("event text = %q, want readable file fallback", ev.Text)
	}
	wm, err := productdb.GetSteeringWatermark(db, "D03LH2RCZMG")
	if err != nil {
		t.Fatalf("GetSteeringWatermark: %v", err)
	}
	if wm != "1780916901.021529" {
		t.Fatalf("watermark = %q, want file message ts", wm)
	}
}
