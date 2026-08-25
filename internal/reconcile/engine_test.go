package reconcile

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
	"github.com/kevinkiyosepyo/ridewatch/internal/metrics"
)

// --- fakes ---

type fakeSched struct {
	version    domain.ScheduleVersion
	versionErr error
	trips      map[string]*domain.TripSchedule

	mu      sync.Mutex
	lookups int
}

func (f *fakeSched) ActiveVersion(context.Context) (domain.ScheduleVersion, error) {
	if f.versionErr != nil {
		return domain.ScheduleVersion{}, f.versionErr
	}
	return f.version, nil
}

func (f *fakeSched) TripSchedule(_ context.Context, _ int64, tripID string) (*domain.TripSchedule, error) {
	f.mu.Lock()
	f.lookups++
	f.mu.Unlock()
	if ts, ok := f.trips[tripID]; ok {
		return ts, nil
	}
	return nil, domain.ErrNotFound
}

type finalizeCall struct {
	now   time.Time
	grace time.Duration
}

type fakeEvents struct {
	mu        sync.Mutex
	upserts   [][]domain.StopEvent
	positions [][]domain.VehiclePosition
	finalized []finalizeCall
}

func (f *fakeEvents) UpsertStopEvents(_ context.Context, events []domain.StopEvent) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]domain.StopEvent, len(events))
	copy(cp, events)
	f.upserts = append(f.upserts, cp)
	return len(events), nil
}

func (f *fakeEvents) InsertVehiclePositions(_ context.Context, positions []domain.VehiclePosition) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]domain.VehiclePosition, len(positions))
	copy(cp, positions)
	f.positions = append(f.positions, cp)
	return len(positions), nil
}

func (f *fakeEvents) FinalizeDue(_ context.Context, now time.Time, grace time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finalized = append(f.finalized, finalizeCall{now, grace})
	return 0, nil
}

func (f *fakeEvents) all() []domain.StopEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.StopEvent
	for _, batch := range f.upserts {
		out = append(out, batch...)
	}
	return out
}

// --- fixtures ---

var ny = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return loc
}()

// at is 2026-08-21 (a Friday) h:m in the agency timezone.
func at(h, m int) time.Time { return time.Date(2026, 8, 21, h, m, 0, 0, ny) }

func testSched() *fakeSched {
	return &fakeSched{
		version: domain.ScheduleVersion{ID: 7, AgencyTZ: "America/New_York", Active: true},
		trips: map[string]*domain.TripSchedule{
			// 08:00 -> 08:10 -> 08:20
			"t1": {TripID: "t1", RouteID: "R1", ServiceID: "wk", DirectionID: 0, Headsign: "Harvard", Stops: []domain.ScheduledStop{
				{StopSequence: 1, StopID: "A", ArrivalSecs: 28800, DepartureSecs: 28800},
				{StopSequence: 2, StopID: "B", ArrivalSecs: 29400, DepartureSecs: 29400},
				{StopSequence: 3, StopID: "C", ArrivalSecs: 30000, DepartureSecs: 30000},
			}},
			// after-midnight trip: 24:30 -> 24:40
			"night": {TripID: "night", RouteID: "R9", DirectionID: 1, Stops: []domain.ScheduledStop{
				{StopSequence: 1, StopID: "N1", ArrivalSecs: 88200, DepartureSecs: 88200},
				{StopSequence: 2, StopID: "N2", ArrivalSecs: 88800, DepartureSecs: 88800},
			}},
		},
	}
}

func newTestEngine(sched domain.ScheduleRepo, ev domain.EventStore, now time.Time) (*Engine, *time.Time) {
	cur := now
	e := NewEngine(sched, ev, Options{FinalizeGrace: 10 * time.Minute, Now: func() time.Time { return cur }})
	return e, &cur
}

func tuSnapshot(polledAt time.Time, tus ...domain.TripUpdate) *domain.Snapshot {
	return &domain.Snapshot{
		Feed:          domain.FeedTripUpdates,
		PolledAt:      polledAt,
		FeedTimestamp: uint64(polledAt.Unix()),
		TripUpdates:   tus,
	}
}

func vpSnapshot(polledAt time.Time, vps ...domain.VehiclePosition) *domain.Snapshot {
	return &domain.Snapshot{
		Feed:          domain.FeedVehiclePositions,
		PolledAt:      polledAt,
		FeedTimestamp: uint64(polledAt.Unix()),
		Vehicles:      vps,
	}
}

// stu builds a StopTimeUpdate with the decoder's absent-field sentinels.
func stu(seq int, stopID string) domain.StopTimeUpdate {
	return domain.StopTimeUpdate{StopSequence: seq, StopID: stopID, Relationship: "SCHEDULED"}
}

func tu(tripID string, stus ...domain.StopTimeUpdate) domain.TripUpdate {
	return domain.TripUpdate{
		TripID:               tripID,
		DirectionID:          -1,
		ScheduleRelationship: "SCHEDULED",
		VehicleID:            "v1",
		StopTimeUpdates:      stus,
	}
}

func eventAt(t *testing.T, events []domain.StopEvent, seq int) domain.StopEvent {
	t.Helper()
	for _, ev := range events {
		if ev.StopSequence == seq {
			return ev
		}
	}
	t.Fatalf("no event with stop_sequence %d in %+v", seq, events)
	return domain.StopEvent{}
}

func wantDelay(t *testing.T, ev domain.StopEvent, want int) {
	t.Helper()
	if ev.DelaySecs == nil {
		t.Fatalf("stop %d: delay = nil, want %d", ev.StopSequence, want)
	}
	if *ev.DelaySecs != want {
		t.Errorf("stop %d: delay = %d, want %d", ev.StopSequence, *ev.DelaySecs, want)
	}
}

// --- rule 1: service date ---

func TestServiceDateFromStartDate(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	u := tu("t1", stu(1, "A"))
	u.StartDate = "20260820" // feed says yesterday; it wins over resolution
	u.StopTimeUpdates[0].ArrivalTime = at(8, 5).Unix()
	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), u)); err != nil {
		t.Fatal(err)
	}
	for _, got := range ev.all() {
		if got.ServiceDate != "20260820" {
			t.Errorf("service date = %q, want start_date 20260820", got.ServiceDate)
		}
	}
}

func TestServiceDateAfterMidnight(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	// 00:35 on Aug 22: the 24:30 trip belongs to Aug 21's service day.
	now := time.Date(2026, 8, 22, 0, 35, 0, 0, ny)
	e, _ := newTestEngine(sched, ev, now)

	u := tu("night", stu(1, "N1"))
	u.StopTimeUpdates[0].ArrivalTime = time.Date(2026, 8, 22, 0, 36, 0, 0, ny).Unix()
	if err := e.Process(context.Background(), tuSnapshot(now, u)); err != nil {
		t.Fatal(err)
	}

	events := ev.all()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	first := eventAt(t, events, 1)
	if first.ServiceDate != "20260821" {
		t.Errorf("service date = %q, want 20260821 (previous service day)", first.ServiceDate)
	}
	wantDelay(t, first, 360) // scheduled 00:30, predicted 00:36
	second := eventAt(t, events, 2)
	if second.ServiceDate != "20260821" {
		t.Errorf("propagated service date = %q, want 20260821", second.ServiceDate)
	}
	wantPred := time.Date(2026, 8, 22, 0, 46, 0, 0, ny)
	if !second.PredictedArrival.Equal(wantPred) {
		t.Errorf("propagated prediction = %v, want %v", second.PredictedArrival, wantPred)
	}
}

func TestServiceDateDaytimeResolvesToday(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	u := tu("t1", stu(1, "A"))
	u.StopTimeUpdates[0].ArrivalTime = at(8, 5).Unix()
	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), u)); err != nil {
		t.Fatal(err)
	}
	if got := eventAt(t, ev.all(), 1).ServiceDate; got != "20260821" {
		t.Errorf("service date = %q, want 20260821", got)
	}
}

// --- rule 2: delay derivation and propagation ---

func TestDelayPrefersExplicitArrival(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	s1 := stu(1, "A")
	s1.ArrivalTime = at(8, 5).Unix()   // wins
	s1.DepartureTime = at(8, 7).Unix() // ignored while arrival is present
	s1.ArrivalDelay, s1.ArrivalDelaySet = 999, true
	s2 := stu(2, "B")
	s2.DepartureTime = at(8, 13).Unix() // no arrival: departure time used
	s3 := stu(3, "C")
	s3.ArrivalDelay, s3.ArrivalDelaySet = 60, true // no times: delay applied to schedule

	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), tu("t1", s1, s2, s3))); err != nil {
		t.Fatal(err)
	}
	events := ev.all()
	wantDelay(t, eventAt(t, events, 1), 300)
	wantDelay(t, eventAt(t, events, 2), 180)
	e3 := eventAt(t, events, 3)
	wantDelay(t, e3, 60)
	if want := at(8, 21); !e3.PredictedArrival.Equal(want) {
		t.Errorf("stop 3 prediction = %v, want %v", e3.PredictedArrival, want)
	}
}

func TestDelayPropagatesToLaterStops(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	s1 := stu(1, "A")
	s1.ArrivalTime = at(8, 5).Unix() // +300s
	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), tu("t1", s1))); err != nil {
		t.Fatal(err)
	}
	events := ev.all()
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 (delay propagates to the whole rest of the trip)", len(events))
	}
	for seq, wantPred := range map[int]time.Time{2: at(8, 15), 3: at(8, 25)} {
		got := eventAt(t, events, seq)
		wantDelay(t, got, 300)
		if !got.PredictedArrival.Equal(wantPred) {
			t.Errorf("stop %d prediction = %v, want %v", seq, got.PredictedArrival, wantPred)
		}
	}
}

func TestTripLevelDelayCoversWholeTrip(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	u := tu("t1") // no stop_time_updates at all
	u.DelaySecs, u.DelaySet = 120, true
	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), u)); err != nil {
		t.Fatal(err)
	}
	events := ev.all()
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	for _, ev := range events {
		wantDelay(t, ev, 120)
	}
}

func TestStopsBeforeFirstUpdateEmitNothing(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 12))

	s2 := stu(2, "B")
	s2.ArrivalTime = at(8, 12).Unix()
	if err := e.Process(context.Background(), tuSnapshot(at(8, 12), tu("t1", s2))); err != nil {
		t.Fatal(err)
	}
	events := ev.all()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (stop 1 was already served)", len(events))
	}
	for _, got := range events {
		if got.StopSequence == 1 {
			t.Errorf("unexpected event for the already-served stop 1: %+v", got)
		}
	}
}

// --- rule 3: feed timestamp stamping ---

func TestFeedTimestampHeaderAndEntity(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	plain := tu("t1", stu(1, "A"))
	plain.StopTimeUpdates[0].ArrivalTime = at(8, 5).Unix()
	stamped := tu("night", stu(1, "N1"))
	stamped.StartDate = "20260821"
	stamped.StopTimeUpdates[0].ArrivalTime = at(8, 6).Unix()
	stamped.Timestamp = 12345

	snap := tuSnapshot(at(8, 1), plain, stamped)
	if err := e.Process(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	for _, got := range ev.all() {
		want := snap.FeedTimestamp
		if got.TripID == "night" {
			want = 12345
		}
		if got.FeedTimestamp != want {
			t.Errorf("trip %s: feed timestamp = %d, want %d", got.TripID, got.FeedTimestamp, want)
		}
	}
}

// --- rule 4: unmatched, canceled, skipped ---

func TestUnmatchedTripCountsAndEmitsNothing(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	before := testutil.ToFloat64(metrics.UnmatchedTrips)
	ghost := tu("ghost", stu(1, "X"))
	ghost.ScheduleRelationship = "ADDED"
	ghost.StopTimeUpdates[0].ArrivalTime = at(8, 5).Unix()
	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), ghost)); err != nil {
		t.Fatal(err)
	}
	if got := ev.all(); len(got) != 0 {
		t.Errorf("unmatched trip emitted %d events, want 0", len(got))
	}
	if got := testutil.ToFloat64(metrics.UnmatchedTrips) - before; got != 1 {
		t.Errorf("UnmatchedTrips delta = %v, want 1", got)
	}

	// The negative lookup is cached: a second poll costs no schedule round-trip.
	lookups := sched.lookups
	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), ghost)); err != nil {
		t.Fatal(err)
	}
	if sched.lookups != lookups {
		t.Errorf("second poll of an unknown trip hit the schedule repo (%d -> %d lookups)", lookups, sched.lookups)
	}
}

func TestNoScheduleLoadedYet(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	sched.versionErr = domain.ErrNotFound
	e, _ := newTestEngine(sched, ev, at(8, 1))

	before := testutil.ToFloat64(metrics.UnmatchedTrips)
	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), tu("t1", stu(1, "A")))); err != nil {
		t.Fatalf("Process must not fail without a schedule: %v", err)
	}
	if got := ev.all(); len(got) != 0 {
		t.Errorf("got %d events without a schedule, want 0", len(got))
	}
	if got := testutil.ToFloat64(metrics.UnmatchedTrips) - before; got != 1 {
		t.Errorf("UnmatchedTrips delta = %v, want 1", got)
	}

	// Vehicles still go live (and to history) without a schedule.
	vp := domain.VehiclePosition{VehicleID: "v9", TripID: "t1", Lat: 42.3, Lon: -71.1,
		Bearing: -1, SpeedMPS: -1, StopSequence: -1, Timestamp: uint64(at(8, 1).Unix())}
	if err := e.Process(context.Background(), vpSnapshot(at(8, 1), vp)); err != nil {
		t.Fatal(err)
	}
	if got := e.LiveVehicles(); len(got) != 1 {
		t.Errorf("live vehicles = %d, want 1", len(got))
	}
	if len(ev.positions) != 1 {
		t.Errorf("vehicle positions batches = %d, want 1", len(ev.positions))
	}
}

func TestCanceledTripEmitsSkippedEvents(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	u := tu("t1")
	u.ScheduleRelationship = "CANCELED"
	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), u)); err != nil {
		t.Fatal(err)
	}
	events := ev.all()
	if len(events) != 3 {
		t.Fatalf("got %d events, want one Skipped per scheduled stop (3)", len(events))
	}
	for _, got := range events {
		if !got.Skipped {
			t.Errorf("stop %d: skipped = false, want true", got.StopSequence)
		}
		if !got.PredictedArrival.IsZero() || got.DelaySecs != nil {
			t.Errorf("stop %d: canceled stop carries a prediction (%v, %v)", got.StopSequence, got.PredictedArrival, got.DelaySecs)
		}
		if got.ScheduledArrival.IsZero() {
			t.Errorf("stop %d: scheduled arrival missing", got.StopSequence)
		}
	}
}

func TestSkippedStopEmitsAndDelayKeepsPropagating(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	s1 := stu(1, "A")
	s1.ArrivalTime = at(8, 5).Unix() // +300s
	s2 := stu(2, "B")
	s2.Relationship = "SKIPPED"
	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), tu("t1", s1, s2))); err != nil {
		t.Fatal(err)
	}
	events := ev.all()
	skipped := eventAt(t, events, 2)
	if !skipped.Skipped || !skipped.PredictedArrival.IsZero() || skipped.DelaySecs != nil {
		t.Errorf("skipped stop event wrong: %+v", skipped)
	}
	after := eventAt(t, events, 3)
	wantDelay(t, after, 300) // the vehicle is still late past the skip
}

func TestNoDataStopsPropagation(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	s1 := stu(1, "A")
	s1.ArrivalTime = at(8, 5).Unix()
	s2 := stu(2, "B")
	s2.Relationship = "NO_DATA"
	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), tu("t1", s1, s2))); err != nil {
		t.Fatal(err)
	}
	events := ev.all()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (NO_DATA halts propagation)", len(events))
	}
	if events[0].StopSequence != 1 {
		t.Errorf("event at stop %d, want 1", events[0].StopSequence)
	}
}

// --- rule 5: vanishing vehicles ---

func TestVehicleAgesOutAndReturns(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, now := newTestEngine(sched, ev, at(8, 1))

	vp := domain.VehiclePosition{VehicleID: "v1", TripID: "t1", RouteID: "R1",
		Lat: 42.3, Lon: -71.1, Bearing: -1, SpeedMPS: -1, StopSequence: -1,
		Timestamp: uint64(at(8, 1).Unix())}
	if err := e.Process(context.Background(), vpSnapshot(at(8, 1), vp)); err != nil {
		t.Fatal(err)
	}
	if got := e.LiveVehicles(); len(got) != 1 {
		t.Fatalf("live vehicles = %d, want 1", len(got))
	} else if got[0].Headsign != "Harvard" {
		t.Errorf("headsign = %q, want enriched %q", got[0].Headsign, "Harvard")
	}

	*now = at(8, 5) // 4 minutes later: over the 3-minute TTL
	if err := e.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.LiveVehicles(); len(got) != 0 {
		t.Errorf("live vehicles after TTL = %d, want 0", len(got))
	}
	if len(ev.finalized) != 1 || ev.finalized[0].grace != 10*time.Minute {
		t.Errorf("FinalizeDue calls = %+v, want one with the configured grace", ev.finalized)
	}

	// A vehicle that reappears simply resumes.
	vp.Timestamp = uint64(at(8, 6).Unix())
	if err := e.Process(context.Background(), vpSnapshot(at(8, 6), vp)); err != nil {
		t.Fatal(err)
	}
	if got := e.LiveVehicles(); len(got) != 1 {
		t.Errorf("live vehicles after return = %d, want 1", len(got))
	}
}

// --- rule 6: stop resolution ---

func TestStopResolutionBySequenceOnlyAndByIDOnly(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	bySeq := stu(2, "") // sequence only
	bySeq.ArrivalTime = at(8, 12).Unix()
	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), tu("t1", bySeq))); err != nil {
		t.Fatal(err)
	}
	got := eventAt(t, ev.all(), 2)
	if got.StopID != "B" {
		t.Errorf("sequence-only update resolved stop_id %q, want B", got.StopID)
	}
	wantDelay(t, got, 120)

	ev2 := &fakeEvents{}
	e2, _ := newTestEngine(testSched(), ev2, at(8, 1))
	byID := stu(-1, "B") // stop_id only
	byID.ArrivalTime = at(8, 12).Unix()
	if err := e2.Process(context.Background(), tuSnapshot(at(8, 1), tu("t1", byID))); err != nil {
		t.Fatal(err)
	}
	got2 := eventAt(t, ev2.all(), 2)
	if got2.StopID != "B" {
		t.Errorf("stop-id-only update resolved stop_id %q, want B", got2.StopID)
	}
	wantDelay(t, got2, 120)
}

// --- live state ---

func TestUpcomingAtStopFiltersAndSorts(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	sched.trips["t2"] = &domain.TripSchedule{TripID: "t2", RouteID: "R1", DirectionID: 0, Stops: []domain.ScheduledStop{
		{StopSequence: 1, StopID: "B", ArrivalSecs: 29520, DepartureSecs: 29520}, // 08:12
	}}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	s1 := stu(1, "A")
	s1.ArrivalTime = at(8, 5).Unix() // t1: B predicted 08:15 via propagation
	u2 := tu("t2", stu(1, "B"))
	u2.StopTimeUpdates[0].ArrivalTime = at(8, 13).Unix()
	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), tu("t1", s1), u2)); err != nil {
		t.Fatal(err)
	}

	up := e.UpcomingAtStop("B", time.Hour)
	if len(up) != 2 {
		t.Fatalf("upcoming at B = %d, want 2", len(up))
	}
	if up[0].TripID != "t2" || up[1].TripID != "t1" {
		t.Errorf("order = %s, %s; want t2 (08:13) before t1 (08:15)", up[0].TripID, up[1].TripID)
	}
	if got := e.UpcomingAtStop("B", 5*time.Minute); len(got) != 0 {
		t.Errorf("upcoming within 5m = %d, want 0", len(got))
	}

	// The vehicle on t1 picks up the delay of its next upcoming stop.
	vp := domain.VehiclePosition{VehicleID: "v1", TripID: "t1", RouteID: "R1",
		Lat: 42, Lon: -71, Bearing: -1, SpeedMPS: -1, StopSequence: -1,
		Timestamp: uint64(at(8, 1).Unix())}
	if err := e.Process(context.Background(), vpSnapshot(at(8, 1), vp)); err != nil {
		t.Fatal(err)
	}
	lv := e.LiveVehicles()
	if len(lv) != 1 || lv[0].DelaySecs == nil || *lv[0].DelaySecs != 300 {
		t.Errorf("live vehicle delay = %+v, want 300", lv)
	}
}

func TestOutOfOrderApplyNeverRegressesLiveState(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	newer := tuSnapshot(at(8, 1))
	nu := tu("t1", stu(1, "A"))
	nu.StopTimeUpdates[0].ArrivalTime = at(8, 6).Unix()
	newer.TripUpdates = []domain.TripUpdate{nu}
	if err := e.Process(context.Background(), newer); err != nil {
		t.Fatal(err)
	}

	older := tuSnapshot(at(7, 59)) // older header timestamp
	ou := tu("t1", stu(1, "A"))
	ou.StopTimeUpdates[0].ArrivalTime = at(8, 3).Unix()
	older.TripUpdates = []domain.TripUpdate{ou}
	if err := e.Process(context.Background(), older); err != nil {
		t.Fatal(err)
	}

	up := e.UpcomingAtStop("A", time.Hour)
	if len(up) != 1 {
		t.Fatalf("upcoming at A = %d, want 1", len(up))
	}
	if want := at(8, 6); !up[0].PredictedArrival.Equal(want) {
		t.Errorf("prediction = %v, want the newer %v (older apply must not regress)", up[0].PredictedArrival, want)
	}
}

func TestFeedAge(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, _ := newTestEngine(sched, ev, at(8, 1))

	if _, ok := e.FeedAge(domain.FeedTripUpdates); ok {
		t.Error("FeedAge before any snapshot: ok = true, want false")
	}
	snap := tuSnapshot(at(8, 0))
	if err := e.Process(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	age, ok := e.FeedAge(domain.FeedTripUpdates)
	if !ok || age != time.Minute {
		t.Errorf("FeedAge = %v, %v; want 1m, true", age, ok)
	}
}

// Sweep ages finalized-by-time events out of the live view.
func TestSweepAgesOutStaleLiveEvents(t *testing.T) {
	sched, ev := testSched(), &fakeEvents{}
	e, now := newTestEngine(sched, ev, at(8, 1))

	s1 := stu(1, "A")
	s1.ArrivalTime = at(8, 5).Unix()
	if err := e.Process(context.Background(), tuSnapshot(at(8, 1), tu("t1", s1))); err != nil {
		t.Fatal(err)
	}
	if got := e.UpcomingAtStop("A", time.Hour); len(got) != 1 {
		t.Fatalf("upcoming = %d, want 1", len(got))
	}

	*now = at(8, 20) // past 08:05 + 10m grace
	if err := e.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	*now = at(8, 1) // anchor the UpcomingAtStop window back before the predictions
	if got := e.UpcomingAtStop("A", time.Hour); len(got) != 0 {
		t.Errorf("upcoming after sweep = %d, want 0", len(got))
	}
}
