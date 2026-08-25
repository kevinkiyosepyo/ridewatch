// Package config loads all runtime configuration from environment variables.
// Defaults target the MBTA's keyless public feeds; point the three feed URLs at
// any agency publishing GTFS + GTFS-Realtime to track a different system.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	// Postgres, e.g. postgres://user:pass@host:5432/ridewatch
	DatabaseURL string

	// Feeds.
	StaticGTFSURL       string
	VehiclePositionsURL string
	TripUpdatesURL      string
	PollInterval        time.Duration // spec: 15s
	StaticRefreshEvery  time.Duration // how often to check the static zip for a new version

	// Feed auth, for agencies that key their feeds (e.g. WMATA). When
	// FeedAPIKey is set it is sent as the FeedAPIKeyHeader header on every
	// feed request — header, not URL, so keys never end up in logs.
	FeedAPIKey       string
	FeedAPIKeyHeader string // header name; default "api_key" (WMATA's)

	// Raw archive.
	ArchiveDir       string
	RawRetentionDays int // prune raw blobs older than this; 0 = keep forever

	// Derived-data retention (weeks of partitions to keep; 0 = keep forever).
	VehiclePosRetentionWeeks int
	StopEventRetentionWeeks  int

	// Reconciliation.
	FinalizeGrace time.Duration // how long after the last prediction passes before an event is frozen

	// Rollups.
	RollupHourUTC     int // nightly rollup trigger hour (UTC)
	RollupWindowWeeks int

	// HTTP.
	ListenAddr string
	PublicURL  string // external origin, used in docs/links

	// Web Push.
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	VAPIDSubject    string // mailto: or https: contact
	PushHorizon     time.Duration

	// Frontend.
	TileURL string // raster tile template for MapLibre
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:         env("DATABASE_URL", "postgres://ridewatch:ridewatch@localhost:5432/ridewatch?sslmode=disable"),
		StaticGTFSURL:       env("STATIC_GTFS_URL", "https://cdn.mbta.com/MBTA_GTFS.zip"),
		VehiclePositionsURL: env("VEHICLE_POSITIONS_URL", "https://cdn.mbta.com/realtime/VehiclePositions.pb"),
		TripUpdatesURL:      env("TRIP_UPDATES_URL", "https://cdn.mbta.com/realtime/TripUpdates.pb"),
		FeedAPIKey:          os.Getenv("FEED_API_KEY"),
		FeedAPIKeyHeader:    env("FEED_API_KEY_HEADER", "api_key"),
		ArchiveDir:          env("ARCHIVE_DIR", "./data/raw"),
		ListenAddr:          env("LISTEN_ADDR", ":8080"),
		PublicURL:           env("PUBLIC_URL", "http://localhost:8080"),
		VAPIDPublicKey:      os.Getenv("VAPID_PUBLIC_KEY"),
		VAPIDPrivateKey:     os.Getenv("VAPID_PRIVATE_KEY"),
		VAPIDSubject:        env("VAPID_SUBJECT", "mailto:admin@example.com"),
		TileURL:             env("TILE_URL", "https://tile.openstreetmap.org/{z}/{x}/{y}.png"),
	}
	var err error
	if c.PollInterval, err = envDuration("POLL_INTERVAL", 15*time.Second); err != nil {
		return c, err
	}
	if c.StaticRefreshEvery, err = envDuration("STATIC_REFRESH_EVERY", 6*time.Hour); err != nil {
		return c, err
	}
	if c.FinalizeGrace, err = envDuration("FINALIZE_GRACE", 10*time.Minute); err != nil {
		return c, err
	}
	if c.PushHorizon, err = envDuration("PUSH_HORIZON", 45*time.Minute); err != nil {
		return c, err
	}
	if c.RawRetentionDays, err = envInt("RAW_RETENTION_DAYS", 30); err != nil {
		return c, err
	}
	if c.VehiclePosRetentionWeeks, err = envInt("VEHICLE_POS_RETENTION_WEEKS", 8); err != nil {
		return c, err
	}
	if c.StopEventRetentionWeeks, err = envInt("STOP_EVENT_RETENTION_WEEKS", 26); err != nil {
		return c, err
	}
	if c.RollupHourUTC, err = envInt("ROLLUP_HOUR_UTC", 7); err != nil { // ~3 AM US/Eastern
		return c, err
	}
	if c.RollupWindowWeeks, err = envInt("ROLLUP_WINDOW_WEEKS", 6); err != nil {
		return c, err
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
