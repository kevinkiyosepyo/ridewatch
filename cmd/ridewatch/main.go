// Command ridewatch is the whole system in one static binary:
//
//	serve       poll the feeds, reconcile, serve the site (the long-running mode)
//	migrate     apply database migrations
//	load-static download the static GTFS zip and load it as a schedule version
//	replay      re-derive the dataset from archived raw blobs
//	vapid-keys  generate a Web Push VAPID key pair
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // agency timezones must resolve even on scratch containers

	"github.com/kevinkiyosepyo/ridewatch/internal/api"
	"github.com/kevinkiyosepyo/ridewatch/internal/config"
	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
	"github.com/kevinkiyosepyo/ridewatch/internal/gtfsstatic"
	"github.com/kevinkiyosepyo/ridewatch/internal/ingest"
	"github.com/kevinkiyosepyo/ridewatch/internal/metrics"
	"github.com/kevinkiyosepyo/ridewatch/internal/push"
	"github.com/kevinkiyosepyo/ridewatch/internal/reconcile"
	"github.com/kevinkiyosepyo/ridewatch/internal/rollup"
	"github.com/kevinkiyosepyo/ridewatch/internal/store"
	"github.com/kevinkiyosepyo/ridewatch/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd := os.Args[1]; cmd {
	case "serve":
		err = runServe(ctx)
	case "migrate":
		err = runMigrate(ctx)
	case "load-static":
		err = runLoadStatic(ctx)
	case "replay":
		err = runReplay(ctx, os.Args[2:])
	case "vapid-keys":
		err = runVAPIDKeys()
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("ridewatch failed", "cmd", os.Args[1], "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: ridewatch <command>

  serve                     run pollers, reconciler, rollups, push, and the HTTP server
  migrate                   apply database migrations
  load-static               fetch the static GTFS zip and load it as a new schedule version
  replay --from T --to T    re-derive stop events from the raw archive (RFC3339 or YYYY-MM-DD)
  vapid-keys                generate a Web Push VAPID key pair

Configuration is environment variables; see internal/config.
`)
}

func openStore(ctx context.Context) (config.Config, *store.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return cfg, nil, err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, st, nil
}

func runMigrate(ctx context.Context) error {
	_, st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	slog.Info("migrations applied")
	return nil
}

func runVAPIDKeys() error {
	pub, priv, err := push.GenerateVAPIDKeys()
	if err != nil {
		return err
	}
	fmt.Printf("VAPID_PUBLIC_KEY=%s\nVAPID_PRIVATE_KEY=%s\n", pub, priv)
	return nil
}

func runLoadStatic(ctx context.Context) error {
	cfg, st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	return loadStatic(ctx, cfg, st)
}

// loadStatic downloads the static GTFS zip and loads it as a new active
// schedule version; a zip already loaded (same sha256) is a no-op. The zips
// live next to the raw archive: together they are the system of record.
func loadStatic(ctx context.Context, cfg config.Config, st *store.Store) error {
	destDir := filepath.Join(cfg.ArchiveDir, "static")
	zipPath, sha, err := gtfsstatic.Download(ctx, cfg.StaticGTFSURL, destDir)
	if err != nil {
		metrics.ScheduleLoads.WithLabelValues("error").Inc()
		return fmt.Errorf("download static GTFS: %w", err)
	}

	load, err := st.NewScheduleLoad(ctx, sha, time.Now())
	if errors.Is(err, store.ErrVersionExists) {
		metrics.ScheduleLoads.WithLabelValues("unchanged").Inc()
		slog.Info("static GTFS unchanged", "sha256", sha[:8])
		return nil
	}
	if err != nil {
		metrics.ScheduleLoads.WithLabelValues("error").Inc()
		return err
	}
	if err := gtfsstatic.ParseZip(zipPath, load); err != nil {
		_ = load.Abort(ctx)
		metrics.ScheduleLoads.WithLabelValues("error").Inc()
		return fmt.Errorf("parse static GTFS: %w", err)
	}
	id, err := load.Commit(ctx)
	if err != nil {
		_ = load.Abort(ctx)
		metrics.ScheduleLoads.WithLabelValues("error").Inc()
		return fmt.Errorf("commit static GTFS load: %w", err)
	}
	metrics.ScheduleLoads.WithLabelValues("loaded").Inc()
	slog.Info("static GTFS loaded", "version", id, "sha256", sha[:8])
	return nil
}

func runReplay(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	fromS := fs.String("from", "", "start of the range (RFC3339 or YYYY-MM-DD), inclusive")
	toS := fs.String("to", "", "end of the range (RFC3339 or YYYY-MM-DD), inclusive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	from, err := parseWhen(*fromS, false)
	if err != nil {
		return fmt.Errorf("--from: %w", err)
	}
	to, err := parseWhen(*toS, true)
	if err != nil {
		return fmt.Errorf("--to: %w", err)
	}

	cfg, st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.EnsurePartitions(ctx, from.AddDate(0, 0, -1), to.AddDate(0, 0, 7)); err != nil {
		return err
	}

	engine := reconcile.NewEngine(st, st, reconcile.Options{FinalizeGrace: cfg.FinalizeGrace})
	n, err := ingest.Replay(ctx, cfg.ArchiveDir, from, to, engine.Process)
	if err != nil {
		return err
	}
	finalized, err := st.FinalizeDue(ctx, time.Now(), cfg.FinalizeGrace)
	if err != nil {
		return err
	}
	fmt.Printf("replayed %d blobs, finalized %d stop events\n", n, finalized)
	return nil
}

// parseWhen accepts RFC3339 instants and bare dates; a bare date means the
// start of that UTC day, or its end when it is the --to bound.
func parseWhen(s string, endOfDay bool) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("required")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("not RFC3339 or YYYY-MM-DD: %q", s)
	}
	if endOfDay {
		t = t.AddDate(0, 0, 1).Add(-time.Second)
	}
	return t, nil
}

func runServe(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cfg, st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	now := time.Now()
	if err := st.EnsurePartitions(ctx, now.AddDate(0, 0, -7), now.AddDate(0, 0, 28)); err != nil {
		return err
	}

	// First boot: make `make db-up && make migrate && make run` really work by
	// loading the static schedule when none is active yet.
	if _, err := st.ActiveVersion(ctx); errors.Is(err, domain.ErrNotFound) {
		slog.Info("no active schedule version, loading static GTFS", "url", cfg.StaticGTFSURL)
		if err := loadStatic(ctx, cfg, st); err != nil {
			return fmt.Errorf("initial static GTFS load: %w", err)
		}
	} else if err != nil {
		return err
	}
	updateScheduleAge(ctx, st)

	engine := reconcile.NewEngine(st, st, reconcile.Options{FinalizeGrace: cfg.FinalizeGrace})

	var wg sync.WaitGroup
	for _, pc := range []ingest.PollerConfig{
		{Feed: domain.FeedVehiclePositions, URL: cfg.VehiclePositionsURL, Interval: cfg.PollInterval, ArchiveDir: cfg.ArchiveDir},
		{Feed: domain.FeedTripUpdates, URL: cfg.TripUpdatesURL, Interval: cfg.PollInterval, ArchiveDir: cfg.ArchiveDir},
	} {
		p := ingest.NewPoller(pc, st, engine.Process)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Run(ctx) // returns only when ctx is done
		}()
	}

	goEvery(ctx, &wg, 30*time.Second, func(ctx context.Context) {
		if err := engine.Sweep(ctx); err != nil && ctx.Err() == nil {
			slog.Error("sweep failed", "err", err)
		}
	})

	goEvery(ctx, &wg, cfg.StaticRefreshEvery, func(ctx context.Context) {
		if err := loadStatic(ctx, cfg, st); err != nil && ctx.Err() == nil {
			slog.Error("static GTFS refresh failed", "err", err)
		}
		updateScheduleAge(ctx, st)
	})

	if cfg.VAPIDPublicKey != "" && cfg.VAPIDPrivateKey != "" {
		ev := push.NewEvaluator(push.Config{
			VAPIDPublic:  cfg.VAPIDPublicKey,
			VAPIDPrivate: cfg.VAPIDPrivateKey,
			Subject:      cfg.VAPIDSubject,
			Horizon:      cfg.PushHorizon,
		}, st, engine, st)
		goEvery(ctx, &wg, time.Minute, func(ctx context.Context) {
			if err := ev.Evaluate(ctx); err != nil && ctx.Err() == nil {
				slog.Error("push evaluation failed", "err", err)
			}
		})
	} else {
		slog.Info("web push disabled: VAPID keys not set (generate with `ridewatch vapid-keys`)")
	}

	startNightly(ctx, &wg, cfg, st)

	handler := api.New(cfg, st, engine, st, web.FS)
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	slog.Info("ridewatch serving", "addr", cfg.ListenAddr, "public_url", cfg.PublicURL)

	var runErr error
	select {
	case <-ctx.Done():
		shCtx, shCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = srv.Shutdown(shCtx)
		shCancel()
	case err := <-serveErr:
		runErr = fmt.Errorf("http server: %w", err)
		cancel()
	}
	wg.Wait()
	return runErr
}

// startNightly runs the once-a-day work: rollups at ROLLUP_HOUR_UTC, then
// retention (partitions ahead, old partitions dropped, raw archive pruned)
// the hour after. A minute tick with per-day latches keeps it simple and
// restart-safe; failures alert via metrics and retry the next day.
func startNightly(ctx context.Context, wg *sync.WaitGroup, cfg config.Config, st *store.Store) {
	var rollupDay, retentionDay string
	goEvery(ctx, wg, time.Minute, func(ctx context.Context) {
		now := time.Now().UTC()
		day := now.Format("2006-01-02")

		if now.Hour() == cfg.RollupHourUTC && rollupDay != day {
			rollupDay = day
			ver, err := st.ActiveVersion(ctx)
			if err != nil {
				slog.Error("rollup skipped: no active schedule version", "err", err)
				return
			}
			start := time.Now()
			if err := rollup.Run(ctx, st.Pool(), ver.AgencyTZ, cfg.RollupWindowWeeks); err != nil {
				slog.Error("rollup failed", "err", err)
			} else {
				slog.Info("rollup complete", "took", time.Since(start).Round(time.Second))
			}
		}

		if now.Hour() == (cfg.RollupHourUTC+1)%24 && retentionDay != day {
			retentionDay = day
			if err := st.EnsurePartitions(ctx, now, now.AddDate(0, 0, 28)); err != nil {
				slog.Error("ensure partitions failed", "err", err)
			}
			if err := st.DropOldPartitions(ctx, map[string]int{
				"stop_events":       cfg.StopEventRetentionWeeks,
				"vehicle_positions": cfg.VehiclePosRetentionWeeks,
			}); err != nil {
				slog.Error("drop old partitions failed", "err", err)
			}
			if cfg.RawRetentionDays > 0 {
				removed, err := ingest.PruneArchive(cfg.ArchiveDir, now.AddDate(0, 0, -cfg.RawRetentionDays))
				if err != nil {
					slog.Error("prune raw archive failed", "err", err)
				} else if removed > 0 {
					slog.Info("pruned raw archive", "files", removed)
				}
			}
			updateScheduleAge(ctx, st)
		}
	})
}

func updateScheduleAge(ctx context.Context, st *store.Store) {
	if ver, err := st.ActiveVersion(ctx); err == nil {
		metrics.ScheduleVersionAge.Set(time.Since(ver.FetchedAt).Hours() / 24)
	}
}

// goEvery runs fn on every tick of d until ctx is done.
func goEvery(ctx context.Context, wg *sync.WaitGroup, d time.Duration, fn func(context.Context)) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				fn(ctx)
			}
		}
	}()
}
