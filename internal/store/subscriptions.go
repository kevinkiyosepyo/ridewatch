package store

import (
	"context"
	"fmt"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// SaveSubscription inserts a subscription, or — since a browser re-subscribing
// reuses its endpoint — updates the existing row's watch in place (and gives
// the endpoint a clean failure slate).
func (s *Store) SaveSubscription(ctx context.Context, sub domain.Subscription) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO push_subscriptions
			(endpoint, p256dh, auth, stop_id, route_id, direction_id, threshold_secs)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (endpoint) DO UPDATE
		SET p256dh = EXCLUDED.p256dh,
		    auth = EXCLUDED.auth,
		    stop_id = EXCLUDED.stop_id,
		    route_id = EXCLUDED.route_id,
		    direction_id = EXCLUDED.direction_id,
		    threshold_secs = EXCLUDED.threshold_secs,
		    failure_count = 0
		RETURNING id`,
		sub.Endpoint, sub.P256dh, sub.Auth, sub.StopID, sub.RouteID, sub.DirectionID, sub.ThresholdSecs).
		Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: save subscription: %w", err)
	}
	return id, nil
}

// DeleteSubscription removes the subscription for an endpoint, returning
// domain.ErrNotFound when none exists.
func (s *Store) DeleteSubscription(ctx context.Context, endpoint string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint = $1`, endpoint)
	if err != nil {
		return fmt.Errorf("store: delete subscription: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: subscription: %w", domain.ErrNotFound)
	}
	return nil
}

// AllSubscriptions lists every subscription for the push evaluator.
func (s *Store) AllSubscriptions(ctx context.Context) ([]domain.Subscription, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, endpoint, p256dh, auth, stop_id, route_id, direction_id,
		       threshold_secs, created_at, failure_count
		FROM push_subscriptions
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list subscriptions: %w", err)
	}
	defer rows.Close()
	var out []domain.Subscription
	for rows.Next() {
		var sub domain.Subscription
		if err := rows.Scan(&sub.ID, &sub.Endpoint, &sub.P256dh, &sub.Auth, &sub.StopID,
			&sub.RouteID, &sub.DirectionID, &sub.ThresholdSecs, &sub.CreatedAt, &sub.FailureCount); err != nil {
			return nil, fmt.Errorf("store: list subscriptions: %w", err)
		}
		sub.CreatedAt = sub.CreatedAt.UTC()
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list subscriptions: %w", err)
	}
	return out, nil
}

// MarkPushSent records intent to alert this (subscription, trip instance,
// stop); fresh is false when a previous pass already claimed it.
func (s *Store) MarkPushSent(ctx context.Context, subscriptionID int64, key domain.EventKey) (bool, error) {
	serviceDate, err := dateFromServiceDate(key.ServiceDate)
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO push_sent (subscription_id, service_date, trip_id, start_time, stop_sequence)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`,
		subscriptionID, serviceDate, key.TripID, key.StartTime, key.StopSequence)
	if err != nil {
		return false, fmt.Errorf("store: mark push sent: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordPushResult updates delivery bookkeeping; an endpoint the push service
// reports gone (404/410) is deleted outright, cascading its push_sent rows.
func (s *Store) RecordPushResult(ctx context.Context, subscriptionID int64, ok bool, gone bool) error {
	var err error
	switch {
	case gone:
		_, err = s.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE id = $1`, subscriptionID)
	case ok:
		_, err = s.pool.Exec(ctx, `
			UPDATE push_subscriptions
			SET last_success = now(), failure_count = 0
			WHERE id = $1`, subscriptionID)
	default:
		_, err = s.pool.Exec(ctx, `
			UPDATE push_subscriptions
			SET failure_count = failure_count + 1
			WHERE id = $1`, subscriptionID)
	}
	if err != nil {
		return fmt.Errorf("store: record push result: %w", err)
	}
	return nil
}
