# RideWatch observability

`prometheus-rules.yaml` holds the alerting rules (group `ridewatch.rules`);
`grafana-dashboard.json` is an importable dashboard (uid `ridewatch`, datasource
picked via a template variable, 30s refresh, 6h window). Every metric referenced
here is defined in `internal/metrics/metrics.go`. The scrape config is expected
to label the app's target with `job="ridewatch"`.

## Alerts

### FeedStalled (critical)

`ridewatch_feed_staleness_seconds > 300` for 5m, per feed. The feed's newest
header timestamp is more than five minutes old — the one condition that makes
every downstream number a lie: the map shows ghost vehicles, delays freeze,
push alerts fire (or fail to fire) on stale predictions, and the night's
rollups quietly absorb bad data. It predicts either an agency-side outage or a
wedged poller. First step: check the poll-outcomes panel for the feed — if
`ok` polls continue while staleness climbs, the agency is serving a frozen
feed (curl the feed URL and read its header timestamp); if `ok` polls stopped,
read the poller logs.

### NoSuccessfulPolls (critical)

`sum by (feed) (rate(ridewatch_polls_total{outcome="ok"}[10m])) == 0` for 10m.
Not a single poll of the feed has succeeded in ten minutes against an expected
cadence of one per 15s — ingest for that feed is fully down, and FeedStalled
will follow shortly if it has not fired already. It predicts a network/DNS
problem, an agency outage, or a changed feed URL. First step: read the
poller's logged HTTP error, then curl the configured feed URL from the host
running RideWatch to see which side is broken.

### DroppedPollsHigh (warning)

`error` + `decode_error` outcomes exceed 20% of the expected 1-per-15s poll
cadence for 10m. Some polls still succeed, so the system limps along, but data
has gaps: missed prediction updates widen delay error and finalization freezes
events from older predictions than it should. It predicts flaky agency
infrastructure or a feed that intermittently publishes malformed protobuf.
First step: split `ridewatch_polls_total` by outcome — `error` points at
HTTP/network (check poller logs for status codes), `decode_error` points at
the payload (fetch the newest blob from the raw archive, gunzip, and decode it
locally; the blob is always archived even when decoding fails).

### IngestLagHigh (warning)

`histogram_quantile(0.95, sum by (feed, le)
(rate(ridewatch_ingest_lag_seconds_bucket[10m]))) > 60` for 10m. Polls succeed
but rows land in Postgres more than a minute after the feed produced them, so
"live" delays shown to riders trail reality and push alerts go out late — the
kind of latency riders notice before any error does. It predicts a slow or
contended database, or a reconcile engine falling behind. First step: compare
with poll duration p95 — if polls are fast, look at Postgres (locks, disk,
upsert batch sizes on the event-writes panel); if polls are slow too, the
problem is upstream of the database.

### ScheduleVersionStale (warning)

`ridewatch_schedule_version_age_days > 45` (held 1h to skip scrape blips).
Agencies republish static GTFS every few weeks; at 45 days the active version
is almost certainly obsolete, which shows up as a creeping rise in unmatched
trips, wrong scheduled times, and therefore wrong delays. It predicts a
failing static-refresh loop rather than an emergency. First step: check
`ridewatch_schedule_loads_total` for `error` outcomes and read the
load-static logs; verify `STATIC_GTFS_URL` still resolves to a fresh zip.

### PushFailureRatioHigh (warning)

More than 50% of Web Push sends over the last hour ended in outcome `error`,
guarded by a minimum of 10 attempted sends so a single failed notification at
3 AM cannot page anyone. `gone` outcomes (expired subscriptions, HTTP 404/410)
are normal churn and deliberately excluded from the numerator. It predicts a
bad or rotated VAPID key pair, a wrong VAPID subject, or a push-service
outage — riders who asked to be told about delays are not being told. First
step: read the evaluator logs for the HTTP status codes the push endpoints
return (401/403 means VAPID config, 5xx means the push service).

### RollupFailed (warning)

`increase(ridewatch_rollup_runs_total{outcome="error"}[24h]) > 0`. The nightly
rollup failed at least once in the last day, so the reliability heatmaps,
worst-departures tables, and plain-language sentences are frozen at the last
successful run — stale but not wrong, which is why this warns rather than
pages. It predicts a Postgres error inside one of the DELETE+INSERT
transactions (lock timeout, disk pressure, a partition problem) or a bad
agency timezone. First step: grep the app logs around the configured
`ROLLUP_HOUR_UTC` for the rollup error; the run is transactional per table, so
re-running after fixing the cause is safe.

### TargetDown (critical)

`up{job="ridewatch"} == 0` for 5m. Prometheus cannot scrape the app at all, so
every other alert is flying blind — polling, the API, and push evaluation are
all presumed stopped, and each minute down is a permanent hole in the archive
that replay cannot fill. First step: `kubectl -n ridewatch get pods` and read
the crash/restart logs; if the pod is healthy, verify the scrape config and
the network path to `/metrics` (which Caddy blocks externally by design —
Prometheus must scrape it in-cluster).

## Deploying

- Prometheus: mount `prometheus-rules.yaml` and list it under `rule_files`.
- Grafana: import `grafana-dashboard.json` (Dashboards → Import) or provision
  it from a dashboards directory; pick your Prometheus datasource in the
  dashboard's "Data source" variable.
