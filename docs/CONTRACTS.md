# RideWatch — package contracts

This file freezes the boundaries between packages so components can be built
independently. **Read `internal/domain/domain.go`, `internal/domain/gtfstime.go`,
`internal/domain/interfaces.go`, and `db/migrations/0001_init.sql` before writing
any code.** If a contract here is unworkable, note it in your report — do not
edit shared files.

## Shared, frozen (do not edit)

- `go.mod` / `go.sum` — all dependencies are pre-resolved. Allowed imports beyond
  stdlib: `github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs`,
  `google.golang.org/protobuf`, `github.com/jackc/pgx/v5` (+ `pgxpool`),
  `github.com/SherClockHolmes/webpush-go`,
  `github.com/prometheus/client_golang`. Nothing else.
- `internal/domain` — types, interfaces, GTFS time helpers.
- `internal/config` — env config.
- `internal/metrics` — every Prometheus collector; increment these, never define
  new collectors elsewhere.
- `db/migrations/0001_init.sql` — the schema.

Each component owns exactly its listed paths, may run
`go build ./internal/<pkg> && go vet ./internal/<pkg> && go test ./internal/<pkg>`
(other packages may not exist yet — never build `./...`), and writes hermetic
tests: no network, no live DB unless `TEST_DATABASE_URL` is set (skip otherwise).

## internal/gtfsstatic — static GTFS fetch + parse

```go
// Download fetches url to destDir/gtfs-<sha8>.zip (temp file + rename),
// returning the file path and hex sha256 of the bytes.
func Download(ctx context.Context, url, destDir string) (path, sha256 string, err error)

// ParseZip streams a GTFS zip into w. Calls w.Meta as soon as agency.txt and
// feed_info.txt are read (parse those two first; feedVersion "" if absent).
// Streams stop_times.txt with encoding/csv ReuseRecord — never load the file
// into memory (MBTA stop_times is ~2M rows). Missing optional files are fine;
// missing required files (agency, stops, routes, trips, stop_times) error.
// Columns are located by header name, never by position.
func ParseZip(path string, w domain.ScheduleWriter) error
```

Times parse via `domain.ParseGTFSTime` (empty → -1). direction_id absent → -1.

## internal/ingest — GTFS-RT polling, raw archive, decode

```go
type Sink func(ctx context.Context, snap *domain.Snapshot) error

type PollerConfig struct {
    Feed       domain.Feed
    URL        string
    Interval   time.Duration
    ArchiveDir string // root; poller writes blobs under it
}

func NewPoller(cfg PollerConfig, idx domain.RawArchive, sink Sink) *Poller
func (p *Poller) Run(ctx context.Context) error // ticks until ctx is done; never returns on poll errors

// DecodeFeed is the single protobuf→domain mapping, shared by poller and replay.
// Absent optional fields map to the documented sentinels (-1 / "" / 0).
func DecodeFeed(feed domain.Feed, raw []byte, polledAt time.Time) (*domain.Snapshot, error)

// Replay walks archived blobs for both feeds chronologically across the time
// range and feeds each through DecodeFeed into sink. Returns blobs processed.
func Replay(ctx context.Context, archiveDir string, from, to time.Time, sink Sink) (int, error)

// PruneArchive deletes blobs older than retention. Returns files removed.
func PruneArchive(archiveDir string, olderThan time.Time) (int, error)
```

Poll cycle: fetch (timeout < interval) → sha256 → if sha == `idx.LastSHA` count
`unchanged`, skip archive+decode → else gzip blob to
`{feed}/{YYYY}/{MM}/{DD}/{HH}/{unix}_{sha8}.pb.gz` (UTC path parts, temp file +
rename) → `idx.RecordRawPoll` → decode → sink. Archive BEFORE decode: a blob
that fails to decode is still archived (outcome `decode_error`). Update
`metrics.Polls`, `PollDuration`, `FeedStaleness`, `RawArchiveBytes`. Replay must
derive feed + timestamp from the path alone (no DB).

## internal/reconcile — the reconciliation engine

```go
type Options struct {
    FinalizeGrace time.Duration
    Now           func() time.Time // nil = time.Now; injectable for tests
}

func NewEngine(sched domain.ScheduleRepo, events domain.EventStore, opts Options) *Engine

// Process handles one snapshot: resolve trips against the active schedule
// version, build StopEvent upserts / VehiclePosition rows, apply via events,
// refresh the in-memory live state. Concurrency-safe.
func (e *Engine) Process(ctx context.Context, snap *domain.Snapshot) error

// Sweep runs finalization (events.FinalizeDue) and ages out live vehicles /
// live events unseen for >3 min. Call every ~30s.
func (e *Engine) Sweep(ctx context.Context) error

// Engine implements domain.LiveSource.
```

Rules (each one is a test):
1. **Service date**: use `TripDescriptor.start_date` when present. Otherwise
   resolve against the schedule: try yesterday and today (agency tz); pick the
   date whose scheduled time for the trip's first predicted stop is closest to
   the prediction — this handles after-midnight (>24:00) trips.
2. **Delay**: prefer explicit per-stop arrival time, else departure time, else
   trip-level or per-stop `delay` applied to the scheduled time. delay =
   predicted − scheduled (via `domain.ScheduledTime` with the version's tz).
   Propagate the last known StopTimeUpdate delay to later stops of the trip
   that have no update of their own (GTFS-RT propagation rule).
3. **Idempotency** is the store's ON CONFLICT guard; the engine just stamps
   `FeedTimestamp` (header ts; per-entity ts when present) on every event.
4. **Unmatched trips** (ADDED / not in schedule): count
   `metrics.UnmatchedTrips`, keep vehicle on the map, emit no StopEvent.
   CANCELED trips and SKIPPED stops emit events flagged `Skipped` with no
   prediction.
5. **Vanishing vehicles**: live entries age out after 3 min; DB finalization is
   time-based (`FinalizeDue`), so a vehicle that reappears simply resumes
   upserting the same natural keys, and one that never returns gets its
   remaining events finalized from their last predictions after the grace.
6. Stop-sequence-only or stop-id-only StopTimeUpdates both resolve via the
   trip's schedule.

Live state: latest `LiveVehicle` per vehicle (join delay from that vehicle's
trip events; enrich RouteShortName/Headsign via ScheduleRepo), non-final events
per stop for `UpcomingAtStop`, `FeedAge` from header timestamps. Cache
`TripSchedule` lookups (bounded, keyed by version+trip; drop on version change).
Set `metrics.ActiveVehicles`, `IngestLag`.

## internal/store + db/migrations.go + internal/rollup — Postgres

`db/migrations.go`: `package db`, `//go:embed migrations/*.sql`, exported `var FS embed.FS`.

```go
func Open(ctx context.Context, databaseURL string) (*Store, error) // pgxpool
func (s *Store) Close()
func (s *Store) Pool() *pgxpool.Pool
func (s *Store) Migrate(ctx context.Context) error // schema_migrations table, ordered .sql from db.FS, each in a tx

// Weekly partitions, ISO-Monday boundaries, for stop_events and
// vehicle_positions: stop_events_y2026w35 etc. Idempotent (IF NOT EXISTS).
func (s *Store) EnsurePartitions(ctx context.Context, from, to time.Time) error
// DropOldPartitions detaches+drops whole weeks older than keepWeeks (skip if 0).
func (s *Store) DropOldPartitions(ctx context.Context, keepWeeks map[string]int) error

var ErrVersionExists = errors.New("schedule version already loaded")
// NewScheduleLoad inserts a schedule_versions row and returns a loader whose
// ScheduleWriter methods COPY rows in one transaction.
func (s *Store) NewScheduleLoad(ctx context.Context, sha256 string, fetchedAt time.Time) (*ScheduleLoad, error)
func (l *ScheduleLoad) Commit(ctx context.Context) (versionID int64, err error) // sets loaded_at, atomically moves `active`
func (l *ScheduleLoad) Abort(ctx context.Context) error
```

`Store` implements `domain.ScheduleRepo` (TripSchedule behind a bounded
in-process cache), `domain.EventStore`, `domain.RawArchive`,
`domain.StopQueries`, `domain.SubscriptionStore`.

UpsertStopEvents (batch, single statement or pgx.Batch):
```sql
INSERT ... ON CONFLICT (service_date, trip_id, start_time, stop_sequence) DO UPDATE
SET predicted_arrival=EXCLUDED.predicted_arrival, delay_secs=EXCLUDED.delay_secs,
    vehicle_id=..., feed_timestamp=EXCLUDED.feed_timestamp,
    last_updated=EXCLUDED.last_updated, update_count=stop_events.update_count+1,
    skipped=EXCLUDED.skipped
WHERE stop_events.feed_timestamp < EXCLUDED.feed_timestamp AND NOT stop_events.final
```
`FinalizeDue`: `final=false AND COALESCE(predicted_arrival, scheduled_arrival) < now-grace`
→ `final=true, actual_arrival=predicted_arrival, delay_secs` frozen (delay stays
NULL when there was never a prediction — never fabricate on-time records).
All timestamps stored UTC. `service_date` DATE column ↔ domain YYYYMMDD strings.
StopQueries reads reference data from the **active** schedule version and
filters rollups to `n >= domain.MinObservations`. DB-touching tests skip unless
`TEST_DATABASE_URL` is set.

`internal/rollup`:
```go
// Run recomputes all three rollup tables over the trailing window from
// finalized, non-skipped stop_events with non-null delay (percentile_cont for
// p50/p90; hour_of_week and day_class in agency tz). DELETE+INSERT per table
// in one transaction each.
func Run(ctx context.Context, pool *pgxpool.Pool, agencyTZ string, windowWeeks int) error
```

## internal/api + internal/push — HTTP + Web Push

```go
func New(cfg config.Config, q domain.StopQueries, live domain.LiveSource,
         subs domain.SubscriptionStore, static fs.FS) http.Handler
```

Go 1.22+ ServeMux patterns. JSON endpoints (all under /api, no auth, read-only
except subscriptions):
- `GET /api/vehicles` → GeoJSON FeatureCollection of `live.LiveVehicles()`
- `GET /api/stops?bbox=minLon,minLat,maxLon,maxLat` and `?q=` (search)
- `GET /api/stops/{id}` → stop + routes serving it
- `GET /api/stops/{id}/upcoming` → live.UpcomingAtStop, enriched
- `GET /api/stops/{id}/reliability` → hourly + departure stats **plus
  plain-language sentences** rendered server-side ("The 8:12 AM Route 1 toward
  Harvard is 5+ minutes late on 38% of weekday mornings"), from
  `domain.FormatGTFSTime`/rollup rows
- `GET /api/routes`, `GET /api/routes/{id}/reliability`
- `GET /api/feedinfo` → feed ages, active schedule version (for the status pill)
- `POST /api/subscriptions` `{endpoint,keys:{p256dh,auth},stop_id,route_id?,direction_id?,threshold_secs?}`
- `DELETE /api/subscriptions` `{endpoint}`
- `GET /api/vapid-public-key` → `{key}`
- `GET /healthz`; `GET /metrics` (promhttp); everything else serves `static`
  (SPA fallback to index.html for /stop/*). Request-size limits on POSTs,
  `Cache-Control: no-store` on live endpoints, gzip not required.

```go
// push
type Config struct{ VAPIDPublic, VAPIDPrivate, Subject string; Horizon time.Duration }
func NewEvaluator(cfg Config, subs domain.SubscriptionStore, live domain.LiveSource, q domain.StopQueries) *Evaluator
func (e *Evaluator) Evaluate(ctx context.Context) error // one pass; call every ~60s from cmd
func GenerateVAPIDKeys() (publicKey, privateKey string, err error)
```
Evaluate: for each subscription → `live.UpcomingAtStop(stop, horizon)` →
filter route/direction → events with `DelaySecs >= ThresholdSecs` →
`MarkPushSent` (skip when not fresh) → webpush send (TTL ~15 min, payload JSON
`{title, body, url}` with plain-language body) → `RecordPushResult`
(404/410 ⇒ gone). Count `metrics.PushSent`.

## web/ — frontend (embedded)

`web/embed.go`: `package web`, `//go:embed` all assets, exported `var FS embed.FS`
(cmd passes `web.FS` into api.New via fs.Sub). Vanilla JS + MapLibre GL
(vendored minified into `web/vendor/`, committed — no CDN at runtime). Pages:
- `index.html` — full-screen MapLibre map, OSM raster tiles from a
  `TILE_URL`-injected template (`/api/feedinfo` carries it) with attribution,
  live vehicles from `/api/vehicles` every 10s (delay-colored, click →
  route/delay popup), stop search box, stops appear at zoom ≥ 14 via bbox,
  status pill from `/api/feedinfo`.
- `stop.html` (served at `/stop/{id}`) — upcoming arrivals with live delays,
  the plain-language reliability sentences, an hour-of-week reliability
  heatmap (pure CSS/JS), worst departures table, and the "Notify me" flow.
- `sw.js` — service worker at root scope: `push` → showNotification,
  `notificationclick` → focus/open the stop page.
- `app.js`, `stop.js`, `style.css` (mobile-first, dark theme).
Notify flow: permission → `pushManager.subscribe` with VAPID key → POST
`/api/subscriptions` with threshold picker (3/5/10 min).

## deploy/observability — Prometheus rules + Grafana

`prometheus-rules.yaml`: FeedStalled (`ridewatch_feed_staleness_seconds > 300`
for 5m, per feed), NoSuccessfulPolls (10m), IngestLagHigh (p95 > 60s, 10m),
ScheduleVersionStale (> 45 days), PushFailureRatioHigh, RollupFailed
(increase of `outcome="error"`), plus `up == 0`. `grafana-dashboard.json`:
staleness, poll outcomes rate, ingest lag p50/p95, active vehicles, upserts/s,
unmatched trips, push outcomes, archive growth. `README.md` explaining each
alert's rationale.

## deploy/ — Dockerfile, k3s, Caddy, OpenTofu, CI

Single static binary `ridewatch` with subcommands (cmd wiring comes later):
`serve` | `replay --from --to` | `load-static` | `vapid-keys` | `migrate`.
Multi-stage Dockerfile (golang → gcr.io/distroless/static, nonroot). k3s
manifests in `deploy/k8s/`: namespace `ridewatch`; Postgres StatefulSet + PVC;
app Deployment (1 replica, Recreate strategy — poller must be a singleton),
PVC for `/data` archive, probes on `/healthz`, resources sized for a free-tier
VM; Prometheus + Grafana Deployments with provisioned scrape config, rules,
dashboard; Caddy Deployment with hostPorts 80/443, Caddyfile reverse_proxy →
app (auto Let's Encrypt; domain via env), `/metrics` blocked externally.
Secrets templated as `secrets.example.yaml` (never real values).
`deploy/tofu/`: OCI always-free ARM instance (or documented variables for any
provider), cloud-init installing k3s + kubectl apply of the manifests, outputs
the IP. `.github/workflows/ci.yml`: vet + build + test (Go from go.mod) on
push/PR; `deploy.yml`: on tag — buildx image → GHCR, then SSH `kubectl set
image`, secrets documented in README comments.
