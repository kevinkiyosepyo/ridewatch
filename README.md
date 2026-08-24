# RideWatch — is your bus *actually* on time?

RideWatch is a public transit reliability tracker. It polls an agency's free
GTFS-Realtime protobuf feeds every fifteen seconds, reconciles every vehicle
against the static schedule, and tells riders the thing no agency will:
*"the 8:12 northbound is five or more minutes late on 38% of weekday mornings."*

Defaults point at the MBTA's keyless public feeds; three environment variables
retarget it at any agency that publishes GTFS + GTFS-Realtime.

**Status: under construction — full docs land with the first release.**

```
make db-up && make migrate && make run    # then open http://localhost:8080
```

## How it works (short version)

- **Ingest**: two pollers (vehicle positions, trip updates) fetch protobufs on a
  15s tick. Every raw blob is gzipped to disk *before* parsing, so the entire
  derived dataset can be recomputed from scratch (`ridewatch replay`).
- **Reconcile**: predictions are matched to the static schedule and upserted on
  a natural key `(service_date, trip_id, start_time, stop_sequence)` with a
  feed-timestamp guard — repeated polls, out-of-order applies, and full replays
  are all idempotent. Static GTFS republishes are snapshotted as immutable
  schedule versions so historical comparisons never rot.
- **Store**: weekly-partitioned Postgres (`stop_events`, `vehicle_positions`),
  growing by millions of rows a week; nightly jobs precompute percentile delay
  by route, stop, and hour-of-week into materialized aggregate tables.
- **Serve**: MapLibre map of live vehicles, plain-language stop pages, and Web
  Push "your bus is running late" alerts via VAPID — no email provider, no cost.
- **Observe**: Prometheus metrics chosen to predict breakage (feed staleness,
  ingest lag, dropped polls) with Grafana dashboards and alert rules.
- **Run**: k3s on a free-tier instance behind Caddy (automatic Let's Encrypt),
  provisioned by OpenTofu, deployed by GitHub Actions.
