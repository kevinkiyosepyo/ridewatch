// Package metrics defines every Prometheus collector in one place. The chosen
// metrics are the ones that predict breakage: feed staleness, ingest lag, and
// dropped polls — not vanity counters.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// FeedStaleness: now minus FeedHeader.timestamp of the last successful
	// poll, updated every poll. THE canary — it rises whether the agency's
	// feed stalls or our poller does.
	FeedStaleness = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ridewatch_feed_staleness_seconds",
		Help: "Age of the newest feed header timestamp per feed.",
	}, []string{"feed"})

	// Polls: outcome ∈ ok | error | unchanged (byte-identical response) |
	// decode_error. "dropped polls" = rate of error + decode_error.
	Polls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ridewatch_polls_total",
		Help: "Feed poll attempts by outcome.",
	}, []string{"feed", "outcome"})

	PollDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ridewatch_poll_duration_seconds",
		Help:    "Wall time of one poll (fetch + archive + decode).",
		Buckets: prometheus.DefBuckets,
	}, []string{"feed"})

	// IngestLag: feed header timestamp -> derived rows committed.
	IngestLag = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ridewatch_ingest_lag_seconds",
		Help:    "Latency from feed header timestamp to database commit.",
		Buckets: []float64{1, 2.5, 5, 10, 20, 30, 60, 120, 300},
	}, []string{"feed"})

	StopEventsUpserted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ridewatch_stop_events_upserted_total",
		Help: "Stop-event rows inserted or advanced (not skipped by the idempotency guard).",
	})

	StopEventsFinalized = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ridewatch_stop_events_finalized_total",
		Help: "Stop events frozen by the finalization sweeper.",
	})

	VehiclePositionsWritten = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ridewatch_vehicle_positions_written_total",
		Help: "Vehicle position rows appended.",
	})

	ActiveVehicles = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ridewatch_active_vehicles",
		Help: "Vehicles currently visible in the live snapshot.",
	})

	UnmatchedTrips = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ridewatch_unmatched_trips_total",
		Help: "Realtime trips with no match in the active static schedule.",
	})

	ScheduleVersionAge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ridewatch_schedule_version_age_days",
		Help: "Days since the active static schedule version was fetched.",
	})

	ScheduleLoads = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ridewatch_schedule_loads_total",
		Help: "Static GTFS load attempts by outcome (loaded | unchanged | error).",
	}, []string{"outcome"})

	PushSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ridewatch_push_notifications_total",
		Help: "Web Push sends by outcome (ok | gone | error).",
	}, []string{"outcome"})

	RollupRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ridewatch_rollup_runs_total",
		Help: "Nightly rollup runs by outcome (ok | error).",
	}, []string{"outcome"})

	RollupDuration = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ridewatch_rollup_duration_seconds",
		Help: "Wall time of the last rollup run.",
	})

	RawArchiveBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ridewatch_raw_archive_bytes_total",
		Help: "Compressed bytes written to the raw archive.",
	})
)
