package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// Reference data (stops, routes) is always read from the active schedule
// version. With no version loaded yet, list queries return empty results and
// single-row lookups return domain.ErrNotFound — the API stays up either way.

// activeVersionID returns the id of the active schedule version, or
// domain.ErrNotFound when none is loaded.
func (s *Store) activeVersionID(ctx context.Context) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM schedule_versions WHERE active`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("store: active schedule version: %w", domain.ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("store: active schedule version: %w", err)
	}
	return id, nil
}

const stopColumns = `stop_id, name, COALESCE(lat, 0), COALESCE(lon, 0), parent_station, location_type, platform_code`

// likeEscape escapes LIKE metacharacters so user input matches literally.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// SearchStops matches stop names case-insensitively (prefix matches first)
// plus exact stop ids. Entrances and generic nodes (location_type > 1) are
// never search results.
func (s *Store) SearchStops(ctx context.Context, q string, limit int) ([]domain.Stop, error) {
	verID, err := s.activeVersionID(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	needle := likeEscape(strings.ToLower(strings.TrimSpace(q)))
	rows, err := s.pool.Query(ctx, `
		SELECT `+stopColumns+`
		FROM gtfs_stops
		WHERE schedule_version_id = $1 AND location_type <= 1
		  AND (lower(name) LIKE '%' || $2 || '%' OR stop_id = $3)
		ORDER BY CASE WHEN lower(name) LIKE $2 || '%' THEN 0 ELSE 1 END,
		         location_type DESC, -- stations before their platforms
		         name, stop_id
		LIMIT $4`, verID, needle, q, limit)
	if err != nil {
		return nil, fmt.Errorf("store: search stops: %w", err)
	}
	return scanStops(rows, "search stops")
}

// StopsInBBox returns stops and stations inside the box, for the map layer.
func (s *Store) StopsInBBox(ctx context.Context, minLat, minLon, maxLat, maxLon float64, limit int) ([]domain.Stop, error) {
	verID, err := s.activeVersionID(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+stopColumns+`
		FROM gtfs_stops
		WHERE schedule_version_id = $1 AND location_type <= 1
		  AND lat BETWEEN $2 AND $3 AND lon BETWEEN $4 AND $5
		ORDER BY stop_id
		LIMIT $6`, verID, minLat, maxLat, minLon, maxLon, limit)
	if err != nil {
		return nil, fmt.Errorf("store: stops in bbox: %w", err)
	}
	return scanStops(rows, "stops in bbox")
}

func scanStops(rows pgx.Rows, op string) ([]domain.Stop, error) {
	defer rows.Close()
	var out []domain.Stop
	for rows.Next() {
		var st domain.Stop
		if err := rows.Scan(&st.StopID, &st.Name, &st.Lat, &st.Lon, &st.ParentStation, &st.LocationType, &st.PlatformCode); err != nil {
			return nil, fmt.Errorf("store: %s: %w", op, err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: %s: %w", op, err)
	}
	return out, nil
}

func (s *Store) Stop(ctx context.Context, stopID string) (*domain.Stop, error) {
	verID, err := s.activeVersionID(ctx)
	if err != nil {
		return nil, err
	}
	var st domain.Stop
	err = s.pool.QueryRow(ctx, `
		SELECT `+stopColumns+`
		FROM gtfs_stops
		WHERE schedule_version_id = $1 AND stop_id = $2`, verID, stopID).
		Scan(&st.StopID, &st.Name, &st.Lat, &st.Lon, &st.ParentStation, &st.LocationType, &st.PlatformCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: stop %q: %w", stopID, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: stop %q: %w", stopID, err)
	}
	return &st, nil
}

const routeColumns = `route_id, short_name, long_name, route_type, color, text_color, COALESCE(sort_order, 0)`

func (s *Store) Routes(ctx context.Context) ([]domain.Route, error) {
	verID, err := s.activeVersionID(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+routeColumns+`
		FROM gtfs_routes
		WHERE schedule_version_id = $1
		ORDER BY COALESCE(sort_order, 2147483647), route_id`, verID)
	if err != nil {
		return nil, fmt.Errorf("store: routes: %w", err)
	}
	return scanRoutes(rows, "routes")
}

func (s *Store) Route(ctx context.Context, routeID string) (*domain.Route, error) {
	verID, err := s.activeVersionID(ctx)
	if err != nil {
		return nil, err
	}
	var r domain.Route
	err = s.pool.QueryRow(ctx, `
		SELECT `+routeColumns+`
		FROM gtfs_routes
		WHERE schedule_version_id = $1 AND route_id = $2`, verID, routeID).
		Scan(&r.RouteID, &r.ShortName, &r.LongName, &r.RouteType, &r.Color, &r.TextColor, &r.SortOrder)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: route %q: %w", routeID, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: route %q: %w", routeID, err)
	}
	return &r, nil
}

// RoutesServingStop lists the routes with any scheduled stop at stopID. A
// parent station includes the routes serving its child platforms.
func (s *Store) RoutesServingStop(ctx context.Context, stopID string) ([]domain.Route, error) {
	verID, err := s.activeVersionID(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT r.route_id, r.short_name, r.long_name, r.route_type,
		       r.color, r.text_color, COALESCE(r.sort_order, 0)
		FROM gtfs_routes r
		JOIN gtfs_trips t
		  ON t.schedule_version_id = r.schedule_version_id AND t.route_id = r.route_id
		JOIN gtfs_stop_times st
		  ON st.schedule_version_id = t.schedule_version_id AND st.trip_id = t.trip_id
		WHERE r.schedule_version_id = $1
		  AND st.stop_id IN (
			SELECT $2::text
			UNION
			SELECT stop_id FROM gtfs_stops
			WHERE schedule_version_id = $1 AND parent_station = $2)
		ORDER BY COALESCE(r.sort_order, 0), r.route_id`, verID, stopID)
	if err != nil {
		return nil, fmt.Errorf("store: routes serving stop %q: %w", stopID, err)
	}
	return scanRoutes(rows, "routes serving stop")
}

func scanRoutes(rows pgx.Rows, op string) ([]domain.Route, error) {
	defer rows.Close()
	var out []domain.Route
	for rows.Next() {
		var r domain.Route
		if err := rows.Scan(&r.RouteID, &r.ShortName, &r.LongName, &r.RouteType, &r.Color, &r.TextColor, &r.SortOrder); err != nil {
			return nil, fmt.Errorf("store: %s: %w", op, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: %s: %w", op, err)
	}
	return out, nil
}

// stopFamilyFilter matches rollup rows for the stop itself or, when it is a
// parent station, any of its child platforms. $1 = stop id, $2 = version id.
const stopFamilyFilter = `(stop_id = $1 OR stop_id IN (
	SELECT stop_id FROM gtfs_stops
	WHERE schedule_version_id = $2 AND parent_station = $1))`

// rollupVersionID is the active version for expanding parent stations in
// rollup reads; rollup rows themselves are version-independent, so a missing
// version just means no expansion.
func (s *Store) rollupVersionID(ctx context.Context) (int64, error) {
	verID, err := s.activeVersionID(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return 0, nil
	}
	return verID, err
}

// StopHourly returns rollup_stop_hour rows for a stop across every route and
// direction, filtered to n >= domain.MinObservations.
func (s *Store) StopHourly(ctx context.Context, stopID string) ([]domain.HourlyStat, error) {
	verID, err := s.rollupVersionID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT route_id, stop_id, direction_id, hour_of_week, n,
		       p50_delay_secs, p90_delay_secs, late5_pct, early_pct
		FROM rollup_stop_hour
		WHERE `+stopFamilyFilter+` AND n >= $3
		ORDER BY route_id, direction_id, hour_of_week`, stopID, verID, domain.MinObservations)
	if err != nil {
		return nil, fmt.Errorf("store: stop hourly %q: %w", stopID, err)
	}
	return scanHourly(rows, "stop hourly", true)
}

// StopDepartures returns rollup_departure rows for a stop, filtered to
// n >= domain.MinObservations.
func (s *Store) StopDepartures(ctx context.Context, stopID string) ([]domain.DepartureStat, error) {
	verID, err := s.rollupVersionID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT route_id, stop_id, direction_id, scheduled_secs, day_class, n,
		       p50_delay_secs, p90_delay_secs, late5_pct
		FROM rollup_departure
		WHERE `+stopFamilyFilter+` AND n >= $3
		ORDER BY route_id, direction_id, day_class, scheduled_secs`, stopID, verID, domain.MinObservations)
	if err != nil {
		return nil, fmt.Errorf("store: stop departures %q: %w", stopID, err)
	}
	defer rows.Close()
	var out []domain.DepartureStat
	for rows.Next() {
		var d domain.DepartureStat
		if err := rows.Scan(&d.RouteID, &d.StopID, &d.DirectionID, &d.ScheduledSecs, &d.DayClass,
			&d.N, &d.P50DelaySecs, &d.P90DelaySecs, &d.Late5Pct); err != nil {
			return nil, fmt.Errorf("store: stop departures %q: %w", stopID, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: stop departures %q: %w", stopID, err)
	}
	return out, nil
}

// RouteHourly returns rollup_route_hour rows for a route, filtered to
// n >= domain.MinObservations.
func (s *Store) RouteHourly(ctx context.Context, routeID string) ([]domain.HourlyStat, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT route_id, direction_id, hour_of_week, n,
		       p50_delay_secs, p90_delay_secs, late5_pct
		FROM rollup_route_hour
		WHERE route_id = $1 AND n >= $2
		ORDER BY direction_id, hour_of_week`, routeID, domain.MinObservations)
	if err != nil {
		return nil, fmt.Errorf("store: route hourly %q: %w", routeID, err)
	}
	return scanHourly(rows, "route hourly", false)
}

// scanHourly reads HourlyStat rows; stop-level rows carry stop_id and
// early_pct, route-level rows do not.
func scanHourly(rows pgx.Rows, op string, stopLevel bool) ([]domain.HourlyStat, error) {
	defer rows.Close()
	var out []domain.HourlyStat
	for rows.Next() {
		var h domain.HourlyStat
		var err error
		if stopLevel {
			err = rows.Scan(&h.RouteID, &h.StopID, &h.DirectionID, &h.HourOfWeek, &h.N,
				&h.P50DelaySecs, &h.P90DelaySecs, &h.Late5Pct, &h.EarlyPct)
		} else {
			err = rows.Scan(&h.RouteID, &h.DirectionID, &h.HourOfWeek, &h.N,
				&h.P50DelaySecs, &h.P90DelaySecs, &h.Late5Pct)
		}
		if err != nil {
			return nil, fmt.Errorf("store: %s: %w", op, err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: %s: %w", op, err)
	}
	return out, nil
}

// RecentStopEvents returns the latest finalized events at a stop, newest
// first by their (actual, else scheduled) time.
func (s *Store) RecentStopEvents(ctx context.Context, stopID string, limit int) ([]domain.StopEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT service_date, trip_id, start_time, stop_sequence, stop_id, route_id,
		       direction_id, schedule_version_id, vehicle_id, scheduled_arrival,
		       predicted_arrival, actual_arrival, delay_secs, final, skipped,
		       feed_timestamp, last_updated
		FROM stop_events
		WHERE stop_id = $1 AND final
		ORDER BY COALESCE(actual_arrival, scheduled_arrival) DESC NULLS LAST, service_date DESC, trip_id
		LIMIT $2`, stopID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent stop events %q: %w", stopID, err)
	}
	defer rows.Close()
	var out []domain.StopEvent
	for rows.Next() {
		var (
			ev                           domain.StopEvent
			serviceDate                  time.Time
			scheduled, predicted, actual *time.Time
			feedTS                       int64
		)
		if err := rows.Scan(&serviceDate, &ev.TripID, &ev.StartTime, &ev.StopSequence, &ev.StopID,
			&ev.RouteID, &ev.DirectionID, &ev.ScheduleVersionID, &ev.VehicleID,
			&scheduled, &predicted, &actual, &ev.DelaySecs, &ev.Final, &ev.Skipped,
			&feedTS, &ev.ObservedAt); err != nil {
			return nil, fmt.Errorf("store: recent stop events %q: %w", stopID, err)
		}
		ev.ServiceDate = serviceDateFromTime(serviceDate)
		ev.ScheduledArrival = timeOrZero(scheduled)
		ev.PredictedArrival = timeOrZero(predicted)
		ev.ActualArrival = timeOrZero(actual)
		ev.FeedTimestamp = uint64(feedTS)
		ev.ObservedAt = ev.ObservedAt.UTC()
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: recent stop events %q: %w", stopID, err)
	}
	return out, nil
}
