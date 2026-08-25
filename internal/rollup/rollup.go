// Package rollup rebuilds the three materialized aggregate tables
// (rollup_stop_hour, rollup_departure, rollup_route_hour) from finalized
// stop_events over a trailing window. Each table is rewritten with a
// DELETE-all plus one INSERT...SELECT inside its own transaction, so readers
// never see a partially rebuilt table.
package rollup

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinkiyosepyo/ridewatch/internal/metrics"
)

const defaultWindowWeeks = 6

// eventFilter selects the observations every rollup aggregates over:
// finalized, non-skipped events with a known delay and a resolvable scheduled
// time, whose service date falls inside [$1, $2).
const eventFilter = `final
        AND NOT skipped
        AND delay_secs IS NOT NULL
        AND scheduled_arrival IS NOT NULL
        AND service_date >= $1::date
        AND service_date < $2::date`

// hourOfWeekExpr maps scheduled_arrival to 0 (Monday 00:00) .. 167
// (Sunday 23:00) in the agency timezone ($3), matching domain.HourOfWeek.
const hourOfWeekExpr = `((EXTRACT(ISODOW FROM (scheduled_arrival AT TIME ZONE $3))::int - 1) * 24
        + EXTRACT(HOUR FROM (scheduled_arrival AT TIME ZONE $3))::int)`

// scheduledSecsExpr recovers the GTFS seconds-of-service-day of the scheduled
// stop. The inner expression is the service date's "noon minus 12 hours"
// origin as a timestamptz, which is what keeps a stop scheduled at 08:12 wall
// clock at the same scheduled_secs across DST transitions.
const scheduledSecsExpr = `EXTRACT(EPOCH FROM (scheduled_arrival
        - (((service_date + interval '12 hours') AT TIME ZONE $3) - interval '12 hours')))::int`

const dayClassExpr = `CASE EXTRACT(ISODOW FROM service_date)::int
        WHEN 6 THEN 'saturday'
        WHEN 7 THEN 'sunday'
        ELSE 'weekday'
        END`

const stopHourInsert = `INSERT INTO rollup_stop_hour
    (route_id, stop_id, direction_id, hour_of_week, n,
     p50_delay_secs, p90_delay_secs, late5_pct, early_pct,
     window_start, window_end, computed_at)
SELECT route_id, stop_id, direction_id, hour_of_week, count(*)::int,
       round(percentile_cont(0.5) WITHIN GROUP (ORDER BY delay_secs))::int,
       round(percentile_cont(0.9) WITHIN GROUP (ORDER BY delay_secs))::int,
       avg((delay_secs >= 300)::int)::real,
       avg((delay_secs <= -60)::int)::real,
       $1::date, $2::date, now()
FROM (
    SELECT route_id, stop_id, direction_id, delay_secs,
           ` + hourOfWeekExpr + ` AS hour_of_week
    FROM stop_events
    WHERE ` + eventFilter + `
) obs
GROUP BY route_id, stop_id, direction_id, hour_of_week`

const departureInsert = `INSERT INTO rollup_departure
    (route_id, stop_id, direction_id, scheduled_secs, day_class, n,
     p50_delay_secs, p90_delay_secs, late5_pct,
     window_start, window_end, computed_at)
SELECT route_id, stop_id, direction_id, scheduled_secs, day_class, count(*)::int,
       round(percentile_cont(0.5) WITHIN GROUP (ORDER BY delay_secs))::int,
       round(percentile_cont(0.9) WITHIN GROUP (ORDER BY delay_secs))::int,
       avg((delay_secs >= 300)::int)::real,
       $1::date, $2::date, now()
FROM (
    SELECT route_id, stop_id, direction_id, delay_secs,
           ` + scheduledSecsExpr + ` AS scheduled_secs,
           ` + dayClassExpr + ` AS day_class
    FROM stop_events
    WHERE ` + eventFilter + `
) obs
GROUP BY route_id, stop_id, direction_id, scheduled_secs, day_class`

const routeHourInsert = `INSERT INTO rollup_route_hour
    (route_id, direction_id, hour_of_week, n,
     p50_delay_secs, p90_delay_secs, late5_pct,
     window_start, window_end, computed_at)
SELECT route_id, direction_id, hour_of_week, count(*)::int,
       round(percentile_cont(0.5) WITHIN GROUP (ORDER BY delay_secs))::int,
       round(percentile_cont(0.9) WITHIN GROUP (ORDER BY delay_secs))::int,
       avg((delay_secs >= 300)::int)::real,
       $1::date, $2::date, now()
FROM (
    SELECT route_id, direction_id, delay_secs,
           ` + hourOfWeekExpr + ` AS hour_of_week
    FROM stop_events
    WHERE ` + eventFilter + `
) obs
GROUP BY route_id, direction_id, hour_of_week`

// Run recomputes all three rollup tables over the trailing window ending
// today (exclusive) in the agency timezone. windowWeeks <= 0 falls back to
// the default of 6. All groups are written regardless of size — small-n
// suppression happens on the read side.
func Run(ctx context.Context, pool *pgxpool.Pool, agencyTZ string, windowWeeks int) error {
	if windowWeeks <= 0 {
		windowWeeks = defaultWindowWeeks
	}
	// time.LoadLocation("") silently means UTC, but an empty agency timezone
	// is a misconfiguration (and Postgres rejects AT TIME ZONE ''), so treat
	// it as invalid rather than guessing.
	if agencyTZ == "" {
		metrics.RollupRuns.WithLabelValues("error").Inc()
		return fmt.Errorf("rollup: empty agency timezone")
	}
	loc, err := time.LoadLocation(agencyTZ)
	if err != nil {
		metrics.RollupRuns.WithLabelValues("error").Inc()
		return fmt.Errorf("rollup: invalid agency timezone %q: %w", agencyTZ, err)
	}

	start := time.Now()
	today := start.In(loc)
	windowEnd := today.Format("2006-01-02")
	windowStart := today.AddDate(0, 0, -windowWeeks*7).Format("2006-01-02")

	tables := []struct {
		name      string
		insertSQL string
	}{
		{"rollup_stop_hour", stopHourInsert},
		{"rollup_departure", departureInsert},
		{"rollup_route_hour", routeHourInsert},
	}
	for _, tbl := range tables {
		if err := rebuild(ctx, pool, tbl.name, tbl.insertSQL, windowStart, windowEnd, agencyTZ); err != nil {
			metrics.RollupDuration.Set(time.Since(start).Seconds())
			metrics.RollupRuns.WithLabelValues("error").Inc()
			return err
		}
	}
	metrics.RollupDuration.Set(time.Since(start).Seconds())
	metrics.RollupRuns.WithLabelValues("ok").Inc()
	return nil
}

func rebuild(ctx context.Context, pool *pgxpool.Pool, table, insertSQL, windowStart, windowEnd, tz string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("rollup: begin %s: %w", table, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	if _, err := tx.Exec(ctx, "DELETE FROM "+table); err != nil {
		return fmt.Errorf("rollup: clear %s: %w", table, err)
	}
	if _, err := tx.Exec(ctx, insertSQL, windowStart, windowEnd, tz); err != nil {
		return fmt.Errorf("rollup: rebuild %s: %w", table, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rollup: commit %s: %w", table, err)
	}
	return nil
}
