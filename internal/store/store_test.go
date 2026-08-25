package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// openTestStore connects to TEST_DATABASE_URL (skipping when unset) inside a
// throwaway schema, so tests are hermetic and can run against any Postgres.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB test")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	schema := fmt.Sprintf("store_test_%d", time.Now().UnixNano())
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		pool.Close()
	})
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	s := &Store{pool: pool, tripCache: make(map[string]*domain.TripSchedule)}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func intp(n int) *int { return &n }

func TestUpsertGuardIdempotency(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	sched := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	ev := domain.StopEvent{
		ServiceDate: "20260821", TripID: "t1", StopSequence: 1, StopID: "A",
		RouteID: "R1", DirectionID: 0, ScheduleVersionID: 1, VehicleID: "v1",
		ScheduledArrival: sched, PredictedArrival: sched.Add(5 * time.Minute),
		DelaySecs: intp(300), FeedTimestamp: 10, ObservedAt: sched,
	}
	apply := func(e domain.StopEvent) int {
		t.Helper()
		n, err := s.UpsertStopEvents(ctx, []domain.StopEvent{e})
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		return n
	}

	if got := apply(ev); got != 1 {
		t.Fatalf("initial insert applied %d, want 1", got)
	}
	if got := apply(ev); got != 0 {
		t.Errorf("same feed timestamp applied %d, want 0", got)
	}
	older := ev
	older.FeedTimestamp = 9
	older.PredictedArrival = sched.Add(time.Minute)
	if got := apply(older); got != 0 {
		t.Errorf("older feed timestamp applied %d, want 0", got)
	}
	newer := ev
	newer.FeedTimestamp = 11
	newer.PredictedArrival = sched.Add(7 * time.Minute)
	newer.DelaySecs = intp(420)
	if got := apply(newer); got != 1 {
		t.Errorf("newer feed timestamp applied %d, want 1", got)
	}

	// An event that never had a prediction must finalize without fabricating one.
	bare := ev
	bare.TripID = "t2"
	bare.PredictedArrival = time.Time{}
	bare.DelaySecs = nil
	if got := apply(bare); got != 1 {
		t.Fatalf("bare insert applied %d, want 1", got)
	}

	n, err := s.FinalizeDue(ctx, sched.Add(time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if n != 2 {
		t.Errorf("finalized %d, want 2", n)
	}

	var actual, predicted *time.Time
	var delay *int
	var updates int
	err = s.pool.QueryRow(ctx, `
		SELECT actual_arrival, predicted_arrival, delay_secs, update_count
		FROM stop_events WHERE trip_id = 't1'`).Scan(&actual, &predicted, &delay, &updates)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if actual == nil || !actual.Equal(sched.Add(7*time.Minute)) {
		t.Errorf("actual_arrival = %v, want frozen prediction %v", actual, sched.Add(7*time.Minute))
	}
	if delay == nil || *delay != 420 {
		t.Errorf("delay_secs = %v, want 420", delay)
	}
	if updates != 2 {
		t.Errorf("update_count = %d, want 2", updates)
	}
	var bareActual *time.Time
	var bareDelay *int
	if err := s.pool.QueryRow(ctx, `
		SELECT actual_arrival, delay_secs FROM stop_events WHERE trip_id = 't2'`).
		Scan(&bareActual, &bareDelay); err != nil {
		t.Fatalf("read back bare: %v", err)
	}
	if bareActual != nil || bareDelay != nil {
		t.Errorf("finalization fabricated data for a never-predicted event: actual=%v delay=%v", bareActual, bareDelay)
	}

	// Final rows never reopen, even for newer timestamps.
	locked := newer
	locked.FeedTimestamp = 99
	if got := apply(locked); got != 0 {
		t.Errorf("post-final upsert applied %d, want 0", got)
	}
}

// loadFixture streams a small schedule through the COPY loader and returns
// the committed version id.
func loadFixture(t *testing.T, s *Store, sha string) int64 {
	t.Helper()
	ctx := context.Background()
	load, err := s.NewScheduleLoad(ctx, sha, time.Now())
	if err != nil {
		t.Fatalf("new schedule load: %v", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			_ = load.Abort(ctx)
			t.Fatalf("load fixture: %v", err)
		}
	}
	must(load.Meta("v1", "America/New_York"))
	must(load.Route(domain.Route{RouteID: "R1", ShortName: "1", LongName: "Central - Harvard", RouteType: 3, SortOrder: 10}))
	must(load.Stop(domain.Stop{StopID: "place-x", Name: "Central Square", Lat: 42.365, Lon: -71.103, LocationType: 1}))
	must(load.Stop(domain.Stop{StopID: "x-1", Name: "Central Square - Platform 1", Lat: 42.3651, Lon: -71.1031, ParentStation: "place-x"}))
	must(load.Stop(domain.Stop{StopID: "y", Name: "Harvard Ave", Lat: 42.350, Lon: -71.130}))
	must(load.Stop(domain.Stop{StopID: "e-1", Name: "Central Square - Main Entrance", Lat: 42.3652, Lon: -71.1032, ParentStation: "place-x", LocationType: 2}))
	must(load.Trip(domain.Trip{TripID: "t1", RouteID: "R1", ServiceID: "wk", Headsign: "Harvard", DirectionID: 0}))
	must(load.StopTime(domain.StopTime{TripID: "t1", StopSequence: 1, StopID: "x-1", ArrivalSecs: 28800, DepartureSecs: 28800}))
	must(load.StopTime(domain.StopTime{TripID: "t1", StopSequence: 2, StopID: "y", ArrivalSecs: 29400, DepartureSecs: 29400}))
	must(load.Calendar(domain.CalendarEntry{ServiceID: "wk", Weekdays: [7]bool{true, true, true, true, true, false, false}, StartDate: "20260101", EndDate: "20261231"}))
	must(load.CalendarDate(domain.CalendarDate{ServiceID: "wk", Date: "20260704", ExceptionType: 2}))
	id, err := load.Commit(ctx)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

func TestScheduleLoadAndReferenceQueries(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := loadFixture(t, s, "sha-one")

	ver, err := s.ActiveVersion(ctx)
	if err != nil {
		t.Fatalf("active version: %v", err)
	}
	if ver.ID != id || ver.AgencyTZ != "America/New_York" || !ver.Active || ver.LoadedAt.IsZero() {
		t.Errorf("active version = %+v, want id %d, tz America/New_York, active, loaded", ver, id)
	}

	ts, err := s.TripSchedule(ctx, id, "t1")
	if err != nil {
		t.Fatalf("trip schedule: %v", err)
	}
	if ts.RouteID != "R1" || ts.Headsign != "Harvard" || len(ts.Stops) != 2 || ts.Stops[1].StopID != "y" {
		t.Errorf("trip schedule = %+v", ts)
	}
	if _, err := s.TripSchedule(ctx, id, "ghost"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown trip error = %v, want ErrNotFound", err)
	}

	stops, err := s.SearchStops(ctx, "central", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(stops) != 2 {
		t.Fatalf("search 'central' = %d stops (%+v), want station + platform (entrance excluded)", len(stops), stops)
	}
	if stops[0].StopID != "place-x" {
		t.Errorf("search order: first = %s, want the station place-x", stops[0].StopID)
	}
	if got, err := s.SearchStops(ctx, "%", 10); err != nil || len(got) != 0 {
		t.Errorf("wildcard search = %v, %v; want literal match (empty)", got, err)
	}

	boxed, err := s.StopsInBBox(ctx, 42.36, -71.11, 42.37, -71.10, 10)
	if err != nil {
		t.Fatalf("bbox: %v", err)
	}
	if len(boxed) != 2 {
		t.Errorf("bbox = %d stops (%+v), want station + platform", len(boxed), boxed)
	}

	for _, stopID := range []string{"x-1", "place-x", "y"} {
		routes, err := s.RoutesServingStop(ctx, stopID)
		if err != nil {
			t.Fatalf("routes serving %s: %v", stopID, err)
		}
		if len(routes) != 1 || routes[0].RouteID != "R1" {
			t.Errorf("routes serving %s = %+v, want [R1]", stopID, routes)
		}
	}

	if _, err := s.Stop(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing stop error = %v, want ErrNotFound", err)
	}
	if routes, err := s.Routes(ctx); err != nil || len(routes) != 1 {
		t.Errorf("routes = %v, %v; want [R1]", routes, err)
	}
	if _, err := s.Route(ctx, "zz"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing route error = %v, want ErrNotFound", err)
	}

	// Loading the identical zip again is a no-op.
	if _, err := s.NewScheduleLoad(ctx, "sha-one", time.Now()); !errors.Is(err, ErrVersionExists) {
		t.Errorf("reload same sha error = %v, want ErrVersionExists", err)
	}

	// A new publish becomes the single active version.
	id2 := loadFixture(t, s, "sha-two")
	ver2, err := s.ActiveVersion(ctx)
	if err != nil {
		t.Fatalf("active version after republish: %v", err)
	}
	if ver2.ID != id2 {
		t.Errorf("active version = %d, want the new %d", ver2.ID, id2)
	}
}

func TestRollupQueriesSuppressSmallN(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	loadFixture(t, s, "sha-one")

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	// Child-platform rows: one big enough to show, one suppressed.
	mustExec(`INSERT INTO rollup_stop_hour (route_id, stop_id, direction_id, hour_of_week, n,
		p50_delay_secs, p90_delay_secs, late5_pct, early_pct, window_start, window_end, computed_at)
		VALUES ('R1','x-1',0,32,15,120,600,0.38,0.02,'2026-07-01','2026-08-21',now()),
		       ('R1','x-1',0,33,5,60,300,0.10,0.00,'2026-07-01','2026-08-21',now())`)
	mustExec(`INSERT INTO rollup_departure (route_id, stop_id, direction_id, scheduled_secs, day_class, n,
		p50_delay_secs, p90_delay_secs, late5_pct, window_start, window_end, computed_at)
		VALUES ('R1','x-1',0,29520,'weekday',12,300,720,0.38,'2026-07-01','2026-08-21',now()),
		       ('R1','x-1',0,30120,'weekday',3,60,120,0.05,'2026-07-01','2026-08-21',now())`)
	mustExec(`INSERT INTO rollup_route_hour (route_id, direction_id, hour_of_week, n,
		p50_delay_secs, p90_delay_secs, late5_pct, window_start, window_end, computed_at)
		VALUES ('R1',0,32,40,90,420,0.21,'2026-07-01','2026-08-21',now()),
		       ('R1',0,34,2,10,20,0.00,'2026-07-01','2026-08-21',now())`)

	hourly, err := s.StopHourly(ctx, "x-1")
	if err != nil {
		t.Fatalf("stop hourly: %v", err)
	}
	if len(hourly) != 1 || hourly[0].N != 15 || *hourly[0].P50DelaySecs != 120 {
		t.Errorf("stop hourly = %+v, want only the n=15 row", hourly)
	}
	// The parent station sees its platform's rollups.
	viaParent, err := s.StopHourly(ctx, "place-x")
	if err != nil {
		t.Fatalf("stop hourly via parent: %v", err)
	}
	if len(viaParent) != 1 || viaParent[0].StopID != "x-1" {
		t.Errorf("stop hourly via parent = %+v, want the platform row", viaParent)
	}

	deps, err := s.StopDepartures(ctx, "x-1")
	if err != nil {
		t.Fatalf("departures: %v", err)
	}
	if len(deps) != 1 || deps[0].ScheduledSecs != 29520 || deps[0].DayClass != "weekday" {
		t.Errorf("departures = %+v, want only the n=12 row", deps)
	}

	route, err := s.RouteHourly(ctx, "R1")
	if err != nil {
		t.Fatalf("route hourly: %v", err)
	}
	if len(route) != 1 || route[0].N != 40 || route[0].StopID != "" {
		t.Errorf("route hourly = %+v, want only the n=40 row with empty stop_id", route)
	}
}

func TestRecentStopEvents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	mk := func(trip string, sched time.Time, ts uint64) domain.StopEvent {
		return domain.StopEvent{
			ServiceDate: "20260821", TripID: trip, StopSequence: 1, StopID: "A", RouteID: "R1",
			DirectionID: 0, ScheduleVersionID: 1, ScheduledArrival: sched,
			PredictedArrival: sched.Add(2 * time.Minute), DelaySecs: intp(120),
			FeedTimestamp: ts, ObservedAt: sched,
		}
	}
	events := []domain.StopEvent{
		mk("early", base, 1),
		mk("late", base.Add(30*time.Minute), 2),
		mk("open", base.Add(6*time.Hour), 3), // stays unfinalized
	}
	if _, err := s.UpsertStopEvents(ctx, events); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := s.FinalizeDue(ctx, base.Add(2*time.Hour), 10*time.Minute); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	got, err := s.RecentStopEvents(ctx, "A", 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("recent = %d events, want the 2 finalized", len(got))
	}
	if got[0].TripID != "late" || got[1].TripID != "early" {
		t.Errorf("order = %s, %s; want newest first", got[0].TripID, got[1].TripID)
	}
	if !got[0].Final || got[0].ActualArrival.IsZero() || got[0].ServiceDate != "20260821" {
		t.Errorf("finalized event mapped wrong: %+v", got[0])
	}
}

func TestSubscriptionLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	sub := domain.Subscription{
		Endpoint: "https://push.example/abc", P256dh: "p", Auth: "a",
		StopID: "x-1", RouteID: "R1", DirectionID: -1, ThresholdSecs: 300,
	}
	id, err := s.SaveSubscription(ctx, sub)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	// Re-subscribing the same endpoint updates the watch in place.
	sub.StopID, sub.ThresholdSecs = "y", 600
	id2, err := s.SaveSubscription(ctx, sub)
	if err != nil {
		t.Fatalf("re-save: %v", err)
	}
	if id2 != id {
		t.Errorf("re-save id = %d, want same row %d", id2, id)
	}
	subs, err := s.AllSubscriptions(ctx)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(subs) != 1 || subs[0].StopID != "y" || subs[0].ThresholdSecs != 600 {
		t.Errorf("subscriptions = %+v, want one updated row", subs)
	}

	key := domain.EventKey{ServiceDate: "20260821", TripID: "t1", StopSequence: 1}
	if fresh, err := s.MarkPushSent(ctx, id, key); err != nil || !fresh {
		t.Errorf("first mark = %v, %v; want fresh", fresh, err)
	}
	if fresh, err := s.MarkPushSent(ctx, id, key); err != nil || fresh {
		t.Errorf("second mark = %v, %v; want duplicate", fresh, err)
	}

	if err := s.RecordPushResult(ctx, id, false, false); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if err := s.RecordPushResult(ctx, id, true, false); err != nil {
		t.Fatalf("record success: %v", err)
	}
	subs, _ = s.AllSubscriptions(ctx)
	if len(subs) != 1 || subs[0].FailureCount != 0 {
		t.Errorf("after success: %+v, want failure_count reset", subs)
	}

	// A gone endpoint disappears (with its push_sent rows).
	if err := s.RecordPushResult(ctx, id, false, true); err != nil {
		t.Fatalf("record gone: %v", err)
	}
	if subs, _ := s.AllSubscriptions(ctx); len(subs) != 0 {
		t.Errorf("after gone: %+v, want none", subs)
	}
	if err := s.DeleteSubscription(ctx, sub.Endpoint); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("delete missing = %v, want ErrNotFound", err)
	}
}

func TestPartitionLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	old := now.AddDate(0, 0, -70) // ten weeks back
	if err := s.EnsurePartitions(ctx, old, now); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := s.EnsurePartitions(ctx, old, now); err != nil {
		t.Fatalf("ensure twice (idempotent): %v", err)
	}
	children, err := s.childPartitions(ctx, "stop_events")
	if err != nil {
		t.Fatalf("children: %v", err)
	}
	// 11 ISO weeks touched plus the default partition.
	if len(children) != 12 {
		t.Errorf("stop_events partitions = %d (%v), want 12", len(children), children)
	}

	if err := s.dropOldPartitions(ctx, map[string]int{"stop_events": 4, "vehicle_positions": 0}, now); err != nil {
		t.Fatalf("drop: %v", err)
	}
	kept, _ := s.childPartitions(ctx, "stop_events")
	for _, name := range kept {
		ws, ok := parsePartitionName("stop_events", name)
		if !ok {
			continue // default partition survives
		}
		if ws.AddDate(0, 0, 7).Before(now.AddDate(0, 0, -28)) {
			t.Errorf("partition %s should have been dropped", name)
		}
	}
	if vp, _ := s.childPartitions(ctx, "vehicle_positions"); len(vp) != 12 {
		t.Errorf("vehicle_positions (keep 0 = forever) = %d partitions, want 12 untouched", len(vp))
	}
}

func TestRawArchiveIndex(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if sha, err := s.LastSHA(ctx, domain.FeedTripUpdates); err != nil || sha != "" {
		t.Errorf("empty LastSHA = %q, %v; want empty", sha, err)
	}
	at := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if err := s.RecordRawPoll(ctx, domain.FeedTripUpdates, at, 0, "aaa", "trip_updates/x", 10, 5); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.RecordRawPoll(ctx, domain.FeedTripUpdates, at.Add(15*time.Second), 100, "bbb", "trip_updates/y", 11, 6); err != nil {
		t.Fatalf("record: %v", err)
	}
	if sha, err := s.LastSHA(ctx, domain.FeedTripUpdates); err != nil || sha != "bbb" {
		t.Errorf("LastSHA = %q, %v; want bbb", sha, err)
	}
	if sha, err := s.LastSHA(ctx, domain.FeedVehiclePositions); err != nil || sha != "" {
		t.Errorf("other feed LastSHA = %q, %v; want empty", sha, err)
	}
}
