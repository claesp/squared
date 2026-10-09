package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"squared/internal/domain"
)

// EnqueueTask adds a task to the outbox. Call it in the same transaction
// as the data change that needs the task.
func (s *Store) EnqueueTask(ctx context.Context, t domain.Task) error {
	b, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode task: %w", err)
	}
	_, err = s.db.Exec(ctx, "INSERT INTO outbox (payload) VALUES ($1)", b)
	return mapErr(err)
}

// PublishOutbox sends at most limit unpublished tasks with publish, in
// order. It marks each task that publish accepts. Rows that other
// processes work on are skipped, thus more than one relay can run. It
// returns the number of published tasks.
func (s *Store) PublishOutbox(ctx context.Context, limit int, publish func(ctx context.Context, payload []byte) error) (int, error) {
	if s.pool == nil {
		return 0, fmt.Errorf("publish outbox: store is in a transaction")
	}
	var published []int64
	var publishErr error
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, payload FROM outbox WHERE published_at IS NULL
			ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		type item struct {
			id      int64
			payload []byte
		}
		var items []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.payload); err != nil {
				rows.Close()
				return err
			}
			items = append(items, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, it := range items {
			if err := publish(ctx, it.payload); err != nil {
				// Stop, but keep the rows that are already published.
				// The remaining rows stay in the outbox for the next run.
				publishErr = err
				break
			}
			published = append(published, it.id)
		}
		if len(published) == 0 {
			return nil
		}
		_, err = tx.Exec(ctx, "UPDATE outbox SET published_at = now() WHERE id = ANY($1)", published)
		return err
	})
	if err != nil {
		// The transaction failed. The broker can thus get a task again
		// in the next run. This is safe because tasks are idempotent.
		return 0, fmt.Errorf("publish outbox: %w", err)
	}
	if publishErr != nil {
		return len(published), fmt.Errorf("publish outbox: %w", publishErr)
	}
	return len(published), nil
}

// PurgeOutbox removes published tasks that are older than age.
func (s *Store) PurgeOutbox(ctx context.Context, age time.Duration) (int64, error) {
	tag, err := s.db.Exec(ctx, "DELETE FROM outbox WHERE published_at < $1", time.Now().Add(-age))
	return tag.RowsAffected(), mapErr(err)
}
