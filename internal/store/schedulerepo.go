package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// tripCacheCap bounds the in-process TripSchedule cache.
const tripCacheCap = 20000

// ActiveVersion returns the active schedule version, or domain.ErrNotFound
// when no static schedule has been loaded yet.
func (s *Store) ActiveVersion(ctx context.Context) (domain.ScheduleVersion, error) {
	var (
		v        domain.ScheduleVersion
		loadedAt *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, sha256, feed_version, agency_tz, fetched_at, loaded_at, active
		FROM schedule_versions WHERE active`).
		Scan(&v.ID, &v.SHA256, &v.FeedVersion, &v.AgencyTZ, &v.FetchedAt, &loadedAt, &v.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ScheduleVersion{}, fmt.Errorf("store: active schedule version: %w", domain.ErrNotFound)
	}
	if err != nil {
		return domain.ScheduleVersion{}, fmt.Errorf("store: active schedule version: %w", err)
	}
	v.FetchedAt = v.FetchedAt.UTC()
	v.LoadedAt = timeOrZero(loadedAt)
	return v, nil
}

// TripSchedule returns the ordered stops of a trip under a version, through a
// bounded cache. The cache holds entries for one version at a time and is
// fully cleared when a different version is requested; ErrNotFound results
// are not cached.
func (s *Store) TripSchedule(ctx context.Context, versionID int64, tripID string) (*domain.TripSchedule, error) {
	s.cacheMu.Lock()
	if s.tripCacheVersion != versionID {
		s.tripCache = make(map[string]*domain.TripSchedule)
		s.tripCacheVersion = versionID
	}
	if ts, ok := s.tripCache[tripID]; ok {
		s.cacheMu.Unlock()
		return ts, nil
	}
	s.cacheMu.Unlock()

	ts, err := s.loadTripSchedule(ctx, versionID, tripID)
	if err != nil {
		return nil, err
	}

	s.cacheMu.Lock()
	if s.tripCacheVersion == versionID {
		if len(s.tripCache) >= tripCacheCap {
			for k := range s.tripCache { // evict one arbitrary entry
				delete(s.tripCache, k)
				break
			}
		}
		s.tripCache[tripID] = ts
	}
	s.cacheMu.Unlock()
	return ts, nil
}

func (s *Store) loadTripSchedule(ctx context.Context, versionID int64, tripID string) (*domain.TripSchedule, error) {
	ts := &domain.TripSchedule{TripID: tripID}
	err := s.pool.QueryRow(ctx, `
		SELECT route_id, service_id, headsign, direction_id
		FROM gtfs_trips WHERE schedule_version_id = $1 AND trip_id = $2`,
		versionID, tripID).
		Scan(&ts.RouteID, &ts.ServiceID, &ts.Headsign, &ts.DirectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: trip %q version %d: %w", tripID, versionID, domain.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: trip %q version %d: %w", tripID, versionID, err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT stop_sequence, stop_id, arrival_secs, departure_secs
		FROM gtfs_stop_times
		WHERE schedule_version_id = $1 AND trip_id = $2
		ORDER BY stop_sequence`, versionID, tripID)
	if err != nil {
		return nil, fmt.Errorf("store: stop times for trip %q: %w", tripID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var st domain.ScheduledStop
		if err := rows.Scan(&st.StopSequence, &st.StopID, &st.ArrivalSecs, &st.DepartureSecs); err != nil {
			return nil, fmt.Errorf("store: stop times for trip %q: %w", tripID, err)
		}
		ts.Stops = append(ts.Stops, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: stop times for trip %q: %w", tripID, err)
	}
	return ts, nil
}
