package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
	"github.com/kevinkiyosepyo/ridewatch/internal/metrics"
)

// upsertStopEventSQL is the idempotency guard: a row only advances when the
// incoming feed timestamp is strictly newer, and final rows never reopen.
const upsertStopEventSQL = `
INSERT INTO stop_events (
	service_date, trip_id, start_time, stop_sequence, stop_id, route_id,
	direction_id, schedule_version_id, vehicle_id, scheduled_arrival,
	predicted_arrival, delay_secs, skipped, feed_timestamp, first_seen, last_updated
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (service_date, trip_id, start_time, stop_sequence) DO UPDATE
SET predicted_arrival = EXCLUDED.predicted_arrival,
    delay_secs        = EXCLUDED.delay_secs,
    vehicle_id        = EXCLUDED.vehicle_id,
    feed_timestamp    = EXCLUDED.feed_timestamp,
    last_updated      = EXCLUDED.last_updated,
    update_count      = stop_events.update_count + 1,
    skipped           = EXCLUDED.skipped
WHERE stop_events.feed_timestamp < EXCLUDED.feed_timestamp AND NOT stop_events.final`

// UpsertStopEvents inserts or advances rows on their natural key via one
// pgx.Batch; applied is the summed RowsAffected (rows skipped by the guard
// count zero).
func (s *Store) UpsertStopEvents(ctx context.Context, events []domain.StopEvent) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	b := &pgx.Batch{}
	for i := range events {
		e := &events[i]
		serviceDate, err := dateFromServiceDate(e.ServiceDate)
		if err != nil {
			return 0, fmt.Errorf("store: stop event %s/%s: %w", e.TripID, e.StopID, err)
		}
		observed := e.ObservedAt.UTC()
		b.Queue(upsertStopEventSQL,
			serviceDate, e.TripID, e.StartTime, e.StopSequence, e.StopID, e.RouteID,
			e.DirectionID, e.ScheduleVersionID, e.VehicleID, nullTime(e.ScheduledArrival),
			nullTime(e.PredictedArrival), e.DelaySecs, e.Skipped, int64(e.FeedTimestamp),
			observed, observed)
	}
	applied, err := s.execBatch(ctx, b)
	metrics.StopEventsUpserted.Add(float64(applied))
	if err != nil {
		return applied, fmt.Errorf("store: upsert stop events: %w", err)
	}
	return applied, nil
}

// InsertVehiclePositions appends position history, deduplicating on
// (vehicle_id, ts). Positions without a timestamp are skipped: they cannot be
// placed in time or deduplicated.
func (s *Store) InsertVehiclePositions(ctx context.Context, positions []domain.VehiclePosition) (int, error) {
	b := &pgx.Batch{}
	for i := range positions {
		p := &positions[i]
		if p.Timestamp == 0 {
			continue
		}
		b.Queue(`
			INSERT INTO vehicle_positions
				(ts, vehicle_id, trip_id, route_id, lat, lon, bearing, speed_mps, stop_sequence, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (vehicle_id, ts) DO NOTHING`,
			time.Unix(int64(p.Timestamp), 0).UTC(), p.VehicleID, p.TripID, p.RouteID,
			p.Lat, p.Lon, nullNegFloat32(p.Bearing), nullNegFloat32(p.SpeedMPS),
			nullNegInt(p.StopSequence), p.Status)
	}
	if b.Len() == 0 {
		return 0, nil
	}
	applied, err := s.execBatch(ctx, b)
	metrics.VehiclePositionsWritten.Add(float64(applied))
	if err != nil {
		return applied, fmt.Errorf("store: insert vehicle positions: %w", err)
	}
	return applied, nil
}

// FinalizeDue freezes rows whose best-known time passed more than grace ago:
// actual_arrival takes the last prediction (staying NULL when there never was
// one — no fabricated on-time records) and delay_secs is left as last written.
func (s *Store) FinalizeDue(ctx context.Context, now time.Time, grace time.Duration) (int, error) {
	cutoff := now.UTC().Add(-grace)
	tag, err := s.pool.Exec(ctx, `
		UPDATE stop_events
		SET final = true, actual_arrival = predicted_arrival, last_updated = $1
		WHERE NOT final AND COALESCE(predicted_arrival, scheduled_arrival) < $2`,
		now.UTC(), cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: finalize due: %w", err)
	}
	n := int(tag.RowsAffected())
	metrics.StopEventsFinalized.Add(float64(n))
	return n, nil
}

// execBatch sends b and sums RowsAffected over every queued statement.
func (s *Store) execBatch(ctx context.Context, b *pgx.Batch) (int, error) {
	br := s.pool.SendBatch(ctx, b)
	var applied int
	var execErr error
	for i := 0; i < b.Len(); i++ {
		tag, err := br.Exec()
		if err != nil {
			execErr = err
			break
		}
		applied += int(tag.RowsAffected())
	}
	if err := br.Close(); execErr == nil {
		execErr = err
	}
	return applied, execErr
}

func nullNegFloat32(f float32) any {
	if f < 0 {
		return nil
	}
	return f
}

func nullNegInt(n int) any {
	if n < 0 {
		return nil
	}
	return n
}
