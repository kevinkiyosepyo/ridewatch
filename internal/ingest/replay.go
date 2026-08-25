package ingest

import (
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
	"github.com/kevinkiyosepyo/ridewatch/internal/metrics"
)

// blobNameRe matches archived blob filenames: {unixseconds}_{sha8}.pb.gz.
var blobNameRe = regexp.MustCompile(`^(\d+)_[0-9a-f]{8}\.pb\.gz$`)

// blobRef is one archived poll, discovered from the path layout alone.
type blobRef struct {
	feed    domain.Feed
	unix    int64
	relPath string
}

// Replay walks archived blobs for both feeds, merges them in strict
// unix-timestamp order across feeds over [from, to] (inclusive), and feeds
// each through DecodeFeed into sink with PolledAt taken from the filename.
// Blobs that fail to read or decode are counted (metrics.Polls decode_error)
// and skipped. Returns the number of blobs successfully decoded and sunk.
func Replay(ctx context.Context, archiveDir string, from, to time.Time, sink Sink) (int, error) {
	blobs, err := discoverBlobs(archiveDir)
	if err != nil {
		return 0, err
	}
	fromU, toU := from.Unix(), to.Unix()
	n := 0
	for _, b := range blobs {
		if b.unix < fromU || b.unix > toU {
			continue
		}
		if err := ctx.Err(); err != nil {
			return n, err
		}
		snap, err := readBlob(archiveDir, b)
		if err != nil {
			metrics.Polls.WithLabelValues(string(b.feed), "decode_error").Inc()
			slog.Warn("replay: skipping undecodable blob", "path", b.relPath, "err", err)
			continue
		}
		if err := sink(ctx, snap); err != nil {
			return n, fmt.Errorf("replay sink %s: %w", b.relPath, err)
		}
		n++
	}
	return n, nil
}

// discoverBlobs finds every archive blob under root for the two known feeds,
// sorted by timestamp ascending (ties broken by path for determinism).
func discoverBlobs(root string) ([]blobRef, error) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	var out []blobRef
	err := fs.WalkDir(os.DirFS(root), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, ok := parseBlobPath(rel)
		if !ok {
			return nil
		}
		out = append(out, b)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk archive %s: %w", root, err)
	}
	slices.SortFunc(out, func(a, b blobRef) int {
		if a.unix != b.unix {
			return cmp.Compare(a.unix, b.unix)
		}
		return cmp.Compare(a.relPath, b.relPath)
	})
	return out, nil
}

// parseBlobPath derives feed and timestamp from a slash-separated relative
// archive path; ok is false for paths that are not blobs of a known feed.
func parseBlobPath(rel string) (blobRef, bool) {
	m := blobNameRe.FindStringSubmatch(path.Base(rel))
	if m == nil {
		return blobRef{}, false
	}
	var feed domain.Feed
	switch {
	case strings.HasPrefix(rel, string(domain.FeedTripUpdates)+"/"):
		feed = domain.FeedTripUpdates
	case strings.HasPrefix(rel, string(domain.FeedVehiclePositions)+"/"):
		feed = domain.FeedVehiclePositions
	default:
		return blobRef{}, false
	}
	unix, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return blobRef{}, false
	}
	return blobRef{feed: feed, unix: unix, relPath: rel}, true
}

// readBlob gunzips and decodes one archived blob into a snapshot, restoring
// RawPath, RawSHA256 (recomputed from the decompressed bytes), and PolledAt
// (from the filename timestamp).
func readBlob(root string, b blobRef) (*domain.Snapshot, error) {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(b.relPath)))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gunzip %s: %w", b.relPath, err)
	}
	raw, err := io.ReadAll(gz)
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("gunzip %s: %w", b.relPath, err)
	}
	snap, err := DecodeFeed(b.feed, raw, time.Unix(b.unix, 0).UTC())
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	snap.RawSHA256 = hex.EncodeToString(sum[:])
	snap.RawPath = b.relPath
	return snap, nil
}

// PruneArchive removes archived blobs whose path-derived timestamp is before
// olderThan, then removes directories left empty. Returns files removed.
func PruneArchive(archiveDir string, olderThan time.Time) (int, error) {
	if _, err := os.Stat(archiveDir); os.IsNotExist(err) {
		return 0, nil
	}
	cutoff := olderThan.Unix()
	removed := 0
	var dirs []string
	err := fs.WalkDir(os.DirFS(archiveDir), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != "." {
				dirs = append(dirs, rel)
			}
			return nil
		}
		m := blobNameRe.FindStringSubmatch(path.Base(rel))
		if m == nil {
			return nil
		}
		unix, perr := strconv.ParseInt(m[1], 10, 64)
		if perr != nil || unix >= cutoff {
			return nil
		}
		if rerr := os.Remove(filepath.Join(archiveDir, filepath.FromSlash(rel))); rerr != nil {
			return fmt.Errorf("prune %s: %w", rel, rerr)
		}
		removed++
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("prune walk %s: %w", archiveDir, err)
	}
	// Deepest first, so parents empty out as their children are removed.
	// os.Remove refuses non-empty directories, which is exactly what we want.
	slices.SortFunc(dirs, func(a, b string) int {
		return cmp.Compare(strings.Count(b, "/"), strings.Count(a, "/"))
	})
	for _, rel := range dirs {
		_ = os.Remove(filepath.Join(archiveDir, filepath.FromSlash(rel)))
	}
	return removed, nil
}
