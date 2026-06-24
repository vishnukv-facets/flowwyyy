package productdb

import (
	"database/sql"
	"fmt"
)

// SteeringTrace is one row of the attention-router decision log: the full
// journey of a single observed event through the triage cascade.
type SteeringTrace struct {
	ID, CreatedAt, Origin, Source         string
	Channel, ChannelType, Author          string
	ThreadKey, TextPreview                string
	Disposition, StageReached, DropReason string // disposition: dropped|surfaced|error
	Stage1Relevant                        *bool  // nil = stage 1 not reached
	Stage1Reason                          string
	Stage2Action                          string
	Stage2Confidence                      float64
	Stage3Action                          string
	Stage3Confidence                      float64
	FinalAction                           string
	FinalConfidence                       float64
	FeedItemID, Error                     string
	AutonomyAction                        string
	AutonomyDecision                      string
	AutonomyReason                        string
	LatencyMS                             int64
	Model                                 string
	TS                                    string // Slack message ts (for permalink)
	TeamID                                string // Slack team/workspace id (for permalink)
	URL                                   string // connector permalink (GitHub item URL, etc.)
}

// InsertSteeringTrace writes one decision-log row.
func InsertSteeringTrace(db *sql.DB, t SteeringTrace) error {
	var s1 any
	if t.Stage1Relevant != nil {
		if *t.Stage1Relevant {
			s1 = 1
		} else {
			s1 = 0
		}
	}
	_, err := db.Exec(`
		INSERT INTO steering_trace (
			id, created_at, origin, source,
			channel, channel_type, author, thread_key, text_preview,
			disposition, stage_reached, drop_reason,
			stage1_relevant, stage1_reason,
			stage2_action, stage2_confidence,
			stage3_action, stage3_confidence,
			final_action, final_confidence,
			feed_item_id, error,
			autonomy_action, autonomy_decision, autonomy_reason,
			latency_ms, model,
			ts, team_id, url
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.CreatedAt, t.Origin, t.Source,
		NullIfEmpty(t.Channel), NullIfEmpty(t.ChannelType), NullIfEmpty(t.Author), NullIfEmpty(t.ThreadKey), NullIfEmpty(t.TextPreview),
		t.Disposition, t.StageReached, NullIfEmpty(t.DropReason),
		s1, NullIfEmpty(t.Stage1Reason),
		NullIfEmpty(t.Stage2Action), t.Stage2Confidence,
		NullIfEmpty(t.Stage3Action), t.Stage3Confidence,
		NullIfEmpty(t.FinalAction), t.FinalConfidence,
		NullIfEmpty(t.FeedItemID), NullIfEmpty(t.Error),
		NullIfEmpty(t.AutonomyAction), NullIfEmpty(t.AutonomyDecision), NullIfEmpty(t.AutonomyReason),
		t.LatencyMS, NullIfEmpty(t.Model),
		NullIfEmpty(t.TS), NullIfEmpty(t.TeamID), NullIfEmpty(t.URL),
	)
	if err != nil {
		return fmt.Errorf("productdb: insert steering trace: %w", err)
	}
	return nil
}

// steeringTraceCols is the SELECT column list shared by ListSteeringTrace and
// GetSteeringTraceByFeedItem; scanSteeringTrace consumes rows in this order.
const steeringTraceCols = `
	id, created_at, origin, source,
	channel, channel_type, author, thread_key, text_preview,
	disposition, stage_reached, drop_reason,
	stage1_relevant, stage1_reason,
	stage2_action, stage2_confidence,
	stage3_action, stage3_confidence,
	final_action, final_confidence,
	feed_item_id, error,
	autonomy_action, autonomy_decision, autonomy_reason,
	latency_ms, model,
	ts, team_id, url`

// scanSteeringTrace scans one steering_trace row (columns in steeringTraceCols
// order) into a SteeringTrace, handling the NULLable columns.
func scanSteeringTrace(rows interface {
	Scan(dest ...any) error
}) (SteeringTrace, error) {
	var tr SteeringTrace
	var channel, channelType, author, threadKey, textPreview, dropReason sql.NullString
	var stage1Reason sql.NullString
	var stage2Action, stage3Action, finalAction, feedItemID, errStr, model sql.NullString
	var autonomyAction, autonomyDecision, autonomyReason sql.NullString
	var ts, teamID, url sql.NullString
	var stage1Rel sql.NullInt64
	var stage2Conf, stage3Conf, finalConf sql.NullFloat64

	if err := rows.Scan(
		&tr.ID, &tr.CreatedAt, &tr.Origin, &tr.Source,
		&channel, &channelType, &author, &threadKey, &textPreview,
		&tr.Disposition, &tr.StageReached, &dropReason,
		&stage1Rel, &stage1Reason,
		&stage2Action, &stage2Conf,
		&stage3Action, &stage3Conf,
		&finalAction, &finalConf,
		&feedItemID, &errStr,
		&autonomyAction, &autonomyDecision, &autonomyReason,
		&tr.LatencyMS, &model,
		&ts, &teamID, &url,
	); err != nil {
		return SteeringTrace{}, err
	}

	tr.Channel = channel.String
	tr.ChannelType = channelType.String
	tr.Author = author.String
	tr.ThreadKey = threadKey.String
	tr.TextPreview = textPreview.String
	tr.DropReason = dropReason.String
	tr.Stage1Reason = stage1Reason.String
	tr.Stage2Action = stage2Action.String
	tr.Stage2Confidence = stage2Conf.Float64
	tr.Stage3Action = stage3Action.String
	tr.Stage3Confidence = stage3Conf.Float64
	tr.FinalAction = finalAction.String
	tr.FinalConfidence = finalConf.Float64
	tr.FeedItemID = feedItemID.String
	tr.Error = errStr.String
	tr.AutonomyAction = autonomyAction.String
	tr.AutonomyDecision = autonomyDecision.String
	tr.AutonomyReason = autonomyReason.String
	tr.Model = model.String
	tr.TS = ts.String
	tr.TeamID = teamID.String
	tr.URL = url.String

	if stage1Rel.Valid {
		v := stage1Rel.Int64 == 1
		tr.Stage1Relevant = &v
	}
	return tr, nil
}

// GetSteeringTraceByFeedItem returns the most recent trace row that surfaced
// the given feed item (feed_item_id == id). Returns (zero, sql.ErrNoRows-wrapped)
// when none — older feed items predate tracing.
func GetSteeringTraceByFeedItem(db *sql.DB, feedID string) (SteeringTrace, error) {
	row := db.QueryRow(
		`SELECT `+steeringTraceCols+`
		FROM steering_trace WHERE feed_item_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		feedID,
	)
	tr, err := scanSteeringTrace(row)
	if err != nil {
		return SteeringTrace{}, fmt.Errorf("productdb: get steering trace by feed item %q: %w", feedID, err)
	}
	return tr, nil
}

// TraceFilter narrows ListSteeringTrace.
type TraceFilter struct {
	Disposition string // "" = all
	Source      string // connector source (slack|github); "" = all
	Since       string // RFC3339 lower bound on created_at; "" = no bound
	Limit       int    // <=0 → 200
}

// ListSteeringTrace returns rows newest-first, filtered.
func ListSteeringTrace(db *sql.DB, f TraceFilter) ([]SteeringTrace, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}

	q := `SELECT ` + steeringTraceCols + `
	FROM steering_trace`

	args := []any{}
	conditions := []string{}

	if f.Disposition != "" {
		conditions = append(conditions, "disposition = ?")
		args = append(args, f.Disposition)
	}
	if f.Source != "" {
		conditions = append(conditions, "source = ?")
		args = append(args, f.Source)
	}
	if f.Since != "" {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, f.Since)
	}
	if len(conditions) > 0 {
		q += " WHERE "
		for i, c := range conditions {
			if i > 0 {
				q += " AND "
			}
			q += c
		}
	}
	q += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("productdb: list steering traces: %w", err)
	}
	defer rows.Close()

	var out []SteeringTrace
	for rows.Next() {
		tr, err := scanSteeringTrace(rows)
		if err != nil {
			return nil, fmt.Errorf("productdb: scan steering trace: %w", err)
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// SteeringFunnel is the aggregate funnel over a time window.
type SteeringFunnel struct {
	Observed      int
	DroppedStage0 int
	DroppedCache  int
	DroppedStage1 int
	DroppedStage2 int
	Surfaced      int
	Errors        int
}

// SteeringTraceLite is the minimal projection the analytics funnel needs: the
// columns required to bucket events over time, trend triage latency, and split
// the connector→task conversion by source.
type SteeringTraceLite struct {
	CreatedAt    string
	Disposition  string // dropped|surfaced|error
	StageReached string
	LatencyMS    int64
	Source       string // slack|github (connector)
	FinalAction  string // make_task|forward|reply|... (the routed decision)
}

// ListSteeringTraceLite returns the lite projection for rows with
// created_at >= since (since == "" → all rows), oldest constraints aside.
// Unlike SteeringFunnelSince (which aggregates window totals in SQL), this
// returns one row per event so the server can bucket them into the analytics
// grid AND compute a true per-bucket p50 latency — a median that cannot be
// reconstructed from per-day SQL aggregates.
func ListSteeringTraceLite(db *sql.DB, since string) ([]SteeringTraceLite, error) {
	q := `SELECT created_at, disposition, stage_reached, latency_ms, source, final_action FROM steering_trace`
	args := []any{}
	if since != "" {
		q += " WHERE created_at >= ?"
		args = append(args, since)
	}
	q += " ORDER BY created_at ASC, id ASC"

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("productdb: list steering trace lite: %w", err)
	}
	defer rows.Close()

	var out []SteeringTraceLite
	for rows.Next() {
		var r SteeringTraceLite
		var stage, source, finalAction sql.NullString
		if err := rows.Scan(&r.CreatedAt, &r.Disposition, &stage, &r.LatencyMS, &source, &finalAction); err != nil {
			return nil, fmt.Errorf("productdb: scan steering trace lite: %w", err)
		}
		r.StageReached = stage.String
		r.Source = source.String
		r.FinalAction = finalAction.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// SteeringFunnelSince returns funnel counts for rows with created_at >= since
// (since == "" → all rows).
func SteeringFunnelSince(db *sql.DB, since string) (SteeringFunnel, error) {
	q := `SELECT disposition, stage_reached, COUNT(*) FROM steering_trace`
	args := []any{}
	if since != "" {
		q += " WHERE created_at >= ?"
		args = append(args, since)
	}
	q += " GROUP BY disposition, stage_reached"

	rows, err := db.Query(q, args...)
	if err != nil {
		return SteeringFunnel{}, fmt.Errorf("productdb: steering funnel: %w", err)
	}
	defer rows.Close()

	var f SteeringFunnel
	for rows.Next() {
		var disposition, stageReached string
		var count int
		if err := rows.Scan(&disposition, &stageReached, &count); err != nil {
			return SteeringFunnel{}, fmt.Errorf("productdb: scan steering funnel: %w", err)
		}
		f.Observed += count
		switch disposition {
		case "surfaced":
			f.Surfaced += count
		case "error":
			f.Errors += count
		case "dropped":
			switch stageReached {
			case "stage0":
				f.DroppedStage0 += count
			case "cache":
				f.DroppedCache += count
			case "stage1":
				f.DroppedStage1 += count
			case "stage2":
				f.DroppedStage2 += count
			}
		}
	}
	return f, rows.Err()
}
