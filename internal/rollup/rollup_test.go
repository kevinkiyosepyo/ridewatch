package rollup

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

func TestRunRejectsBadTimezone(t *testing.T) {
	// pool is nil on purpose: validation must happen before any SQL runs.
	for _, tz := range []string{"", "Not/AZone", "America/Nowhere"} {
		if err := Run(context.Background(), nil, tz, 6); err == nil {
			t.Errorf("Run with timezone %q: expected error, got nil", tz)
		}
	}
}

// --- DB-backed test -------------------------------------------------------

func newTestPool(t *testing.T) *pgxpool.Pool {
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
	schema := fmt.Sprintf("rollup_test_%d", time.Now().UnixNano())
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
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
	migration, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", "0001_init.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	// Zero-arg Exec uses the simple protocol, so the multi-statement
	// migration file runs as-is inside the test schema.
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	return pool
}

type seedEvent struct {
	serviceDate string // YYYYMMDD
	tripID      string
	stopID      string
	routeID     string
	directionID int16
	schedSecs   int  // GTFS seconds of service day; -1 = scheduled_arrival NULL
	delay       *int // nil = delay_secs NULL
	final       bool
	skipped     bool
}

func insertEvents(t *testing.T, pool *pgxpool.Pool, loc *time.Location, events []seedEvent) {
	t.Helper()
	ctx := context.Background()
	for _, e := range events {
		var sched any
		if e.schedSecs >= 0 {
			ts, err := domain.ScheduledTime(e.serviceDate, e.schedSecs, loc)
			if err != nil {
				t.Fatalf("ScheduledTime(%s, %d): %v", e.serviceDate, e.schedSecs, err)
			}
			sched = ts
		}
		y, m, d, err := domain.ParseServiceDate(e.serviceDate)
		if err != nil {
			t.Fatalf("ParseServiceDate(%s): %v", e.serviceDate, err)
		}
		svcDate := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		_, err = pool.Exec(ctx, `INSERT INTO stop_events
            (service_date, trip_id, start_time, stop_sequence, stop_id, route_id, direction_id,
             schedule_version_id, vehicle_id, scheduled_arrival, delay_secs, final, skipped,
             feed_timestamp, first_seen, last_updated)
            VALUES ($1, $2, '', 1, $3, $4, $5, 1, '', $6, $7, $8, $9, 1, now(), now())`,
			svcDate, e.tripID, e.stopID, e.routeID, e.directionID, sched, e.delay, e.final, e.skipped)
		if err != nil {
			t.Fatalf("seed %s/%s: %v", e.serviceDate, e.tripID, err)
		}
	}
}

type agg struct {
	n                int
	p50, p90         *int
	late5, early     float64
	winStart, winEnd time.Time
}

// loadAggs runs a query returning (key text, n, p50, p90, late5, early,
// window_start, window_end) and maps rows by key.
func loadAggs(t *testing.T, pool *pgxpool.Pool, query string) map[string]agg {
	t.Helper()
	rows, err := pool.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("query rollup rows: %v", err)
	}
	defer rows.Close()
	out := map[string]agg{}
	for rows.Next() {
		var key string
		var a agg
		if err := rows.Scan(&key, &a.n, &a.p50, &a.p90, &a.late5, &a.early, &a.winStart, &a.winEnd); err != nil {
			t.Fatalf("scan rollup row: %v", err)
		}
		out[key] = a
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rollup rows: %v", err)
	}
	return out
}

func checkAgg(t *testing.T, table string, got map[string]agg, key string, n, p50, p90 int, late5, early float64) {
	t.Helper()
	a, ok := got[key]
	if !ok {
		t.Errorf("%s: missing row %q", table, key)
		return
	}
	if a.n != n {
		t.Errorf("%s %q: n = %d, want %d", table, key, a.n, n)
	}
	if a.p50 == nil || *a.p50 != p50 {
		t.Errorf("%s %q: p50 = %v, want %d", table, key, fmtPtr(a.p50), p50)
	}
	if a.p90 == nil || *a.p90 != p90 {
		t.Errorf("%s %q: p90 = %v, want %d", table, key, fmtPtr(a.p90), p90)
	}
	if math.Abs(a.late5-late5) > 1e-4 {
		t.Errorf("%s %q: late5_pct = %v, want %v", table, key, a.late5, late5)
	}
	if math.Abs(a.early-early) > 1e-4 {
		t.Errorf("%s %q: early_pct = %v, want %v", table, key, a.early, early)
	}
}

func fmtPtr(p *int) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d", *p)
}

func ip(n int) *int { return &n }

func TestRunAgainstPostgres(t *testing.T) {
	pool := newTestPool(t)
	const tz = "America/New_York"
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Seed weeks straddle the 2026-03-08 US spring-forward transition:
	// the week of Mon 2026-03-02 (EST throughout) and the week of the
	// transition itself (Sun 2026-03-08 is the 23-hour day; 03-09/03-10 are
	// EDT). windowWeeks is computed below so the trailing window reaches them.
	const s0812 = 8*3600 + 12*60  // 29520
	const s0845 = 8*3600 + 45*60  // 31500
	const s1730 = 17*3600 + 30*60 // 63000
	const s0900 = 9 * 3600        // 32400

	now := time.Now().In(loc)
	yesterday := domain.CivilDate(now.AddDate(0, 0, -1), loc)
	tomorrow := domain.CivilDate(now.AddDate(0, 0, 1), loc) // beyond window_end even across a midnight race

	events := []seedEvent{
		// Group A: R1/S1/dir 0, scheduled 08:12 local.
		{"20260302", "TA1", "S1", "R1", 0, s0812, ip(60), true, false},  // Mon, EST
		{"20260303", "TA2", "S1", "R1", 0, s0812, ip(120), true, false}, // Tue, EST
		{"20260308", "TA3", "S1", "R1", 0, s0812, ip(300), true, false}, // Sun, DST transition day
		{"20260309", "TA4", "S1", "R1", 0, s0812, ip(600), true, false}, // Mon, EDT
		// Group B: R1/S2/dir 1, scheduled 17:30 local, both Tuesdays.
		{"20260303", "TB1", "S2", "R1", 1, s1730, ip(-120), true, false},
		{"20260310", "TB2", "S2", "R1", 1, s1730, ip(-30), true, false},
		// Group C: R1/S3/dir 0, Mon 08:45 local (same hour-of-week as group A Mondays).
		{"20260302", "TC1", "S3", "R1", 0, s0845, ip(900), true, false},
		// Group D: R2/S9/dir 0, yesterday 09:00 local — inside any window.
		{yesterday, "TD1", "S9", "R2", 0, s0900, ip(100), true, false},
		// Rows the filter must exclude (delay 9999 would corrupt group A if leaked).
		{"20260302", "TX1", "S1", "R1", 0, s0812, ip(9999), false, false}, // not final
		{"20260303", "TX2", "S1", "R1", 0, s0812, ip(9999), true, true},   // skipped
		{"20260302", "TX3", "S1", "R1", 0, s0812, nil, true, false},       // delay NULL
		{"20260303", "TX4", "S1", "R1", 0, -1, ip(9999), true, false},     // scheduled NULL
		{tomorrow, "TX5", "S1", "R1", 0, s0812, ip(9999), true, false},    // outside [start, end)
	}
	insertEvents(t, pool, loc, events)

	seedStart := time.Date(2026, 3, 2, 0, 0, 0, 0, loc)
	if time.Since(seedStart) < 0 {
		t.Fatalf("clock says now is before the fixed seed week %v", seedStart)
	}
	weeks := int(time.Since(seedStart).Hours()/(24*7)) + 2

	todayBefore := domain.CivilDate(time.Now(), loc)
	if err := Run(ctx, pool, tz, weeks); err != nil {
		t.Fatalf("Run: %v", err)
	}
	todayAfter := domain.CivilDate(time.Now(), loc)

	// Expectations for group D depend on which weekday "yesterday" is.
	schedD, err := domain.ScheduledTime(yesterday, s0900, loc)
	if err != nil {
		t.Fatal(err)
	}
	hwD := domain.HourOfWeek(schedD, loc)
	dcD, err := domain.DayClass(yesterday)
	if err != nil {
		t.Fatal(err)
	}

	// rollup_stop_hour ----------------------------------------------------
	stopHour := loadAggs(t, pool, `SELECT route_id || '|' || stop_id || '|' || direction_id || '|' || hour_of_week,
        n, p50_delay_secs, p90_delay_secs, late5_pct::float8, early_pct::float8, window_start, window_end
        FROM rollup_stop_hour`)
	if len(stopHour) != 6 {
		t.Errorf("rollup_stop_hour: %d rows, want 6 (%v)", len(stopHour), keys(stopHour))
	}
	// Mon 08:xx = hour 8; Tue 08:xx = 32; Sun 08:xx = 152; Tue 17:xx = 41.
	checkAgg(t, "stop_hour", stopHour, "R1|S1|0|8", 2, 330, 546, 0.5, 0)  // {60, 600}
	checkAgg(t, "stop_hour", stopHour, "R1|S1|0|32", 1, 120, 120, 0, 0)   // {120}
	checkAgg(t, "stop_hour", stopHour, "R1|S1|0|152", 1, 300, 300, 1, 0)  // {300} on the DST day
	checkAgg(t, "stop_hour", stopHour, "R1|S2|1|41", 2, -75, -39, 0, 0.5) // {-120, -30}
	checkAgg(t, "stop_hour", stopHour, "R1|S3|0|8", 1, 900, 900, 1, 0)    // {900}
	checkAgg(t, "stop_hour", stopHour, fmt.Sprintf("R2|S9|0|%d", hwD), 1, 100, 100, 0, 0)

	// Window bounds on a sampled row.
	if a, ok := stopHour["R1|S1|0|8"]; ok {
		gotEnd := a.winEnd.Format("2006-01-02")
		wantEnd1, _ := time.ParseInLocation("20060102", todayBefore, time.UTC)
		wantEnd2, _ := time.ParseInLocation("20060102", todayAfter, time.UTC)
		if gotEnd != wantEnd1.Format("2006-01-02") && gotEnd != wantEnd2.Format("2006-01-02") {
			t.Errorf("window_end = %s, want today in %s (%s)", gotEnd, tz, todayBefore)
		}
		if span := a.winEnd.Sub(a.winStart); span != time.Duration(weeks)*7*24*time.Hour {
			t.Errorf("window span = %v, want %d weeks", span, weeks)
		}
	}

	// rollup_departure ----------------------------------------------------
	dep := loadAggs(t, pool, `SELECT route_id || '|' || stop_id || '|' || direction_id || '|' || scheduled_secs || '|' || day_class,
        n, p50_delay_secs, p90_delay_secs, late5_pct::float8, 0::float8, window_start, window_end
        FROM rollup_departure`)
	if len(dep) != 5 {
		t.Errorf("rollup_departure: %d rows, want 5 (%v)", len(dep), keys(dep))
	}
	// The 08:12 stop must round-trip to 29520 on every service date,
	// including both days of the DST week — so all three weekday
	// observations {60, 120, 600} land in ONE group, and the transition-day
	// Sunday observation sits at the same scheduled_secs.
	checkAgg(t, "departure", dep, "R1|S1|0|29520|weekday", 3, 120, 504, 1.0/3.0, 0)
	checkAgg(t, "departure", dep, "R1|S1|0|29520|sunday", 1, 300, 300, 1, 0)
	checkAgg(t, "departure", dep, "R1|S2|1|63000|weekday", 2, -75, -39, 0, 0)
	checkAgg(t, "departure", dep, "R1|S3|0|31500|weekday", 1, 900, 900, 1, 0)
	checkAgg(t, "departure", dep, fmt.Sprintf("R2|S9|0|%d|%s", s0900, dcD), 1, 100, 100, 0, 0)

	var stray int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM rollup_departure WHERE stop_id = 'S1' AND scheduled_secs <> 29520`).
		Scan(&stray); err != nil {
		t.Fatal(err)
	}
	if stray != 0 {
		t.Errorf("scheduled_secs drifted off 29520 for %d S1 groups (DST anchoring broken)", stray)
	}

	// rollup_route_hour ---------------------------------------------------
	routeHour := loadAggs(t, pool, `SELECT route_id || '|' || direction_id || '|' || hour_of_week,
        n, p50_delay_secs, p90_delay_secs, late5_pct::float8, 0::float8, window_start, window_end
        FROM rollup_route_hour`)
	if len(routeHour) != 5 {
		t.Errorf("rollup_route_hour: %d rows, want 5 (%v)", len(routeHour), keys(routeHour))
	}
	checkAgg(t, "route_hour", routeHour, "R1|0|8", 3, 600, 840, 2.0/3.0, 0) // {60, 600, 900} across S1+S3
	checkAgg(t, "route_hour", routeHour, "R1|0|32", 1, 120, 120, 0, 0)
	checkAgg(t, "route_hour", routeHour, "R1|0|152", 1, 300, 300, 1, 0)
	checkAgg(t, "route_hour", routeHour, "R1|1|41", 2, -75, -39, 0, 0)
	checkAgg(t, "route_hour", routeHour, fmt.Sprintf("R2|0|%d", hwD), 1, 100, 100, 0, 0)

	// Re-run with windowWeeks <= 0: defaults to 6 weeks, and the DELETE-all
	// rewrite must drop the now-out-of-window March groups. Only meaningful
	// once the fixed seed weeks have aged out of a 6-week window.
	if time.Since(seedStart) > 8*7*24*time.Hour {
		if err := Run(ctx, pool, tz, 0); err != nil {
			t.Fatalf("Run(windowWeeks=0): %v", err)
		}
		stopHour = loadAggs(t, pool, `SELECT route_id || '|' || stop_id || '|' || direction_id || '|' || hour_of_week,
            n, p50_delay_secs, p90_delay_secs, late5_pct::float8, early_pct::float8, window_start, window_end
            FROM rollup_stop_hour`)
		if len(stopHour) != 1 {
			t.Errorf("after 6-week rerun: %d stop_hour rows, want 1 (%v)", len(stopHour), keys(stopHour))
		}
		for k, a := range stopHour {
			if span := a.winEnd.Sub(a.winStart); span != 6*7*24*time.Hour {
				t.Errorf("row %q: default window span = %v, want 42 days", k, span)
			}
		}
	}
}

func keys(m map[string]agg) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
