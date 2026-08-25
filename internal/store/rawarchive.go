package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// RecordRawPoll indexes an archived raw blob. A zero feed timestamp (absent
// header) is stored as NULL.
func (s *Store) RecordRawPoll(ctx context.Context, feed domain.Feed, polledAt time.Time, feedTS uint64, sha256, relPath string, size, entities int) error {
	var ts any
	if feedTS != 0 {
		ts = int64(feedTS)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO raw_polls (feed, polled_at, feed_timestamp, sha256, path, bytes, entity_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		string(feed), polledAt.UTC(), ts, sha256, relPath, size, entities)
	if err != nil {
		return fmt.Errorf("store: record raw poll: %w", err)
	}
	return nil
}

// LastSHA returns the sha256 of the most recently archived blob for feed, or
// "" when none exists.
func (s *Store) LastSHA(ctx context.Context, feed domain.Feed) (string, error) {
	var sha string
	err := s.pool.QueryRow(ctx, `
		SELECT sha256 FROM raw_polls WHERE feed = $1
		ORDER BY polled_at DESC, id DESC LIMIT 1`, string(feed)).Scan(&sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: last sha for %s: %w", feed, err)
	}
	return sha, nil
}
