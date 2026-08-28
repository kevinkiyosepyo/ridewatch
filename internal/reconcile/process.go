package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
	"github.com/kevinkiyosepyo/ridewatch/internal/metrics"
)

// processTripUpdates reconciles one trip-updates snapshot: every TripUpdate is
// matched against the active schedule, expanded into StopEvent upserts, applied
// via the event store, and mirrored into the in-memory live state.
func (e *Engine) processTripUpdates(ctx context.Context, snap *domain.Snapshot, ver *domain.ScheduleVersion, loc *time.Location, now time.Time) error {
	var events []domain.StopEvent
	for i := range snap.TripUpdates {
		tu := &snap.TripUpdates[i]
		if ver == nil {
			metrics.UnmatchedTrips.Inc()
			continue
		}
		evs, err := e.reconcileTrip(ctx, tu, snap, ver, loc)
		if err != nil {
			return err
		}
		events = append(events, evs...)
	}

	if len(events) > 0 {
		if _, err := e.events.UpsertStopEvents(ctx, events); err != nil {
			return fmt.Errorf("upsert stop events: %w", err)
		}
	}

	// Mirror into live state with the same newest-timestamp-wins rule as the
	// store, so out-of-order applies (replays) cannot regress what riders see.
	e.mu.Lock()
	for i := range events {
		ev := events[i]
		k := ev.Key()
		if cur, ok := e.liveEvents[k]; ok && cur.FeedTimestamp > ev.FeedTimestamp {
			continue
		}
		e.liveEvents[k] = ev
	}
	e.mu.Unlock()
	return nil
}

// reconcileTrip expands one TripUpdate into StopEvents against the trip's
// static schedule. Unmatched trips (ADDED or simply unknown) count toward
// metrics.UnmatchedTrips and produce nothing.
func (e *Engine) reconcileTrip(ctx context.Context, tu *domain.TripUpdate, snap *domain.Snapshot, ver *domain.ScheduleVersion, loc *time.Location) ([]domain.StopEvent, error) {
	ts, err := e.tripSchedule(ctx, ver.ID, tu.TripID)
	if errors.Is(err, domain.ErrNotFound) {
		metrics.UnmatchedTrips.Inc()
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	serviceDate, err := resolveServiceDate(tu, ts, loc, snap.PolledAt)
	if err != nil {
		// A trip whose service date cannot be pinned down cannot be compared
		// to the schedule; treat it like an unmatched trip.
		metrics.UnmatchedTrips.Inc()
		return nil, nil
	}

	// Rule 3: stamp the header timestamp, or the per-entity one when present.
	feedTS := snap.FeedTimestamp
	if tu.Timestamp > 0 {
		feedTS = tu.Timestamp
	}

	base := domain.StopEvent{
		ServiceDate:       serviceDate,
		TripID:            tu.TripID,
		StartTime:         tu.StartTime,
		RouteID:           ts.RouteID,
		DirectionID:       ts.DirectionID,
		ScheduleVersionID: ver.ID,
		VehicleID:         tu.VehicleID,
		Headsign:          ts.Headsign,
		FeedTimestamp:     feedTS,
		ObservedAt:        snap.PolledAt,
	}
	if base.RouteID == "" {
		base.RouteID = tu.RouteID
	}
	if base.DirectionID < 0 {
		base.DirectionID = tu.DirectionID
	}

	// Rule 4: a canceled trip emits every stop as Skipped with no prediction.
	if tu.ScheduleRelationship == "CANCELED" {
		out := make([]domain.StopEvent, 0, len(ts.Stops))
		for _, stop := range ts.Stops {
			ev := base
			ev.StopSequence = stop.StopSequence
			ev.StopID = stop.StopID
			ev.Skipped = true
			ev.ScheduledArrival = scheduledOrZero(serviceDate, stop.BestSecs(), loc)
			out = append(out, ev)
		}
		return out, nil
	}

	// Rule 6: match updates to scheduled stops by stop_sequence when given,
	// else by stop_id.
	matches := make(map[int]*domain.StopTimeUpdate, len(tu.StopTimeUpdates))
	for i := range tu.StopTimeUpdates {
		stu := &tu.StopTimeUpdates[i]
		if idx := resolveStopIndex(ts, stu); idx >= 0 {
			matches[idx] = stu
		}
	}

	// Predictions start at the first stop the feed says anything about; stops
	// before it have already been served. A trip-level delay with no per-stop
	// updates covers the whole trip.
	startIdx := len(ts.Stops)
	for idx := range matches {
		if idx < startIdx {
			startIdx = idx
		}
	}
	propDelay, propSet := int(0), false
	if tu.DelaySet {
		propDelay, propSet = int(tu.DelaySecs), true
		if len(matches) == 0 {
			startIdx = 0
		}
	}

	var out []domain.StopEvent
	for i := startIdx; i < len(ts.Stops); i++ {
		stop := &ts.Stops[i]
		ev := base
		ev.StopSequence = stop.StopSequence
		ev.StopID = stop.StopID
		ev.ScheduledArrival = scheduledOrZero(serviceDate, stop.BestSecs(), loc)

		stu := matches[i]
		switch {
		case stu != nil && stu.Relationship == "SKIPPED":
			// Rule 4: a skipped stop is an event with no prediction. The
			// running delay still describes the vehicle, so it keeps
			// propagating past the skip.
			ev.Skipped = true
			out = append(out, ev)
			continue
		case stu != nil && stu.Relationship == "NO_DATA":
			// No realtime information at this stop, and the previous delay
			// stops propagating here (it was only good "until the next
			// stop_time_update").
			propSet = false
			continue
		case stu != nil:
			pred, delay, ok := predictionFromUpdate(stu, tu, stop, serviceDate, loc)
			if !ok {
				continue
			}
			ev.PredictedArrival = pred
			ev.DelaySecs = delay
			if delay != nil {
				propDelay, propSet = *delay, true
			}
			out = append(out, ev)
		default:
			// Rule 2: the last known delay propagates to later stops with no
			// update of their own.
			if !propSet || ev.ScheduledArrival.IsZero() {
				continue
			}
			d := propDelay
			ev.PredictedArrival = ev.ScheduledArrival.Add(time.Duration(d) * time.Second)
			ev.DelaySecs = &d
			out = append(out, ev)
		}
	}
	return out, nil
}

// predictionFromUpdate resolves rule 2 for one stop with its own update:
// prefer the explicit arrival time, else the departure time, else a per-stop
// or trip-level delay applied to the scheduled time. ok is false when the
// update carries nothing usable.
func predictionFromUpdate(stu *domain.StopTimeUpdate, tu *domain.TripUpdate, stop *domain.ScheduledStop, serviceDate string, loc *time.Location) (pred time.Time, delay *int, ok bool) {
	sched := scheduledOrZero(serviceDate, stop.BestSecs(), loc)

	if t := firstPositive(stu.ArrivalTime, stu.DepartureTime); t > 0 {
		pred = time.Unix(t, 0).In(loc)
		if !sched.IsZero() {
			d := int(pred.Sub(sched) / time.Second)
			delay = &d
		}
		return pred, delay, true
	}

	var d int
	switch {
	case stu.ArrivalDelaySet:
		d = int(stu.ArrivalDelay)
	case stu.DepartureDelaySet:
		d = int(stu.DepartureDelay)
	case tu.DelaySet:
		d = int(tu.DelaySecs)
	default:
		return time.Time{}, nil, false
	}
	if sched.IsZero() {
		// A delay with no scheduled time to apply it to predicts nothing.
		return time.Time{}, nil, false
	}
	return sched.Add(time.Duration(d) * time.Second), &d, true
}

// resolveServiceDate implements rule 1: the feed's start_date wins when
// present; otherwise try yesterday and today in the agency timezone and pick
// the date whose scheduled time for the trip's first predicted stop lands
// closest to the prediction (or to the poll time for delay-only updates) —
// this is what puts an after-midnight ">24:00" trip on the previous service
// day.
func resolveServiceDate(tu *domain.TripUpdate, ts *domain.TripSchedule, loc *time.Location, polledAt time.Time) (string, error) {
	if tu.StartDate != "" {
		if _, _, _, err := domain.ParseServiceDate(tu.StartDate); err != nil {
			return "", err
		}
		return tu.StartDate, nil
	}

	refSecs, refInstant := -1, polledAt
	for i := range tu.StopTimeUpdates {
		stu := &tu.StopTimeUpdates[i]
		idx := resolveStopIndex(ts, stu)
		if idx < 0 || ts.Stops[idx].BestSecs() < 0 {
			continue
		}
		if t := firstPositive(stu.ArrivalTime, stu.DepartureTime); t > 0 {
			refSecs, refInstant = ts.Stops[idx].BestSecs(), time.Unix(t, 0)
			break
		}
		if refSecs < 0 {
			refSecs = ts.Stops[idx].BestSecs() // fallback anchor: first matched stop vs poll time
		}
	}
	if refSecs < 0 {
		for _, stop := range ts.Stops {
			if stop.BestSecs() >= 0 {
				refSecs = stop.BestSecs()
				break
			}
		}
	}
	if refSecs < 0 {
		return "", fmt.Errorf("trip %q has no scheduled times", ts.TripID)
	}

	today := domain.CivilDate(refInstant, loc)
	yesterday, err := domain.AddDays(today, -1)
	if err != nil {
		return "", err
	}
	best, bestDiff := "", time.Duration(0)
	for _, d := range []string{yesterday, today} {
		st, err := domain.ScheduledTime(d, refSecs, loc)
		if err != nil {
			return "", err
		}
		diff := st.Sub(refInstant)
		if diff < 0 {
			diff = -diff
		}
		if best == "" || diff < bestDiff {
			best, bestDiff = d, diff
		}
	}
	return best, nil
}

// resolveStopIndex finds the scheduled stop an update refers to: by
// stop_sequence when the update carries one, else by stop_id (rule 6).
// Returns -1 when nothing matches.
func resolveStopIndex(ts *domain.TripSchedule, stu *domain.StopTimeUpdate) int {
	if stu.StopSequence >= 0 {
		for i := range ts.Stops {
			if ts.Stops[i].StopSequence == stu.StopSequence {
				return i
			}
		}
		return -1
	}
	if stu.StopID != "" {
		for i := range ts.Stops {
			if ts.Stops[i].StopID == stu.StopID {
				return i
			}
		}
	}
	return -1
}

// scheduledOrZero resolves GTFS seconds to an instant, mapping "no scheduled
// time" (-1) to the zero time.
func scheduledOrZero(serviceDate string, secs int, loc *time.Location) time.Time {
	if secs < 0 {
		return time.Time{}
	}
	t, err := domain.ScheduledTime(serviceDate, secs, loc)
	if err != nil {
		return time.Time{}
	}
	return t
}

func firstPositive(a, b int64) int64 {
	if a > 0 {
		return a
	}
	return b
}

// processVehiclePositions appends position history and refreshes the live
// vehicle map, enriching each vehicle's headsign (and missing route) from the
// static schedule when its trip is known.
func (e *Engine) processVehiclePositions(ctx context.Context, snap *domain.Snapshot, ver *domain.ScheduleVersion) error {
	if len(snap.Vehicles) > 0 {
		if _, err := e.events.InsertVehiclePositions(ctx, snap.Vehicles); err != nil {
			return fmt.Errorf("insert vehicle positions: %w", err)
		}
	}

	entries := make([]liveEntry, 0, len(snap.Vehicles))
	for i := range snap.Vehicles {
		vp := snap.Vehicles[i]
		if vp.VehicleID == "" {
			continue
		}
		headsign := ""
		if ver != nil && vp.TripID != "" {
			// Enrichment is cosmetic: on any lookup failure (including a trip
			// not in the schedule — rule 4 keeps the vehicle on the map) the
			// position is served as-is.
			if ts, err := e.tripSchedule(ctx, ver.ID, vp.TripID); err == nil {
				headsign = ts.Headsign
				if vp.RouteID == "" {
					vp.RouteID = ts.RouteID
				}
			}
		}
		updated := snap.PolledAt
		if vp.Timestamp > 0 {
			updated = time.Unix(int64(vp.Timestamp), 0)
		}
		entries = append(entries, liveEntry{vp: vp, headsign: headsign, updatedAt: updated})
	}

	e.mu.Lock()
	for _, le := range entries {
		if cur, ok := e.vehicles[le.vp.VehicleID]; ok && cur.updatedAt.After(le.updatedAt) {
			continue // an out-of-order apply never regresses a vehicle
		}
		e.vehicles[le.vp.VehicleID] = le
	}
	metrics.ActiveVehicles.Set(float64(len(e.vehicles)))
	e.mu.Unlock()
	return nil
}
