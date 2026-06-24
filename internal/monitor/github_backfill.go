package monitor

import (
	"context"
	"database/sql"

	"github.com/google/go-github/v84/github"

	"flow/internal/productdb"
)

// backfillMaxPages bounds how far back a single backfill walks GitHub's hook
// delivery history (100/page), so a long outage can't turn one trigger into an
// unbounded crawl. GitHub retains deliveries for a limited window anyway.
const backfillMaxPages = 10

// BackfillGitHubDeliveries replays GitHub App webhook deliveries that the live
// receiver missed (e.g. while Flow or the public ingress was down). It is the
// CORRECT gap-recovery path — redelivery replay, not search-polling: it lists
// the App's hook deliveries (GET /app/hook/deliveries via an App-JWT client),
// fetches each one's stored payload, and pushes it through the SAME normalize →
// dispatch pipeline the live receiver uses. Dedupe is the shared
// github_webhook_deliveries table (keyed on the X-GitHub-Delivery GUID): a
// delivery already recorded — by the live path or a prior backfill — is skipped,
// and the dispatcher's github_event_log dedupe is a second backstop. Returns the
// number of deliveries actually replayed. A no-op (0, nil) when no App is
// connected.
func BackfillGitHubDeliveries(ctx context.Context, db *sql.DB, dispatch func(context.Context, GitHubEvent) error) (int, error) {
	if db == nil || dispatch == nil {
		return 0, nil
	}
	client, ok, err := newGitHubAppClient()
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}

	replayed := 0
	opts := &github.ListCursorOptions{PerPage: 100}
	for page := 0; page < backfillMaxPages; page++ {
		deliveries, resp, err := client.Apps.ListHookDeliveries(ctx, opts)
		if err != nil {
			return replayed, err
		}
		for _, d := range deliveries {
			if replayDelivery(ctx, db, client, d, dispatch) {
				replayed++
			}
		}
		if resp == nil || resp.Cursor == "" {
			break
		}
		opts.Cursor = resp.Cursor
	}
	return replayed, nil
}

// replayDelivery records, fetches, normalizes, and dispatches one delivery.
// Returns true only when it dispatched at least one event. Already-recorded
// deliveries (live path or prior backfill) are skipped via the GUID idempotency
// table — the same gate the live receiver uses.
func replayDelivery(ctx context.Context, db *sql.DB, client *github.Client, d *github.HookDelivery, dispatch func(context.Context, GitHubEvent) error) bool {
	guid := d.GetGUID()
	event := d.GetEvent()
	if guid == "" || event == "" {
		return false
	}
	isNew, err := productdb.RecordGitHubDelivery(db, productdb.GitHubDeliveryEntry{
		DeliveryID: guid,
		EventType:  event,
		Action:     d.GetAction(),
	})
	if err != nil || !isNew {
		return false // DB error, or already handled — skip
	}

	full, _, err := client.Apps.GetHookDelivery(ctx, d.GetID())
	if err != nil {
		_ = productdb.FinishGitHubDelivery(db, guid, "error", err.Error(), 0)
		return false
	}
	var payload []byte
	if full.Request != nil && full.Request.RawPayload != nil {
		payload = *full.Request.RawPayload
	}
	events, err := NormalizeGitHubWebhook(event, guid, payload)
	if err != nil {
		_ = productdb.FinishGitHubDelivery(db, guid, "error", err.Error(), 0)
		return false
	}
	if len(events) == 0 {
		_ = productdb.FinishGitHubDelivery(db, guid, "ignored", "", 0)
		return false
	}
	for _, ev := range events {
		_ = dispatch(ctx, ev)
	}
	_ = productdb.FinishGitHubDelivery(db, guid, "processed", "", len(events))
	return true
}
