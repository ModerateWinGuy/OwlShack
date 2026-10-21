package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// MetricsRepo persists node monitoring; every write method must be wrapped by the caller in store.WriteAsync/WriteSync.
type MetricsRepo struct {
	db *sql.DB
}

// Metric is one time-series reading; Channel is the CayenneLPP channel, 0 for status fields that have none.
type Metric struct {
	TS      int64
	Pubkey  []byte
	Metric  string
	Channel int
	Value   float64
}

// HistoryPoint is one history bucket: its start time (unix seconds) and the average of the readings in it.
type HistoryPoint struct {
	TS    int64   `json:"ts"`
	Value float64 `json:"value"`
}

// NodeState is the latest-known snapshot for a monitored node; State is a JSON object of the most recent value per metric.
type NodeState struct {
	Pubkey      []byte `json:"pubkey"`
	CompanionID string `json:"companionId"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	LastPollTS  int64  `json:"lastPollTs"`
	LastOkTS    int64  `json:"lastOkTs"`
	LastError   string `json:"lastError"`
	State       string `json:"state"` // raw JSON object
}

// Neighbor is one observed neighbour SNR sample at a point in time.
type Neighbor struct {
	TS             int64
	Pubkey         []byte
	NeighborPubkey []byte
	SNR            *float64
}

// RecordMetrics inserts a batch of readings in one transaction. Call inside WriteAsync/WriteSync.
func (r *MetricsRepo) RecordMetrics(ctx context.Context, metrics []Metric) error {
	if len(metrics) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO node_metrics (ts, pubkey, metric, channel, value)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing metric insert: %w", err)
	}
	defer stmt.Close()

	for _, m := range metrics {
		if _, err := stmt.ExecContext(ctx, m.TS, m.Pubkey, m.Metric, m.Channel, m.Value); err != nil {
			return fmt.Errorf("inserting metric: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing tx: %w", err)
	}
	return nil
}

// QueryHistory returns ascending readings for one (pubkey, metric) over from..to (unix seconds, inclusive); bucketSecs <= 0 returns raw points.
func (r *MetricsRepo) QueryHistory(ctx context.Context, pubkey []byte, metric string, from, to, bucketSecs int64) ([]HistoryPoint, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if bucketSecs > 0 {
		// Integer bucket key: floor(ts/bucket)*bucket gives the bucket start.
		rows, err = r.db.QueryContext(ctx, `
			SELECT (ts / ?) * ? AS bucket, AVG(value)
			FROM node_metrics
			WHERE pubkey = ? AND metric = ? AND ts >= ? AND ts <= ?
			GROUP BY bucket
			ORDER BY bucket ASC`,
			bucketSecs, bucketSecs, pubkey, metric, from, to)
	} else {
		rows, err = r.db.QueryContext(ctx, `
			SELECT ts, value
			FROM node_metrics
			WHERE pubkey = ? AND metric = ? AND ts >= ? AND ts <= ?
			ORDER BY ts ASC`,
			pubkey, metric, from, to)
	}
	if err != nil {
		return nil, fmt.Errorf("querying metric history: %w", err)
	}
	defer rows.Close()

	var points []HistoryPoint
	for rows.Next() {
		var p HistoryPoint
		if err := rows.Scan(&p.TS, &p.Value); err != nil {
			return nil, fmt.Errorf("scanning history point: %w", err)
		}
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating metric history: %w", err)
	}
	return points, nil
}

// ListMetricNames returns the distinct metric names recorded for a node.
func (r *MetricsRepo) ListMetricNames(ctx context.Context, pubkey []byte) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT metric FROM node_metrics WHERE pubkey = ? ORDER BY metric ASC`, pubkey)
	if err != nil {
		return nil, fmt.Errorf("querying metric names: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scanning metric name: %w", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating metric names: %w", err)
	}
	return names, nil
}

// UpsertNodeState merges the snapshot onto the stored one per metric, so a partial poll keeps last-known values. Call inside WriteAsync/WriteSync.
func (r *MetricsRepo) UpsertNodeState(ctx context.Context, s *NodeState) error {
	return r.upsertNodeState(ctx, s, false)
}

// UpsertPushedState records readings a node sent unasked: it is alive, but the last poll and its error stand. Call inside WriteAsync/WriteSync.
func (r *MetricsRepo) UpsertPushedState(ctx context.Context, s *NodeState) error {
	return r.upsertNodeState(ctx, s, true)
}

func (r *MetricsRepo) upsertNodeState(ctx context.Context, s *NodeState, pushed bool) error {
	state := s.State
	if strings.TrimSpace(state) == "" {
		state = "{}"
	}
	var prev string
	if err := r.db.QueryRowContext(ctx, `SELECT state FROM node_state WHERE pubkey = ?`, s.Pubkey).Scan(&prev); err == nil {
		if merged, mErr := mergeStateJSON(prev, state); mErr == nil {
			state = merged
		}
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO node_state (pubkey, companion_id, kind, name, last_poll_ts, last_ok_ts, last_error, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pubkey) DO UPDATE SET
			companion_id = excluded.companion_id,
			kind         = excluded.kind,
			name         = excluded.name,
			last_poll_ts = CASE WHEN ? THEN node_state.last_poll_ts ELSE excluded.last_poll_ts END,
			last_ok_ts   = excluded.last_ok_ts,
			last_error   = CASE WHEN ? THEN node_state.last_error ELSE excluded.last_error END,
			state        = excluded.state`,
		s.Pubkey, s.CompanionID, s.Kind, s.Name, s.LastPollTS, s.LastOkTS, s.LastError, state, pushed, pushed); err != nil {
		return fmt.Errorf("upserting node state: %w", err)
	}
	return nil
}

// mergeStateJSON overlays cur on prev (cur wins), returning cur unchanged if either side is not a JSON object.
func mergeStateJSON(prev, cur string) (string, error) {
	pm := map[string]json.RawMessage{}
	cm := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(prev), &pm); err != nil {
		return cur, err
	}
	if err := json.Unmarshal([]byte(cur), &cm); err != nil {
		return cur, err
	}
	for k, v := range cm {
		pm[k] = v
	}
	out, err := json.Marshal(pm)
	if err != nil {
		return cur, err
	}
	return string(out), nil
}

// MarkPollFailure records a failed poll without clobbering the last-known-good snapshot. Call inside WriteAsync/WriteSync.
func (r *MetricsRepo) MarkPollFailure(ctx context.Context, s *NodeState) error {
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO node_state (pubkey, companion_id, kind, last_poll_ts, last_error)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(pubkey) DO UPDATE SET
			companion_id = excluded.companion_id,
			kind         = excluded.kind,
			last_poll_ts = excluded.last_poll_ts,
			last_error   = excluded.last_error`,
		s.Pubkey, s.CompanionID, s.Kind, s.LastPollTS, s.LastError); err != nil {
		return fmt.Errorf("marking poll failure: %w", err)
	}
	return nil
}

// ListNodeStates returns every node snapshot, most-recently-polled first.
func (r *MetricsRepo) ListNodeStates(ctx context.Context) ([]NodeState, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT pubkey, companion_id, kind, name, last_poll_ts, last_ok_ts, last_error, state
		FROM node_state
		ORDER BY last_poll_ts DESC`)
	if err != nil {
		return nil, fmt.Errorf("querying node states: %w", err)
	}
	defer rows.Close()

	var states []NodeState
	for rows.Next() {
		var s NodeState
		if err := rows.Scan(&s.Pubkey, &s.CompanionID, &s.Kind, &s.Name, &s.LastPollTS, &s.LastOkTS, &s.LastError, &s.State); err != nil {
			return nil, fmt.Errorf("scanning node state: %w", err)
		}
		states = append(states, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating node states: %w", err)
	}
	return states, nil
}

// RecordNeighbors inserts a batch of neighbour SNR samples. Call inside WriteAsync/WriteSync.
func (r *MetricsRepo) RecordNeighbors(ctx context.Context, neighbors []Neighbor) error {
	if len(neighbors) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO node_neighbors (ts, pubkey, neighbor_pubkey, snr)
		VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing neighbor insert: %w", err)
	}
	defer stmt.Close()

	for _, n := range neighbors {
		if _, err := stmt.ExecContext(ctx, n.TS, n.Pubkey, n.NeighborPubkey, n.SNR); err != nil {
			return fmt.Errorf("inserting neighbor: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing tx: %w", err)
	}
	return nil
}

// ListLatestNeighbors returns the newest SNR sample per (pubkey, neighbor_pubkey) pair since sinceTS (unix seconds), dropping pairs with no newer sample.
func (r *MetricsRepo) ListLatestNeighbors(ctx context.Context, sinceTS int64) ([]Neighbor, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT pubkey, neighbor_pubkey, MAX(ts) AS ts, snr
		FROM node_neighbors
		WHERE ts >= ?
		GROUP BY pubkey, neighbor_pubkey`, sinceTS)
	if err != nil {
		return nil, fmt.Errorf("querying latest neighbors: %w", err)
	}
	defer rows.Close()

	var out []Neighbor
	for rows.Next() {
		var n Neighbor
		var snr sql.NullFloat64
		if err := rows.Scan(&n.Pubkey, &n.NeighborPubkey, &n.TS, &snr); err != nil {
			return nil, fmt.Errorf("scanning neighbor row: %w", err)
		}
		if snr.Valid {
			v := snr.Float64
			n.SNR = &v
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating latest neighbors: %w", err)
	}
	return out, nil
}

// PruneMetrics deletes time-series rows older than the unix-seconds cutoff. Call inside WriteAsync/WriteSync.
func (r *MetricsRepo) PruneMetrics(ctx context.Context, cutoff int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM node_metrics WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("pruning node metrics: %w", err)
	}
	mRows, _ := res.RowsAffected()
	if _, err := r.db.ExecContext(ctx, `DELETE FROM node_neighbors WHERE ts < ?`, cutoff); err != nil {
		return mRows, fmt.Errorf("pruning node neighbors: %w", err)
	}
	return mRows, nil
}
