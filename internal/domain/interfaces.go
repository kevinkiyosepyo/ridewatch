package domain

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by repositories when a row does not exist.
var ErrNotFound = errors.New("not found")

// ScheduleRepo is read access to the static schedule, used by reconciliation.
// Implemented by store (with an LRU cache in front of TripSchedule).
type ScheduleRepo interface {
	// ActiveVersion returns the currently active schedule version.
	// Returns ErrNotFound when no static schedule has been loaded yet.
	ActiveVersion(ctx context.Context) (ScheduleVersion, error)
	// TripSchedule returns the ordered stops of a trip under a version.
	// Returns ErrNotFound for unknown trips (e.g. ADDED trips).
	TripSchedule(ctx context.Context, versionID int64, tripID string) (*TripSchedule, error)
}

// EventStore is the write side applied after each reconciliation pass.
type EventStore interface {
	// UpsertStopEvents inserts or advances rows on their natural key.
	// A row only changes when the incoming FeedTimestamp is strictly newer,
	// and rows already final are never reopened — this is what makes both
	// repeated polls and full replays idempotent.
	UpsertStopEvents(ctx context.Context, events []StopEvent) (applied int, err error)
	// InsertVehiclePositions appends position history (dedup on (vehicle_id, ts)).
	InsertVehiclePositions(ctx context.Context, positions []VehiclePosition) (applied int, err error)
	// FinalizeDue marks rows final whose predicted (or scheduled) time passed
	// more than grace ago, freezing predicted_arrival into actual_arrival.
	FinalizeDue(ctx context.Context, now time.Time, grace time.Duration) (finalized int, err error)
}

// RawArchive records raw poll blobs (already written to disk) in the index.
type RawArchive interface {
	RecordRawPoll(ctx context.Context, feed Feed, polledAt time.Time, feedTS uint64, sha256, relPath string, size, entities int) error
	// LastSHA returns the sha256 of the most recent archived blob for a feed
	// ("" if none) so byte-identical responses are not re-archived.
	LastSHA(ctx context.Context, feed Feed) (string, error)
}

// ScheduleWriter receives one static GTFS feed's rows as they are parsed.
// Implemented by store.ScheduleLoad (which streams them into Postgres with
// COPY inside a single transaction); driven by gtfsstatic.ParseZip.
type ScheduleWriter interface {
	// Meta is called once, as soon as agency.txt / feed_info.txt are parsed.
	Meta(feedVersion, agencyTZ string) error
	Route(Route) error
	Stop(Stop) error
	Trip(Trip) error
	StopTime(StopTime) error
	Calendar(CalendarEntry) error
	CalendarDate(CalendarDate) error
}

// StopQueries is the read side used by the HTTP API. Implemented by store.
type StopQueries interface {
	SearchStops(ctx context.Context, q string, limit int) ([]Stop, error)
	StopsInBBox(ctx context.Context, minLat, minLon, maxLat, maxLon float64, limit int) ([]Stop, error)
	Stop(ctx context.Context, stopID string) (*Stop, error)
	Routes(ctx context.Context) ([]Route, error)
	Route(ctx context.Context, routeID string) (*Route, error)
	RoutesServingStop(ctx context.Context, stopID string) ([]Route, error)
	// StopHourly returns rollup_stop_hour rows for a stop (all routes/directions),
	// already filtered to n >= MinObservations.
	StopHourly(ctx context.Context, stopID string) ([]HourlyStat, error)
	StopDepartures(ctx context.Context, stopID string) ([]DepartureStat, error)
	RouteHourly(ctx context.Context, routeID string) ([]HourlyStat, error)
	// RecentStopEvents returns the latest finalized events at a stop, newest first.
	RecentStopEvents(ctx context.Context, stopID string, limit int) ([]StopEvent, error)
}

// SubscriptionStore persists Web Push subscriptions and alert dedup state.
type SubscriptionStore interface {
	SaveSubscription(ctx context.Context, s Subscription) (id int64, err error)
	DeleteSubscription(ctx context.Context, endpoint string) error
	AllSubscriptions(ctx context.Context) ([]Subscription, error)
	// MarkPushSent records intent to alert; returns false if this
	// (subscription, event) was already alerted — the caller must then skip it.
	MarkPushSent(ctx context.Context, subscriptionID int64, key EventKey) (fresh bool, err error)
	// RecordPushResult updates failure_count / last_success; implementations
	// delete subscriptions whose endpoint is gone (HTTP 404/410 ⇒ gone=true).
	RecordPushResult(ctx context.Context, subscriptionID int64, ok bool, gone bool) error
}

// LiveSource is the in-memory live state served to the map and used by the
// push evaluator. Implemented by the reconcile engine.
type LiveSource interface {
	// LiveVehicles returns the latest known position of every currently
	// visible vehicle (vehicles unseen for a few minutes age out).
	LiveVehicles() []LiveVehicle
	// UpcomingAtStop returns non-final events at a stop with predictions,
	// soonest first, within the given horizon.
	UpcomingAtStop(stopID string, horizon time.Duration) []StopEvent
	// FeedAge returns time since the last successful poll's header timestamp.
	FeedAge(feed Feed) (time.Duration, bool)
}
