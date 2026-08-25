package ingest

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/proto"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
	"github.com/kevinkiyosepyo/ridewatch/internal/metrics"
)

type rawRecord struct {
	feed     domain.Feed
	polledAt time.Time
	feedTS   uint64
	sha      string
	relPath  string
	size     int
	entities int
}

// fakeArchive is an in-memory domain.RawArchive.
type fakeArchive struct {
	mu      sync.Mutex
	lastSHA string
	lastErr error
	records []rawRecord
}

func (f *fakeArchive) RecordRawPoll(_ context.Context, feed domain.Feed, polledAt time.Time, feedTS uint64, sha, relPath string, size, entities int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rawRecord{feed, polledAt, feedTS, sha, relPath, size, entities})
	f.lastSHA = sha
	return nil
}

func (f *fakeArchive) LastSHA(context.Context, domain.Feed) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSHA, f.lastErr
}

func (f *fakeArchive) all() []rawRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rawRecord(nil), f.records...)
}

// sinkRecorder collects snapshots and signals each arrival.
type sinkRecorder struct {
	mu    sync.Mutex
	snaps []*domain.Snapshot
	ch    chan struct{}
}

func newSinkRecorder() *sinkRecorder {
	return &sinkRecorder{ch: make(chan struct{}, 64)}
}

func (s *sinkRecorder) sink(_ context.Context, snap *domain.Snapshot) error {
	s.mu.Lock()
	s.snaps = append(s.snaps, snap)
	s.mu.Unlock()
	s.ch <- struct{}{}
	return nil
}

func (s *sinkRecorder) all() []*domain.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*domain.Snapshot(nil), s.snaps...)
}

func shaHexOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func pollsCount(feed domain.Feed, outcome string) float64 {
	return testutil.ToFloat64(metrics.Polls.WithLabelValues(string(feed), outcome))
}

// listArchivedFiles returns the slash-relative paths of all files under dir.
func listArchivedFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return files
}

func gunzipFile(t *testing.T, p string) []byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open %s: %v", p, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip %s: %v", p, err)
	}
	defer gz.Close()
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return raw
}

func serveBytes(payload *atomic.Pointer[[]byte]) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(*payload.Load())
	}))
}

func staticServer(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	var p atomic.Pointer[[]byte]
	p.Store(&payload)
	srv := serveBytes(&p)
	t.Cleanup(srv.Close)
	return srv
}

func TestPollerOKFlow(t *testing.T) {
	raw := vehicleEntities(t, 1756000100, &gtfs.VehiclePosition{
		Vehicle:  &gtfs.VehicleDescriptor{Id: proto.String("veh-1")},
		Position: &gtfs.Position{Latitude: proto.Float32(1), Longitude: proto.Float32(2)},
	})
	srv := staticServer(t, raw)
	dir := t.TempDir()
	fa := &fakeArchive{}
	sr := newSinkRecorder()
	p := NewPoller(PollerConfig{Feed: domain.FeedVehiclePositions, URL: srv.URL, Interval: time.Second, ArchiveDir: dir}, fa, sr.sink)

	okBefore := pollsCount(domain.FeedVehiclePositions, "ok")
	if err := p.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if got := pollsCount(domain.FeedVehiclePositions, "ok") - okBefore; got != 1 {
		t.Errorf("ok polls delta = %v, want 1", got)
	}

	snaps := sr.all()
	if len(snaps) != 1 {
		t.Fatalf("sink called %d times, want 1", len(snaps))
	}
	snap := snaps[0]
	wantSHA := shaHexOf(raw)
	if snap.RawSHA256 != wantSHA {
		t.Errorf("RawSHA256 = %s, want %s", snap.RawSHA256, wantSHA)
	}
	if snap.FeedTimestamp != 1756000100 {
		t.Errorf("FeedTimestamp = %d", snap.FeedTimestamp)
	}
	if len(snap.Vehicles) != 1 || snap.Vehicles[0].VehicleID != "veh-1" {
		t.Errorf("vehicles = %+v", snap.Vehicles)
	}

	// The blob is on disk at RawPath and gunzips back to the fetched bytes.
	files := listArchivedFiles(t, dir)
	if len(files) != 1 || files[0] != snap.RawPath {
		t.Fatalf("archived files = %v, RawPath = %s", files, snap.RawPath)
	}
	u := snap.PolledAt.UTC()
	wantPath := blobPath(domain.FeedVehiclePositions, u, wantSHA)
	if snap.RawPath != wantPath {
		t.Errorf("RawPath = %s, want %s", snap.RawPath, wantPath)
	}
	abs := filepath.Join(dir, filepath.FromSlash(snap.RawPath))
	if got := gunzipFile(t, abs); string(got) != string(raw) {
		t.Error("archived blob does not round-trip to the fetched bytes")
	}

	recs := fa.all()
	if len(recs) != 1 {
		t.Fatalf("RecordRawPoll called %d times, want 1", len(recs))
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	r := recs[0]
	if r.feed != domain.FeedVehiclePositions || r.sha != wantSHA || r.relPath != snap.RawPath ||
		r.feedTS != 1756000100 || r.entities != 1 || int64(r.size) != info.Size() {
		t.Errorf("raw poll record wrong: %+v (blob size %d)", r, info.Size())
	}
}

func TestPollerUnchangedSkip(t *testing.T) {
	raw := vehicleEntities(t, 1756000100, &gtfs.VehiclePosition{})
	srv := staticServer(t, raw)
	dir := t.TempDir()
	fa := &fakeArchive{lastSHA: shaHexOf(raw)}
	sr := newSinkRecorder()
	p := NewPoller(PollerConfig{Feed: domain.FeedVehiclePositions, URL: srv.URL, Interval: time.Second, ArchiveDir: dir}, fa, sr.sink)

	before := pollsCount(domain.FeedVehiclePositions, "unchanged")
	if err := p.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if got := pollsCount(domain.FeedVehiclePositions, "unchanged") - before; got != 1 {
		t.Errorf("unchanged polls delta = %v, want 1", got)
	}
	if len(sr.all()) != 0 {
		t.Error("sink must not be called on an unchanged poll")
	}
	if recs := fa.all(); len(recs) != 0 {
		t.Errorf("RecordRawPoll must not be called on an unchanged poll, got %+v", recs)
	}
	if files := listArchivedFiles(t, dir); len(files) != 0 {
		t.Errorf("no blob should be archived, got %v", files)
	}
}

func TestPollerArchiveBeforeDecode(t *testing.T) {
	payload := []byte{0xff, 0xff, 0xff, 0xff} // not a FeedMessage
	srv := staticServer(t, payload)
	dir := t.TempDir()
	fa := &fakeArchive{}
	sr := newSinkRecorder()
	p := NewPoller(PollerConfig{Feed: domain.FeedTripUpdates, URL: srv.URL, Interval: time.Second, ArchiveDir: dir}, fa, sr.sink)

	before := pollsCount(domain.FeedTripUpdates, "decode_error")
	if err := p.pollOnce(context.Background()); err == nil {
		t.Fatal("pollOnce on a corrupt payload: want error")
	}
	if got := pollsCount(domain.FeedTripUpdates, "decode_error") - before; got != 1 {
		t.Errorf("decode_error polls delta = %v, want 1", got)
	}

	// The corrupt blob still landed on disk...
	files := listArchivedFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("archived files = %v, want exactly the corrupt blob", files)
	}
	if got := gunzipFile(t, filepath.Join(dir, filepath.FromSlash(files[0]))); string(got) != string(payload) {
		t.Error("archived blob does not match the corrupt payload")
	}
	// ...and was indexed, with zero feed timestamp / entity count.
	recs := fa.all()
	if len(recs) != 1 {
		t.Fatalf("RecordRawPoll called %d times, want 1", len(recs))
	}
	if recs[0].relPath != files[0] || recs[0].sha != shaHexOf(payload) ||
		recs[0].feedTS != 0 || recs[0].entities != 0 {
		t.Errorf("raw poll record wrong: %+v", recs[0])
	}
	if len(sr.all()) != 0 {
		t.Error("sink must not be called when decode fails")
	}
}

func TestPollerLastSHAErrorStillArchives(t *testing.T) {
	raw := vehicleEntities(t, 42, &gtfs.VehiclePosition{})
	srv := staticServer(t, raw)
	dir := t.TempDir()
	fa := &fakeArchive{lastErr: errors.New("db down")}
	sr := newSinkRecorder()
	p := NewPoller(PollerConfig{Feed: domain.FeedVehiclePositions, URL: srv.URL, Interval: time.Second, ArchiveDir: dir}, fa, sr.sink)

	if err := p.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if files := listArchivedFiles(t, dir); len(files) != 1 {
		t.Errorf("blob must be archived even when LastSHA fails, got %v", files)
	}
}

func TestPollerRunSurvivesFailuresAndStopsOnCancel(t *testing.T) {
	raw := vehicleEntities(t, 7, &gtfs.VehiclePosition{})
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Write(raw)
	}))
	defer srv.Close()

	fa := &fakeArchive{}
	sr := newSinkRecorder()
	p := NewPoller(PollerConfig{Feed: domain.FeedVehiclePositions, URL: srv.URL, Interval: 20 * time.Millisecond, ArchiveDir: t.TempDir()}, fa, sr.sink)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	// The first poll fails with a 500; Run must keep ticking and eventually sink.
	select {
	case <-sr.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("sink never called; Run died on a poll error?")
	}
	if requests.Load() < 2 {
		t.Errorf("requests = %d, want the failed first poll plus a retry", requests.Load())
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after ctx cancel")
	}
}

func TestPollerRunPollsImmediately(t *testing.T) {
	raw := vehicleEntities(t, 7, &gtfs.VehiclePosition{})
	srv := staticServer(t, raw)
	sr := newSinkRecorder()
	// Interval of an hour: only an immediate first poll can reach the sink.
	p := NewPoller(PollerConfig{Feed: domain.FeedVehiclePositions, URL: srv.URL, Interval: time.Hour, ArchiveDir: t.TempDir()}, &fakeArchive{}, sr.sink)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	select {
	case <-sr.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no immediate first poll")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after ctx cancel")
	}
}

func TestNewPollerClientTimeout(t *testing.T) {
	p := NewPoller(PollerConfig{Feed: domain.FeedTripUpdates, Interval: 15 * time.Second}, &fakeArchive{}, nil)
	if p.client.Timeout != 10*time.Second {
		t.Errorf("timeout = %v, want capped at 10s", p.client.Timeout)
	}
	p = NewPoller(PollerConfig{Feed: domain.FeedTripUpdates, Interval: 5 * time.Second}, &fakeArchive{}, nil)
	if p.client.Timeout != 5*time.Second {
		t.Errorf("timeout = %v, want the 5s interval", p.client.Timeout)
	}
}
