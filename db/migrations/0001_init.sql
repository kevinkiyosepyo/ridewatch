-- RideWatch initial schema.
--
-- Design notes:
--   * Every static GTFS table is keyed by schedule_version_id. The static feed is
--     republished every few weeks; each publish is snapshotted as a new version and
--     historical observations keep pointing at the version they were derived from,
--     so old delay comparisons never silently rot.
--   * stop_events and vehicle_positions are range-partitioned by week. Partitions are
--     created by the application (see store.EnsurePartitions); the DEFAULT partition
--     is a safety net so out-of-range inserts (e.g. replaying old archives before
--     partitions exist) never fail.
--   * Rollup tables are plain tables rewritten by the nightly job, not MATERIALIZED
--     VIEWs, so they can be refreshed per-key inside a transaction.

CREATE TABLE schedule_versions (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sha256       TEXT NOT NULL UNIQUE,          -- hash of the static GTFS zip
    feed_version TEXT NOT NULL DEFAULT '',      -- feed_info.feed_version if present
    agency_tz    TEXT NOT NULL,                 -- agency.agency_timezone
    fetched_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    loaded_at    TIMESTAMPTZ,                   -- set when the load transaction commits
    active       BOOLEAN NOT NULL DEFAULT false
);

-- at most one active version at a time
CREATE UNIQUE INDEX schedule_versions_one_active ON schedule_versions (active) WHERE active;

CREATE TABLE gtfs_routes (
    schedule_version_id BIGINT NOT NULL REFERENCES schedule_versions(id) ON DELETE CASCADE,
    route_id    TEXT NOT NULL,
    short_name  TEXT NOT NULL DEFAULT '',
    long_name   TEXT NOT NULL DEFAULT '',
    route_type  INT  NOT NULL,
    color       TEXT NOT NULL DEFAULT '',
    text_color  TEXT NOT NULL DEFAULT '',
    sort_order  INT,
    PRIMARY KEY (schedule_version_id, route_id)
);

CREATE TABLE gtfs_stops (
    schedule_version_id BIGINT NOT NULL REFERENCES schedule_versions(id) ON DELETE CASCADE,
    stop_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    lat            DOUBLE PRECISION,
    lon            DOUBLE PRECISION,
    parent_station TEXT NOT NULL DEFAULT '',
    location_type  INT NOT NULL DEFAULT 0,
    platform_code  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (schedule_version_id, stop_id)
);

CREATE INDEX gtfs_stops_geo ON gtfs_stops (schedule_version_id, lat, lon);
CREATE INDEX gtfs_stops_name ON gtfs_stops (schedule_version_id, lower(name) text_pattern_ops);

CREATE TABLE gtfs_trips (
    schedule_version_id BIGINT NOT NULL REFERENCES schedule_versions(id) ON DELETE CASCADE,
    trip_id      TEXT NOT NULL,
    route_id     TEXT NOT NULL,
    service_id   TEXT NOT NULL,
    headsign     TEXT NOT NULL DEFAULT '',
    direction_id SMALLINT NOT NULL DEFAULT -1,  -- -1 = unknown
    shape_id     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (schedule_version_id, trip_id)
);

CREATE INDEX gtfs_trips_route ON gtfs_trips (schedule_version_id, route_id);

CREATE TABLE gtfs_stop_times (
    schedule_version_id BIGINT NOT NULL REFERENCES schedule_versions(id) ON DELETE CASCADE,
    trip_id        TEXT NOT NULL,
    stop_sequence  INT NOT NULL,
    stop_id        TEXT NOT NULL,
    -- GTFS times are seconds after "noon minus 12h" on the service date and may
    -- exceed 86400 for after-midnight stops. -1 = not specified.
    arrival_secs   INT NOT NULL DEFAULT -1,
    departure_secs INT NOT NULL DEFAULT -1,
    PRIMARY KEY (schedule_version_id, trip_id, stop_sequence)
);

CREATE INDEX gtfs_stop_times_stop ON gtfs_stop_times (schedule_version_id, stop_id);

CREATE TABLE gtfs_calendar (
    schedule_version_id BIGINT NOT NULL REFERENCES schedule_versions(id) ON DELETE CASCADE,
    service_id TEXT NOT NULL,
    monday BOOLEAN NOT NULL, tuesday BOOLEAN NOT NULL, wednesday BOOLEAN NOT NULL,
    thursday BOOLEAN NOT NULL, friday BOOLEAN NOT NULL, saturday BOOLEAN NOT NULL,
    sunday BOOLEAN NOT NULL,
    start_date DATE NOT NULL,
    end_date   DATE NOT NULL,
    PRIMARY KEY (schedule_version_id, service_id)
);

CREATE TABLE gtfs_calendar_dates (
    schedule_version_id BIGINT NOT NULL REFERENCES schedule_versions(id) ON DELETE CASCADE,
    service_id     TEXT NOT NULL,
    date           DATE NOT NULL,
    exception_type SMALLINT NOT NULL,  -- 1 = service added, 2 = service removed
    PRIMARY KEY (schedule_version_id, service_id, date)
);

-- Index of raw protobuf blobs archived to disk before parsing. The blobs plus the
-- static GTFS zips are the system of record: the entire derived dataset can be
-- recomputed from them (cmd: ridewatch replay).
CREATE TABLE raw_polls (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    feed           TEXT NOT NULL,           -- 'vehicle_positions' | 'trip_updates'
    polled_at      TIMESTAMPTZ NOT NULL,
    feed_timestamp BIGINT,                  -- FeedHeader.timestamp (unix seconds)
    sha256         TEXT NOT NULL,
    path           TEXT NOT NULL,           -- relative to the archive root
    bytes          INT NOT NULL,
    entity_count   INT NOT NULL DEFAULT 0
);

CREATE INDEX raw_polls_feed_time ON raw_polls (feed, polled_at);

-- The heart of the system: one row per (trip instance, stop). Consecutive polls
-- repeat the same predictions; they land on the same natural key and only advance
-- the row when the feed timestamp is newer, which makes ingestion idempotent and
-- replays safe. trip_ids recycle daily (service_date disambiguates) and across
-- schedule republishes (schedule_version_id pins the comparison basis).
CREATE TABLE stop_events (
    service_date  DATE NOT NULL,
    trip_id       TEXT NOT NULL,
    start_time    TEXT NOT NULL DEFAULT '',  -- frequency-trip disambiguator; '' for schedule-based trips
    stop_sequence INT  NOT NULL,
    stop_id       TEXT NOT NULL,
    route_id      TEXT NOT NULL,
    direction_id  SMALLINT NOT NULL DEFAULT -1,
    schedule_version_id BIGINT NOT NULL,
    vehicle_id    TEXT NOT NULL DEFAULT '',
    scheduled_arrival TIMESTAMPTZ,           -- resolved from the static schedule in agency tz
    predicted_arrival TIMESTAMPTZ,           -- latest RT prediction
    actual_arrival    TIMESTAMPTZ,           -- frozen at finalization
    delay_secs    INT,                       -- (best-known arrival - scheduled); authoritative once final
    final         BOOLEAN NOT NULL DEFAULT false,
    skipped       BOOLEAN NOT NULL DEFAULT false,  -- stop was SKIPPED in the feed
    feed_timestamp BIGINT NOT NULL,          -- monotonic guard for idempotent upserts
    first_seen    TIMESTAMPTZ NOT NULL,
    last_updated  TIMESTAMPTZ NOT NULL,
    update_count  INT NOT NULL DEFAULT 1,
    PRIMARY KEY (service_date, trip_id, start_time, stop_sequence)
) PARTITION BY RANGE (service_date);

CREATE INDEX stop_events_stop  ON stop_events (stop_id, service_date);
CREATE INDEX stop_events_route ON stop_events (route_id, service_date);
CREATE INDEX stop_events_open  ON stop_events (service_date) WHERE NOT final;

CREATE TABLE stop_events_default PARTITION OF stop_events DEFAULT;

-- Vehicle position history (the live map is served from memory, not from here).
CREATE TABLE vehicle_positions (
    ts            TIMESTAMPTZ NOT NULL,
    vehicle_id    TEXT NOT NULL,
    trip_id       TEXT NOT NULL DEFAULT '',
    route_id      TEXT NOT NULL DEFAULT '',
    lat           DOUBLE PRECISION NOT NULL,
    lon           DOUBLE PRECISION NOT NULL,
    bearing       REAL,
    speed_mps     REAL,
    stop_sequence INT,
    status        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (vehicle_id, ts)
) PARTITION BY RANGE (ts);

CREATE INDEX vehicle_positions_ts ON vehicle_positions (ts);

CREATE TABLE vehicle_positions_default PARTITION OF vehicle_positions DEFAULT;

-- Nightly materialized aggregates over finalized stop_events (trailing window).
-- hour_of_week: 0 = Monday 00:00 .. 167 = Sunday 23:00, in the agency timezone.
CREATE TABLE rollup_stop_hour (
    route_id       TEXT NOT NULL,
    stop_id        TEXT NOT NULL,
    direction_id   SMALLINT NOT NULL DEFAULT -1,
    hour_of_week   SMALLINT NOT NULL,
    n              INT NOT NULL,
    p50_delay_secs INT,
    p90_delay_secs INT,
    late5_pct      REAL NOT NULL,   -- share of observations >= 300s late
    early_pct      REAL NOT NULL,   -- share of observations <= -60s (left early)
    window_start   DATE NOT NULL,
    window_end     DATE NOT NULL,
    computed_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (route_id, stop_id, direction_id, hour_of_week)
);

CREATE INDEX rollup_stop_hour_stop ON rollup_stop_hour (stop_id);

-- Per scheduled departure: powers "the 8:12 northbound is 5+ min late on 38% of
-- weekday mornings". scheduled_secs is the GTFS time-of-day of the departure.
CREATE TABLE rollup_departure (
    route_id       TEXT NOT NULL,
    stop_id        TEXT NOT NULL,
    direction_id   SMALLINT NOT NULL DEFAULT -1,
    scheduled_secs INT NOT NULL,
    day_class      TEXT NOT NULL,   -- 'weekday' | 'saturday' | 'sunday'
    n              INT NOT NULL,
    p50_delay_secs INT,
    p90_delay_secs INT,
    late5_pct      REAL NOT NULL,
    window_start   DATE NOT NULL,
    window_end     DATE NOT NULL,
    computed_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (route_id, stop_id, direction_id, scheduled_secs, day_class)
);

CREATE INDEX rollup_departure_stop ON rollup_departure (stop_id);

CREATE TABLE rollup_route_hour (
    route_id       TEXT NOT NULL,
    direction_id   SMALLINT NOT NULL DEFAULT -1,
    hour_of_week   SMALLINT NOT NULL,
    n              INT NOT NULL,
    p50_delay_secs INT,
    p90_delay_secs INT,
    late5_pct      REAL NOT NULL,
    window_start   DATE NOT NULL,
    window_end     DATE NOT NULL,
    computed_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (route_id, direction_id, hour_of_week)
);

-- Web Push (VAPID). One row per browser subscription; a subscription follows one stop.
CREATE TABLE push_subscriptions (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    endpoint       TEXT NOT NULL UNIQUE,
    p256dh         TEXT NOT NULL,
    auth           TEXT NOT NULL,
    stop_id        TEXT NOT NULL,
    route_id       TEXT NOT NULL DEFAULT '',   -- '' = any route at the stop
    direction_id   SMALLINT NOT NULL DEFAULT -1,
    threshold_secs INT NOT NULL DEFAULT 300,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_success   TIMESTAMPTZ,
    failure_count  INT NOT NULL DEFAULT 0
);

CREATE INDEX push_subscriptions_stop ON push_subscriptions (stop_id);

-- One alert per (subscription, trip instance, stop): dedupes repeated evaluator passes.
CREATE TABLE push_sent (
    subscription_id BIGINT NOT NULL REFERENCES push_subscriptions(id) ON DELETE CASCADE,
    service_date    DATE NOT NULL,
    trip_id         TEXT NOT NULL,
    start_time      TEXT NOT NULL DEFAULT '',
    stop_sequence   INT NOT NULL,
    sent_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (subscription_id, service_date, trip_id, start_time, stop_sequence)
);
