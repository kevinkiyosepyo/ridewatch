// Package ingest polls the GTFS-Realtime feeds, archives every distinct raw
// response to disk (the blobs are the system of record), decodes protobuf into
// domain snapshots, and can replay or prune the archive.
package ingest

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
	"github.com/kevinkiyosepyo/ridewatch/internal/metrics"
)

// Sink receives each successfully decoded snapshot (the reconcile engine in
// production).
type Sink func(ctx context.Context, snap *domain.Snapshot) error

type PollerConfig struct {
	Feed       domain.Feed
	URL        string
	Interval   time.Duration
	ArchiveDir string // root; poller writes blobs under it
}

// Poller polls one GTFS-RT feed on a fixed interval.
type Poller struct {
	cfg    PollerConfig
	idx    domain.RawArchive
	sink   Sink
	client *http.Client
	log    *slog.Logger

	// Header timestamp of the last successfully decoded poll, so staleness
	// keeps rising while the upstream feed serves byte-identical responses.
	lastFeedTS uint64
}

func NewPoller(cfg PollerConfig, idx domain.RawArchive, sink Sink) *Poller {
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	return &Poller{
		cfg:    cfg,
		idx:    idx,
		sink:   sink,
		client: &http.Client{Timeout: min(cfg.Interval, 10*time.Second)},
		log:    slog.Default().With("feed", string(cfg.Feed)),
	}
}

// Run polls immediately and then on every tick until ctx is done. A failed
// poll is logged and counted, never fatal.
func (p *Poller) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()
	for {
		if err := p.pollOnce(ctx); err != nil && ctx.Err() == nil {
			p.log.Error("poll failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// pollOnce runs one poll cycle: fetch → sha256 → unchanged-skip → gzip archive
// to disk → RecordRawPoll → sink. The blob is durably on disk before any
// decode result is acted on, so a payload that fails to decode is still
// archived and indexed (outcome decode_error).
func (p *Poller) pollOnce(ctx context.Context) error {
	feed := string(p.cfg.Feed)
	start := time.Now()
	defer func() {
		metrics.PollDuration.WithLabelValues(feed).Observe(time.Since(start).Seconds())
	}()

	raw, err := p.fetch(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		metrics.Polls.WithLabelValues(feed, "error").Inc()
		return err
	}

	sum := sha256.Sum256(raw)
	shaHex := hex.EncodeToString(sum[:])

	// The raw archive is the system of record: a failed index lookup must not
	// prevent archiving, so treat it as "no previous blob" and carry on.
	last, err := p.idx.LastSHA(ctx, p.cfg.Feed)
	if err != nil {
		p.log.Warn("last-sha lookup failed, archiving regardless", "err", err)
		last = ""
	}
	if last != "" && last == shaHex {
		metrics.Polls.WithLabelValues(feed, "unchanged").Inc()
		if p.lastFeedTS > 0 {
			metrics.FeedStaleness.WithLabelValues(feed).Set(time.Since(time.Unix(int64(p.lastFeedTS), 0)).Seconds())
		}
		return nil
	}

	polledAt := start.UTC()
	relPath := blobPath(p.cfg.Feed, polledAt, shaHex)
	size, err := writeBlob(p.cfg.ArchiveDir, relPath, raw)
	if err != nil {
		metrics.Polls.WithLabelValues(feed, "error").Inc()
		return err
	}
	metrics.RawArchiveBytes.Add(float64(size))

	// Decode before indexing only so the index row carries the real header
	// timestamp and entity count; a decode failure still records the blob.
	snap, decErr := DecodeFeed(p.cfg.Feed, raw, polledAt)
	var feedTS uint64
	var entities int
	if decErr == nil {
		feedTS = snap.FeedTimestamp
		entities = len(snap.TripUpdates) + len(snap.Vehicles)
	}
	if err := p.idx.RecordRawPoll(ctx, p.cfg.Feed, polledAt, feedTS, shaHex, relPath, int(size), entities); err != nil {
		metrics.Polls.WithLabelValues(feed, "error").Inc()
		return fmt.Errorf("record raw poll: %w", err)
	}
	if decErr != nil {
		metrics.Polls.WithLabelValues(feed, "decode_error").Inc()
		return decErr
	}

	snap.RawSHA256 = shaHex
	snap.RawPath = relPath
	if p.sink != nil {
		if err := p.sink(ctx, snap); err != nil {
			metrics.Polls.WithLabelValues(feed, "error").Inc()
			return fmt.Errorf("sink: %w", err)
		}
	}

	metrics.Polls.WithLabelValues(feed, "ok").Inc()
	p.lastFeedTS = snap.FeedTimestamp
	if snap.FeedTimestamp > 0 {
		metrics.FeedStaleness.WithLabelValues(feed).Set(time.Since(time.Unix(int64(snap.FeedTimestamp), 0)).Seconds())
	}
	return nil
}

func (p *Poller) fetch(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", p.cfg.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %s", p.cfg.URL, resp.Status)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body %s: %w", p.cfg.URL, err)
	}
	return raw, nil
}

// blobPath is the archive layout: {feed}/{YYYY}/{MM}/{DD}/{HH}/{unix}_{sha8}.pb.gz
// with UTC path parts. Replay and PruneArchive derive feed and timestamp back
// out of this path alone, with no database.
func blobPath(feed domain.Feed, polledAt time.Time, shaHex string) string {
	u := polledAt.UTC()
	return path.Join(
		string(feed),
		fmt.Sprintf("%04d", u.Year()),
		fmt.Sprintf("%02d", int(u.Month())),
		fmt.Sprintf("%02d", u.Day()),
		fmt.Sprintf("%02d", u.Hour()),
		fmt.Sprintf("%d_%s.pb.gz", u.Unix(), shaHex[:8]),
	)
}

// writeBlob gzips raw into root/relPath via a temp file + rename and returns
// the compressed size on disk.
func writeBlob(root, relPath string, raw []byte) (int64, error) {
	abs := filepath.Join(root, filepath.FromSlash(relPath))
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("archive mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return 0, fmt.Errorf("archive temp file: %w", err)
	}
	tmpName := tmp.Name()

	gz, err := gzip.NewWriterLevel(tmp, gzip.BestSpeed)
	if err == nil {
		if _, werr := gz.Write(raw); werr != nil {
			err = werr
		}
		if cerr := gz.Close(); err == nil {
			err = cerr
		}
	}
	var size int64
	if err == nil {
		if info, serr := tmp.Stat(); serr != nil {
			err = serr
		} else {
			size = info.Size()
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmpName, abs)
	}
	if err != nil {
		os.Remove(tmpName)
		return 0, fmt.Errorf("archive %s: %w", relPath, err)
	}
	return size, nil
}
