// Package domain holds the shared vocabulary of RideWatch: the types that flow
// between ingest, reconciliation, storage, rollups, the API, and push alerts,
// plus the GTFS time rules. It depends only on the standard library.
package domain

import "time"

// Feed identifies one of the two GTFS-Realtime feeds we poll.
type Feed string

const (
	FeedVehiclePositions Feed = "vehicle_positions"
	FeedTripUpdates      Feed = "trip_updates"
)

// Snapshot is one decoded poll of one feed. Exactly one of TripUpdates or
// Vehicles is populated depending on Feed.
type Snapshot struct {
	Feed          Feed
	PolledAt      time.Time
	FeedTimestamp uint64 // FeedHeader.timestamp, unix seconds; 0 if absent
	RawSHA256     string
	RawPath       string // relative path of the archived blob, "" when not archived
	TripUpdates   []TripUpdate
	Vehicles      []VehiclePosition
}

// TripUpdate is a decoded GTFS-RT TripUpdate entity, flattened to stdlib types
// so nothing downstream imports protobuf.
type TripUpdate struct {
	TripID               string
	RouteID              string
	DirectionID          int16  // -1 = unknown
	StartDate            string // YYYYMMDD; "" if absent
	StartTime            string // HH:MM:SS; "" if absent (non-frequency trips)
	ScheduleRelationship string // "SCHEDULED", "ADDED", "CANCELED", "UNSCHEDULED", ...
	VehicleID            string
	Timestamp            uint64 // per-entity timestamp; 0 if absent
	DelaySecs            int32  // trip-level delay; only meaningful if DelaySet
	DelaySet             bool
	StopTimeUpdates      []StopTimeUpdate
}

// StopTimeUpdate is one stop's prediction within a TripUpdate.
type StopTimeUpdate struct {
	StopSequence      int    // -1 if absent (resolve via StopID against the schedule)
	StopID            string // "" if absent (resolve via StopSequence)
	ArrivalTime       int64  // unix seconds; 0 if absent
	ArrivalDelay      int32
	ArrivalDelaySet   bool
	DepartureTime     int64
	DepartureDelay    int32
	DepartureDelaySet bool
	Relationship      string // "SCHEDULED", "SKIPPED", "NO_DATA"
}

// VehiclePosition is a decoded GTFS-RT VehiclePosition entity.
type VehiclePosition struct {
	VehicleID    string
	Label        string
	TripID       string
	RouteID      string
	StartDate    string // YYYYMMDD; "" if absent
	Lat, Lon     float64
	Bearing      float32 // -1 = unknown
	SpeedMPS     float32 // -1 = unknown
	StopSequence int     // -1 = unknown
	StopID       string
	Status       string // "IN_TRANSIT_TO", "STOPPED_AT", "INCOMING_AT", ""
	Timestamp    uint64 // unix seconds; 0 if absent
}

// StopEvent is one (trip instance, stop) observation — the natural-key unit of
// the derived dataset. Natural key: (ServiceDate, TripID, StartTime, StopSequence).
type StopEvent struct {
	ServiceDate       string // YYYYMMDD in the agency timezone
	TripID            string
	StartTime         string // "" for schedule-based trips
	StopSequence      int
	StopID            string
	RouteID           string
	DirectionID       int16
	ScheduleVersionID int64
	VehicleID         string
	ScheduledArrival  time.Time // zero = unknown (trip not in static schedule)
	PredictedArrival  time.Time // zero = no prediction
	ActualArrival     time.Time // zero until finalized
	DelaySecs         *int      // best-known arrival minus scheduled; nil = unknown
	Final             bool
	Skipped           bool
	FeedTimestamp     uint64 // guard: an upsert only wins if its FeedTimestamp is newer
	ObservedAt        time.Time
}

// EventKey is the natural key of a StopEvent, used for push-alert dedup.
type EventKey struct {
	ServiceDate  string
	TripID       string
	StartTime    string
	StopSequence int
}

func (e *StopEvent) Key() EventKey {
	return EventKey{e.ServiceDate, e.TripID, e.StartTime, e.StopSequence}
}

// ScheduleVersion is one snapshot of the static GTFS feed.
type ScheduleVersion struct {
	ID          int64
	SHA256      string
	FeedVersion string
	AgencyTZ    string
	FetchedAt   time.Time
	LoadedAt    time.Time
	Active      bool
}

// TripSchedule is the static schedule for one trip under one version.
type TripSchedule struct {
	TripID      string
	RouteID     string
	ServiceID   string
	DirectionID int16
	Headsign    string
	Stops       []ScheduledStop // ordered by StopSequence ascending
}

// ScheduledStop is one scheduled stop within a trip. Times are GTFS seconds
// after noon-minus-12h on the service date; -1 = not specified.
type ScheduledStop struct {
	StopSequence  int
	StopID        string
	ArrivalSecs   int
	DepartureSecs int
}

// BestSecs returns the best available scheduled time for delay comparison:
// arrival if present, else departure, else -1.
func (s ScheduledStop) BestSecs() int {
	if s.ArrivalSecs >= 0 {
		return s.ArrivalSecs
	}
	return s.DepartureSecs
}

// Static reference rows used by the loader and read queries.

type Route struct {
	RouteID   string
	ShortName string
	LongName  string
	RouteType int
	Color     string
	TextColor string
	SortOrder int
}

type Stop struct {
	StopID        string
	Name          string
	Lat, Lon      float64
	ParentStation string
	LocationType  int
	PlatformCode  string
}

type Trip struct {
	TripID      string
	RouteID     string
	ServiceID   string
	Headsign    string
	DirectionID int16
	ShapeID     string
}

type StopTime struct {
	TripID        string
	StopSequence  int
	StopID        string
	ArrivalSecs   int // -1 = not specified
	DepartureSecs int
}

type CalendarEntry struct {
	ServiceID string
	Weekdays  [7]bool // index 0 = Monday ... 6 = Sunday
	StartDate string  // YYYYMMDD
	EndDate   string
}

type CalendarDate struct {
	ServiceID     string
	Date          string // YYYYMMDD
	ExceptionType int    // 1 = added, 2 = removed
}

// LiveVehicle is what the map serves: the latest position enriched with
// schedule context and the current delay estimate.
type LiveVehicle struct {
	VehiclePosition
	RouteShortName string
	Headsign       string
	DelaySecs      *int // nil = unknown
	UpdatedAt      time.Time
}

// Subscription is a Web Push subscription following one stop.
type Subscription struct {
	ID            int64
	Endpoint      string
	P256dh        string
	Auth          string
	StopID        string
	RouteID       string // "" = any route serving the stop
	DirectionID   int16  // -1 = any direction
	ThresholdSecs int
	CreatedAt     time.Time
	FailureCount  int
}

// Rollup rows served by the API.

type HourlyStat struct {
	RouteID      string
	StopID       string // "" on route-level rollups
	DirectionID  int16
	HourOfWeek   int // 0 = Monday 00:00 ... 167 = Sunday 23:00, agency tz
	N            int
	P50DelaySecs *int
	P90DelaySecs *int
	Late5Pct     float32
	EarlyPct     float32
}

type DepartureStat struct {
	RouteID       string
	StopID        string
	DirectionID   int16
	ScheduledSecs int    // GTFS time-of-day seconds of the scheduled departure
	DayClass      string // "weekday" | "saturday" | "sunday"
	N             int
	P50DelaySecs  *int
	P90DelaySecs  *int
	Late5Pct      float32
}

// MinObservations is the small-n suppression threshold: rollup rows with fewer
// finalized observations than this are not shown to riders.
const MinObservations = 10
