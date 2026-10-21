package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AppQueueRepo holds the frames the MeshCore app collects with CMD_SYNC_NEXT_MESSAGE.
type AppQueueRepo struct{ db *sql.DB }

// Push queues a frame while the companion's app access is on, holding at most size: past it the oldest channel messages go, or this one if only direct messages are left (firmware addToOfflineQueue).
func (r *AppQueueRepo) Push(ctx context.Context, companionID int64, channel bool, frame []byte, size int) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("queueing app frame: %w", err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM app_queue WHERE companion_id = ?1
			AND EXISTS (SELECT 1 FROM companions WHERE id = ?1 AND app_enabled = 1)`, companionID).Scan(&n); err != nil {
		return fmt.Errorf("counting app queue: %w", err)
	}
	if over := n - size + 1; over > 0 {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM app_queue WHERE id IN (SELECT id FROM app_queue WHERE companion_id = ? AND channel = 1 ORDER BY id LIMIT ?)`, companionID, over)
		if err != nil {
			return fmt.Errorf("making room in app queue: %w", err)
		}
		if gone, _ := res.RowsAffected(); gone < int64(over) {
			return tx.Commit()
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO app_queue (companion_id, channel, frame)
		SELECT ?1, ?2, ?3 WHERE EXISTS (SELECT 1 FROM companions WHERE id = ?1 AND app_enabled = 1)`,
		companionID, channel, frame); err != nil {
		return fmt.Errorf("queueing app frame: %w", err)
	}
	return tx.Commit()
}

// Pop removes and returns the oldest queued frame; ok is false when nothing is waiting.
func (r *AppQueueRepo) Pop(ctx context.Context, companionID int64) (frame []byte, ok bool, err error) {
	err = r.db.QueryRowContext(ctx, `
		DELETE FROM app_queue WHERE id = (SELECT min(id) FROM app_queue WHERE companion_id = ?)
		RETURNING frame`, companionID).Scan(&frame)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("taking app frame: %w", err)
	}
	return frame, true, nil
}

// Waiting reports whether anything is queued, so an app polling an empty queue costs no write.
func (r *AppQueueRepo) Waiting(ctx context.Context, companionID int64) (bool, error) {
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM app_queue WHERE companion_id = ?)`, companionID).Scan(&n); err != nil {
		return false, fmt.Errorf("checking app queue: %w", err)
	}
	return n == 1, nil
}
