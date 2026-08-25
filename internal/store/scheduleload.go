package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// ErrVersionExists is returned by NewScheduleLoad when the static feed with
// this sha256 has already been fully loaded.
var ErrVersionExists = errors.New("schedule version already loaded")

// copyBatchSize is how many buffered rows trigger a CopyFrom flush.
const copyBatchSize = 5000

// ScheduleLoad streams one static GTFS feed into Postgres: rows are buffered
// per table and flushed with COPY inside a single transaction. It implements
// domain.ScheduleWriter (whose methods carry no context, so the load context
// given to NewScheduleLoad is captured for the duration of the load).
type ScheduleLoad struct {
	store    *Store
	ctx      context.Context
	tx       pgx.Tx
	id       int64
	finished bool

	routes    [][]any
	stops     [][]any
	trips     [][]any
	stopTimes [][]any
	cal       [][]any
	calDates  [][]any
}

var _ domain.ScheduleWriter = (*ScheduleLoad)(nil)

// NewScheduleLoad inserts a schedule_versions row for sha256 and opens the
// load transaction. A version already loaded (loaded_at set) returns
// ErrVersionExists; a leftover row with NULL loaded_at (crashed load) is
// deleted and the load retried. The version row itself commits immediately so
// a crash mid-load leaves exactly that detectable state.
func (s *Store) NewScheduleLoad(ctx context.Context, sha256 string, fetchedAt time.Time) (*ScheduleLoad, error) {
	var (
		existingID int64
		loadedAt   *time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, loaded_at FROM schedule_versions WHERE sha256 = $1`, sha256).
		Scan(&existingID, &loadedAt)
	switch {
	case err == nil:
		if loadedAt != nil {
			return nil, fmt.Errorf("store: sha256 %s: %w", sha256, ErrVersionExists)
		}
		// Crashed earlier load: remove it (cascades to any partial rows) and retry.
		if _, err := s.pool.Exec(ctx, `DELETE FROM schedule_versions WHERE id = $1`, existingID); err != nil {
			return nil, fmt.Errorf("store: delete crashed schedule load %d: %w", existingID, err)
		}
	case errors.Is(err, pgx.ErrNoRows):
		// fresh sha
	default:
		return nil, fmt.Errorf("store: look up schedule version: %w", err)
	}

	var id int64
	err = s.pool.QueryRow(ctx,
		`INSERT INTO schedule_versions (sha256, agency_tz, fetched_at) VALUES ($1, '', $2) RETURNING id`,
		sha256, fetchedAt.UTC()).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("store: insert schedule version: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin schedule load: %w", err)
	}
	return &ScheduleLoad{store: s, ctx: ctx, tx: tx, id: id}, nil
}

// Meta updates the version row with feed_version and agency_tz.
func (l *ScheduleLoad) Meta(feedVersion, agencyTZ string) error {
	_, err := l.tx.Exec(l.ctx,
		`UPDATE schedule_versions SET feed_version = $1, agency_tz = $2 WHERE id = $3`,
		feedVersion, agencyTZ, l.id)
	if err != nil {
		return fmt.Errorf("store: update schedule version meta: %w", err)
	}
	return nil
}

func (l *ScheduleLoad) Route(r domain.Route) error {
	l.routes = append(l.routes, []any{
		l.id, r.RouteID, r.ShortName, r.LongName, r.RouteType, r.Color, r.TextColor, r.SortOrder,
	})
	return l.maybeFlush(&l.routes, tableRoutes)
}

func (l *ScheduleLoad) Stop(st domain.Stop) error {
	l.stops = append(l.stops, []any{
		l.id, st.StopID, st.Name, st.Lat, st.Lon, st.ParentStation, st.LocationType, st.PlatformCode,
	})
	return l.maybeFlush(&l.stops, tableStops)
}

func (l *ScheduleLoad) Trip(t domain.Trip) error {
	l.trips = append(l.trips, []any{
		l.id, t.TripID, t.RouteID, t.ServiceID, t.Headsign, t.DirectionID, t.ShapeID,
	})
	return l.maybeFlush(&l.trips, tableTrips)
}

func (l *ScheduleLoad) StopTime(st domain.StopTime) error {
	l.stopTimes = append(l.stopTimes, []any{
		l.id, st.TripID, st.StopSequence, st.StopID, st.ArrivalSecs, st.DepartureSecs,
	})
	return l.maybeFlush(&l.stopTimes, tableStopTimes)
}

func (l *ScheduleLoad) Calendar(c domain.CalendarEntry) error {
	start, err := dateFromServiceDate(c.StartDate)
	if err != nil {
		return err
	}
	end, err := dateFromServiceDate(c.EndDate)
	if err != nil {
		return err
	}
	l.cal = append(l.cal, []any{
		l.id, c.ServiceID,
		c.Weekdays[0], c.Weekdays[1], c.Weekdays[2], c.Weekdays[3],
		c.Weekdays[4], c.Weekdays[5], c.Weekdays[6],
		start, end,
	})
	return l.maybeFlush(&l.cal, tableCalendar)
}

func (l *ScheduleLoad) CalendarDate(c domain.CalendarDate) error {
	d, err := dateFromServiceDate(c.Date)
	if err != nil {
		return err
	}
	l.calDates = append(l.calDates, []any{l.id, c.ServiceID, d, c.ExceptionType})
	return l.maybeFlush(&l.calDates, tableCalendarDates)
}

// Commit flushes every buffer, stamps loaded_at, atomically moves the active
// flag to this version, and commits. Returns the version id.
func (l *ScheduleLoad) Commit(ctx context.Context) (int64, error) {
	if l.finished {
		return 0, errors.New("store: schedule load already finished")
	}
	for _, t := range copyTables {
		if err := l.flush(t.buf(l), t); err != nil {
			return 0, err
		}
	}
	if _, err := l.tx.Exec(ctx,
		`UPDATE schedule_versions SET loaded_at = now() WHERE id = $1`, l.id); err != nil {
		return 0, fmt.Errorf("store: set loaded_at: %w", err)
	}
	if _, err := l.tx.Exec(ctx,
		`UPDATE schedule_versions SET active = false WHERE active`); err != nil {
		return 0, fmt.Errorf("store: deactivate previous version: %w", err)
	}
	if _, err := l.tx.Exec(ctx,
		`UPDATE schedule_versions SET active = true WHERE id = $1`, l.id); err != nil {
		return 0, fmt.Errorf("store: activate version %d: %w", l.id, err)
	}
	if err := l.tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit schedule load: %w", err)
	}
	l.finished = true
	return l.id, nil
}

// Abort rolls the load transaction back and removes the version row.
func (l *ScheduleLoad) Abort(ctx context.Context) error {
	if l.finished {
		return nil
	}
	l.finished = true
	err := l.tx.Rollback(ctx)
	if errors.Is(err, pgx.ErrTxClosed) {
		err = nil
	}
	if _, delErr := l.store.pool.Exec(ctx,
		`DELETE FROM schedule_versions WHERE id = $1`, l.id); delErr != nil && err == nil {
		err = fmt.Errorf("store: delete aborted schedule version: %w", delErr)
	}
	return err
}

type copyTable struct {
	name string
	cols []string
	buf  func(*ScheduleLoad) *[][]any
}

var (
	tableRoutes = copyTable{"gtfs_routes",
		[]string{"schedule_version_id", "route_id", "short_name", "long_name", "route_type", "color", "text_color", "sort_order"},
		func(l *ScheduleLoad) *[][]any { return &l.routes }}
	tableStops = copyTable{"gtfs_stops",
		[]string{"schedule_version_id", "stop_id", "name", "lat", "lon", "parent_station", "location_type", "platform_code"},
		func(l *ScheduleLoad) *[][]any { return &l.stops }}
	tableTrips = copyTable{"gtfs_trips",
		[]string{"schedule_version_id", "trip_id", "route_id", "service_id", "headsign", "direction_id", "shape_id"},
		func(l *ScheduleLoad) *[][]any { return &l.trips }}
	tableStopTimes = copyTable{"gtfs_stop_times",
		[]string{"schedule_version_id", "trip_id", "stop_sequence", "stop_id", "arrival_secs", "departure_secs"},
		func(l *ScheduleLoad) *[][]any { return &l.stopTimes }}
	tableCalendar = copyTable{"gtfs_calendar",
		[]string{"schedule_version_id", "service_id", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday", "start_date", "end_date"},
		func(l *ScheduleLoad) *[][]any { return &l.cal }}
	tableCalendarDates = copyTable{"gtfs_calendar_dates",
		[]string{"schedule_version_id", "service_id", "date", "exception_type"},
		func(l *ScheduleLoad) *[][]any { return &l.calDates }}

	copyTables = []copyTable{tableRoutes, tableStops, tableTrips, tableStopTimes, tableCalendar, tableCalendarDates}
)

func (l *ScheduleLoad) maybeFlush(buf *[][]any, t copyTable) error {
	if len(*buf) < copyBatchSize {
		return nil
	}
	return l.flush(buf, t)
}

func (l *ScheduleLoad) flush(buf *[][]any, t copyTable) error {
	if len(*buf) == 0 {
		return nil
	}
	_, err := l.tx.CopyFrom(l.ctx, pgx.Identifier{t.name}, t.cols, pgx.CopyFromRows(*buf))
	if err != nil {
		return fmt.Errorf("store: copy into %s: %w", t.name, err)
	}
	*buf = (*buf)[:0]
	return nil
}
