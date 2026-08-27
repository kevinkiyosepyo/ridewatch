# RideWatch: is your bus *actually* on time?

RideWatch is a public transit reliability tracker. It polls an agency's free
GTFS-Realtime protobuf feeds every fifteen seconds, reconciles every vehicle
against the static schedule, and tells riders the thing no agency will:
*"the 8:12 northbound is five or more minutes late on 38% of weekday mornings."*

Riders get a live MapLibre map of every vehicle, a plain-language track record
for each stop, and Web Push alerts when a stop they follow is running late
right now. No accounts, no email provider, no API keys for the default agency.

Defaults point at the MBTA's keyless public feeds. Three environment variables
retarget it at any agency that publishes GTFS + GTFS-Realtime.

## Run it locally

You need Go 1.27+ and Docker.

```sh
git clone https://github.com/kevinkiyosepyo/ridewatch && cd ridewatch
make db-up        # Postgres 16 in Docker
make migrate
make run          # then open http://localhost:8080
```

First boot downloads the MBTA static schedule (~18 MB zip, about 25 seconds to
load its ~2M stop_times rows via COPY) before polling starts. Within a minute
the map shows live vehicles. Stop pages start out thin and get honest as
finalized observations accumulate: reliability stats appear after the first
nightly rollup, and hide behind a small-sample threshold (n >= 10) until there
is enough data to mean something.

If something else owns port 5432 (a Homebrew Postgres, say):

```sh
RIDEWATCH_PG_PORT=5433 make db-up
DATABASE_URL='postgres://ridewatch:ridewatch@localhost:5433/ridewatch?sslmode=disable' make run
```

For push alerts, generate a key pair once and export it:

```sh
make vapid        # prints VAPID_PUBLIC_KEY / VAPID_PRIVATE_KEY
```

## Point it at your agency

```sh
STATIC_GTFS_URL=https://your.agency/gtfs.zip \
VEHICLE_POSITIONS_URL=https://your.agency/rt/vehiclepositions.pb \
TRIP_UPDATES_URL=https://your.agency/rt/tripupdates.pb \
make run
```

Each URL takes a comma-separated list for agencies that split their feeds by
mode (WMATA publishes bus and rail separately): static zips merge into one
schedule version, and each realtime URL gets its own poller. Keyed feeds work
too: set `FEED_API_KEY`, and it is sent as a header (`FEED_API_KEY_HEADER`,
default `api_key`) rather than in the URL, so keys stay out of logs.

## The engineering

Fetching protobufs is easy. The hard part is turning an endless stream of
repeated, mutating predictions into truthful history.

### Reconciliation

Consecutive polls repeat the same predictions, so every observation lands on a
natural key, `(service_date, trip_id, start_time, stop_sequence)`, and only
advances a row when its feed timestamp is strictly newer and the row is not
yet final:

```sql
INSERT ... ON CONFLICT (service_date, trip_id, start_time, stop_sequence)
DO UPDATE SET ... WHERE stop_events.feed_timestamp < EXCLUDED.feed_timestamp
                    AND NOT stop_events.final
```

That one guard makes repeated polls, out-of-order applies, and full replays
idempotent. Trip IDs recycle daily (`service_date` disambiguates) and vehicles
vanish mid-route: a sweeper finalizes an event once its last prediction is
comfortably in the past, freezing the prediction as the actual arrival. A row
with no prediction finalizes with a NULL delay, never a fabricated "on time".
Schedule times go through the GTFS noon-minus-12h rule in the agency's
timezone, which is what keeps a `25:30:00` stop time and a DST transition day
both honest.

### Schedule versioning

Agencies republish their static GTFS every few weeks. Each publish is
snapshotted as an immutable version (keyed by the zip's SHA-256), and every
observation records the version it was derived against. When stop 542 moves or
a trip is renumbered, historical comparisons keep meaning what they meant
instead of silently rotting.

### Replay

Every raw protobuf blob is gzipped to disk *before* parsing. When a
reconciliation bug surfaces, fix it and run
`ridewatch replay -from 2026-08-01 -to 2026-08-24`: the entire derived dataset
recomputes from the archive, and idempotency means replaying over existing
rows is safe. The blobs plus the static zips are the system of record;
Postgres is a cache of conclusions.

### Storage that survives growth

`stop_events` and `vehicle_positions` are range-partitioned by week (created
ahead of need, dropped past retention), and no page ever aggregates them at
request time: a nightly job precomputes p50/p90 delay and late-share into
three rollup tables, by stop and hour-of-week, by route and hour-of-week, and
by individual scheduled departure. That last one is what powers the "8:12
northbound" sentence.

## Commands

One static binary:

| Command | What it does |
|---|---|
| `ridewatch serve` | Pollers, reconciler, sweeper, rollup scheduler, push evaluator, HTTP server |
| `ridewatch migrate` | Apply embedded SQL migrations |
| `ridewatch load-static` | Force a static GTFS download/load now |
| `ridewatch replay -from Y-M-D -to Y-M-D` | Recompute derived data from the raw archive |
| `ridewatch vapid-keys` | Generate a Web Push key pair |

## Configuration

Everything is environment variables. The interesting ones:

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | local dev Postgres | pgx connection string |
| `STATIC_GTFS_URL` | MBTA zip | static schedule; comma-separated to merge |
| `VEHICLE_POSITIONS_URL` | MBTA | GTFS-RT vehicle positions; comma-separated for one poller each |
| `TRIP_UPDATES_URL` | MBTA | GTFS-RT trip updates; same |
| `FEED_API_KEY` / `FEED_API_KEY_HEADER` | unset / `api_key` | auth header for keyed agencies |
| `POLL_INTERVAL` | `15s` | realtime poll cadence |
| `STATIC_REFRESH_EVERY` | `6h` | how often to check for a republished schedule |
| `ARCHIVE_DIR` | `./data/raw` | raw blob archive root |
| `RAW_RETENTION_DAYS` | `30` | prune raw blobs (0 keeps forever) |
| `STOP_EVENT_RETENTION_WEEKS` | `26` | drop old event partitions (0 keeps forever) |
| `VEHICLE_POS_RETENTION_WEEKS` | `8` | drop old position partitions |
| `FINALIZE_GRACE` | `10m` | quiet period before an event is frozen |
| `ROLLUP_HOUR_UTC` | `7` | nightly rollup trigger |
| `ROLLUP_WINDOW_WEEKS` | `6` | trailing window the stats describe |
| `VAPID_PUBLIC_KEY` / `VAPID_PRIVATE_KEY` / `VAPID_SUBJECT` | unset | Web Push; alerts disabled without them |
| `TILE_URL` | OSM raster tiles | MapLibre tile template |
| `LISTEN_ADDR` | `:8080` | HTTP bind |

## HTTP API

All JSON, no auth, read-only except subscriptions:

| Endpoint | Returns |
|---|---|
| `GET /api/vehicles` | live vehicles as GeoJSON, delay-annotated |
| `GET /api/stops?q=` / `?bbox=` | stop search / stops in a map viewport |
| `GET /api/stops/{id}` | stop details + routes serving it |
| `GET /api/stops/{id}/upcoming` | next arrivals with live delays |
| `GET /api/stops/{id}/reliability` | hourly + per-departure stats, plus the plain-language sentences |
| `GET /api/routes`, `GET /api/routes/{id}/reliability` | route list, route-level stats |
| `GET /api/feedinfo` | feed ages, tile URL, whether push is enabled |
| `POST` / `DELETE /api/subscriptions` | follow / unfollow a stop for push alerts |
| `GET /healthz`, `GET /metrics` | health, Prometheus exposition |

## Observability

The exported metrics are the ones that predict breakage, not vanity counters:
`ridewatch_feed_staleness_seconds` (age of the newest feed header; it rises
whether the agency stalls or the poller does), `ridewatch_polls_total` by
outcome (dropped polls = `error` + `decode_error`), and
`ridewatch_ingest_lag_seconds` (header timestamp to database commit). Alert
rules and a Grafana dashboard live in
[`deploy/observability/`](deploy/observability/), with a README explaining what
each alert usually means and where to look first.

## Production

The stack runs on a single free-tier VM: k3s, Postgres, Prometheus, Grafana,
and Caddy terminating TLS with automatic Let's Encrypt certificates.
[`deploy/tofu/`](deploy/tofu/) provisions an Oracle Cloud always-free ARM
instance with k3s preinstalled via cloud-init; [`deploy/k8s/`](deploy/k8s/) is
the kustomize root. CI runs vet, build, and the full test suite (including the
Postgres-backed tests) on every push; tagging `v*` builds a multi-arch image to
GHCR and rolls the deployment over SSH. Bring-up order, secrets, and teardown
are in [`deploy/README.md`](deploy/README.md).

Total monthly cost: $0. Always-free instance, Let's Encrypt certificates, and
Web Push instead of an email provider.

## Development

```
internal/ingest      pollers, raw archive, protobuf decode, replay
internal/gtfsstatic  static GTFS download + streaming zip parse
internal/reconcile   prediction -> stop-event engine, live state
internal/store       migrations, partitions, COPY loads, queries
internal/rollup      nightly percentile aggregates
internal/api         HTTP endpoints, reliability sentences
internal/push        VAPID evaluator
web/                 MapLibre map, stop pages, service worker (embedded)
```

`make test` runs the hermetic suite. Store and rollup tests exercise real SQL
when `TEST_DATABASE_URL` is set (CI wires a Postgres service container;
locally, point it at the dev container and mind the port if you overrode it). The
reconciliation engine has one named test per contract rule in
[`docs/CONTRACTS.md`](docs/CONTRACTS.md), which froze the package boundaries
this was built against.
