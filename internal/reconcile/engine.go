// Package reconcile is the core of RideWatch: it turns decoded GTFS-Realtime
// snapshots into idempotent StopEvent upserts and vehicle-position history,
// and maintains the in-memory live state (vehicles, upcoming arrivals, feed
// ages) served to the map and the push evaluator.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
	"github.com/kevinkiyosepyo/ridewatch/internal/metrics"
)

// vehicleTTL is how long a live vehicle stays on the map without a fresh position.
const vehicleTTL = 3 * time.Minute

// Options configures the engine.
type Options struct {
	// FinalizeGrace is how long after an event's predicted (or scheduled)
	// time passes before it is finalized in the store and aged out of the
	// in-memory live state.
	FinalizeGrace time.Duration
	// Now overrides the clock; nil means time.Now. Injectable for tests.
	Now func() time.Time
}

// Engine reconciles realtime snapshots against the active static schedule.
// Process, Sweep, and the LiveSource getters are safe to call concurrently.
type Engine struct {
	sched  domain.ScheduleRepo
	events domain.EventStore
	opts   Options

	cache *tripCache

	tzMu sync.Mutex
	tz   map[string]*time.Location

	mu         sync.Mutex
	vehicles   map[string]liveEntry
	liveEvents map[domain.EventKey]domain.StopEvent
	feedHeader map[domain.Feed]time.Time
}

// liveEntry is one vehicle on the live map, enriched from the schedule.
type liveEntry struct {
	vp        domain.VehiclePosition
	headsign  string
	updatedAt time.Time
}

var _ domain.LiveSource = (*Engine)(nil)

// NewEngine builds an engine over the given schedule reader and event writer.
func NewEngine(sched domain.ScheduleRepo, events domain.EventStore, opts Options) *Engine {
	return &Engine{
		sched:      sched,
		events:     events,
		opts:       opts,
		cache:      newTripCache(tripCacheMax),
		tz:         make(map[string]*time.Location),
		vehicles:   make(map[string]liveEntry),
		liveEvents: make(map[domain.EventKey]domain.StopEvent),
		feedHeader: make(map[domain.Feed]time.Time),
	}
}

func (e *Engine) now() time.Time {
	if e.opts.Now != nil {
		return e.opts.Now()
	}
	return time.Now()
}

// Process handles one snapshot: resolve trips against the active schedule
// version, build StopEvent upserts / VehiclePosition rows, apply them via the
// event store, and refresh the in-memory live state.
func (e *Engine) Process(ctx context.Context, snap *domain.Snapshot) error {
	if snap == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := e.now()

	var ver *domain.ScheduleVersion
	var loc *time.Location
	v, err := e.sched.ActiveVersion(ctx)
	switch {
	case err == nil:
		l, lerr := e.location(v.AgencyTZ)
		if lerr != nil {
			return lerr
		}
		ver, loc = &v, l
		e.cache.setVersion(v.ID)
	case errors.Is(err, domain.ErrNotFound):
		// No static schedule loaded yet: trips count as unmatched and
		// vehicles stay live; Process must not fail.
	default:
		return fmt.Errorf("active schedule version: %w", err)
	}

	switch snap.Feed {
	case domain.FeedTripUpdates:
		if err := e.processTripUpdates(ctx, snap, ver, loc, now); err != nil {
			return err
		}
	case domain.FeedVehiclePositions:
		if err := e.processVehiclePositions(ctx, snap, ver); err != nil {
			return err
		}
	}

	header := snap.PolledAt
	if snap.FeedTimestamp > 0 {
		header = time.Unix(int64(snap.FeedTimestamp), 0)
	}
	metrics.IngestLag.WithLabelValues(string(snap.Feed)).Observe(now.Sub(header).Seconds())
	e.mu.Lock()
	if cur, ok := e.feedHeader[snap.Feed]; !ok || header.After(cur) {
		e.feedHeader[snap.Feed] = header
	}
	e.mu.Unlock()
	return nil
}

// Sweep runs store finalization and ages out live vehicles and in-memory
// events. Call every ~30s.
func (e *Engine) Sweep(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := e.now()
	grace := e.opts.FinalizeGrace

	e.mu.Lock()
	for id, le := range e.vehicles {
		if now.Sub(le.updatedAt) > vehicleTTL {
			delete(e.vehicles, id)
		}
	}
	metrics.ActiveVehicles.Set(float64(len(e.vehicles)))
	for k, ev := range e.liveEvents {
		ref := ev.PredictedArrival
		if ref.IsZero() {
			ref = ev.ScheduledArrival
		}
		if ref.IsZero() {
			ref = ev.ObservedAt
		}
		if now.Sub(ref) > grace {
			delete(e.liveEvents, k)
		}
	}
	e.mu.Unlock()

	finalized, err := e.events.FinalizeDue(ctx, now, grace)
	if err != nil {
		return fmt.Errorf("finalize due: %w", err)
	}
	metrics.StopEventsFinalized.Add(float64(finalized))
	return nil
}

// location returns a cached *time.Location for an agency timezone name.
func (e *Engine) location(name string) (*time.Location, error) {
	e.tzMu.Lock()
	defer e.tzMu.Unlock()
	if loc, ok := e.tz[name]; ok {
		return loc, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("load agency tz %q: %w", name, err)
	}
	e.tz[name] = loc
	return loc, nil
}

// tripSchedule resolves a trip through the bounded per-version cache,
// including negative entries (ADDED trips are re-polled every cycle).
func (e *Engine) tripSchedule(ctx context.Context, versionID int64, tripID string) (*domain.TripSchedule, error) {
	if ts, ok := e.cache.get(versionID, tripID); ok {
		if ts == nil {
			return nil, domain.ErrNotFound
		}
		return ts, nil
	}
	ts, err := e.sched.TripSchedule(ctx, versionID, tripID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			e.cache.put(versionID, tripID, nil)
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("trip schedule %q: %w", tripID, err)
	}
	e.cache.put(versionID, tripID, ts)
	return ts, nil
}

// LiveVehicles implements domain.LiveSource. It returns copies; the delay on
// each vehicle is that of its trip's next upcoming stop event, when known.
func (e *Engine) LiveVehicles() []domain.LiveVehicle {
	now := e.now()
	e.mu.Lock()
	type upcoming struct {
		at    time.Time
		delay *int
	}
	byTrip := make(map[string]upcoming)
	for _, ev := range e.liveEvents {
		if ev.Final || ev.Skipped || ev.PredictedArrival.IsZero() || ev.PredictedArrival.Before(now) {
			continue
		}
		if cur, ok := byTrip[ev.TripID]; !ok || ev.PredictedArrival.Before(cur.at) {
			byTrip[ev.TripID] = upcoming{ev.PredictedArrival, ev.DelaySecs}
		}
	}
	out := make([]domain.LiveVehicle, 0, len(e.vehicles))
	for _, le := range e.vehicles {
		lv := domain.LiveVehicle{
			VehiclePosition: le.vp,
			Headsign:        le.headsign,
			UpdatedAt:       le.updatedAt,
		}
		if le.vp.TripID != "" {
			if up, ok := byTrip[le.vp.TripID]; ok && up.delay != nil {
				d := *up.delay
				lv.DelaySecs = &d
			}
		}
		out = append(out, lv)
	}
	e.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].VehicleID < out[j].VehicleID })
	return out
}

// UpcomingAtStop implements domain.LiveSource: non-final events at the stop
// with a prediction within [now, now+horizon], soonest first, as copies.
func (e *Engine) UpcomingAtStop(stopID string, horizon time.Duration) []domain.StopEvent {
	now := e.now()
	end := now.Add(horizon)
	e.mu.Lock()
	var out []domain.StopEvent
	for _, ev := range e.liveEvents {
		if ev.StopID != stopID || ev.Final || ev.Skipped || ev.PredictedArrival.IsZero() {
			continue
		}
		if ev.PredictedArrival.Before(now) || ev.PredictedArrival.After(end) {
			continue
		}
		out = append(out, copyEvent(ev))
	}
	e.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].PredictedArrival.Equal(out[j].PredictedArrival) {
			return out[i].PredictedArrival.Before(out[j].PredictedArrival)
		}
		return out[i].TripID < out[j].TripID
	})
	return out
}

// FeedAge implements domain.LiveSource: time since the newest header
// timestamp of a successfully processed snapshot for the feed.
func (e *Engine) FeedAge(feed domain.Feed) (time.Duration, bool) {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	h, ok := e.feedHeader[feed]
	if !ok {
		return 0, false
	}
	return now.Sub(h), true
}

// copyEvent deep-copies a StopEvent so callers never share internal pointers.
func copyEvent(ev domain.StopEvent) domain.StopEvent {
	if ev.DelaySecs != nil {
		d := *ev.DelaySecs
		ev.DelaySecs = &d
	}
	return ev
}
