package ingest

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeArchiveFile creates one file at rel with n bytes of content.
func writeArchiveFile(t *testing.T, root, rel string, n int) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestArchiveSize measures what actually fills the volume. The archive holds
// more than prunable blobs -- the static GTFS zips live there too and are
// never pruned -- so the gauge has to count the whole tree, not just what
// PruneArchive would match.
func TestArchiveSize(t *testing.T) {
	root := t.TempDir()

	if got, err := ArchiveSize(filepath.Join(root, "absent")); err != nil || got != 0 {
		t.Errorf("ArchiveSize(missing dir) = %d, %v; want 0, nil", got, err)
	}
	if got, err := ArchiveSize(root); err != nil || got != 0 {
		t.Errorf("ArchiveSize(empty) = %d, %v; want 0, nil", got, err)
	}

	writeArchiveFile(t, root, "trip_updates/2026/08/25/06/1787638611_d699ecea.pb.gz", 1000)
	writeArchiveFile(t, root, "vehicle_positions/2026/08/25/06/1787638611_aabbccdd.pb.gz", 500)
	writeArchiveFile(t, root, "static/gtfs-2cff599d.zip", 2000) // never pruned, still occupies the disk

	got, err := ArchiveSize(root)
	if err != nil {
		t.Fatalf("ArchiveSize: %v", err)
	}
	if got != 3500 {
		t.Errorf("ArchiveSize = %d, want 3500 (blobs + the static zip)", got)
	}
}

// TestPruneArchiveKeepsStaticZips guards the retention boundary the nightly
// job depends on: timestamped blobs older than the cutoff go, newer ones stay,
// emptied directories are swept, and the static zips -- which carry no
// timestamp in their name and are half the system of record -- survive any
// cutoff.
func TestPruneArchiveKeepsStaticZips(t *testing.T) {
	root := t.TempDir()
	old := time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 9, 16, 6, 0, 0, 0, time.UTC)

	oldBlob := "trip_updates/2026/08/25/06/" + strconv.FormatInt(old.Unix(), 10) + "_d699ecea.pb.gz"
	newBlob := "trip_updates/2026/09/16/06/" + strconv.FormatInt(recent.Unix(), 10) + "_aabbccdd.pb.gz"
	writeArchiveFile(t, root, oldBlob, 10)
	writeArchiveFile(t, root, newBlob, 10)
	writeArchiveFile(t, root, "static/gtfs-2cff599d.zip", 10)

	removed, err := PruneArchive(root, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("PruneArchive: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(oldBlob))); !os.IsNotExist(err) {
		t.Errorf("old blob survived the cutoff")
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(newBlob))); err != nil {
		t.Errorf("blob newer than the cutoff was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "static", "gtfs-2cff599d.zip")); err != nil {
		t.Errorf("static zip was pruned; replay can no longer reconstruct: %v", err)
	}
	// The emptied August directory should not linger.
	if _, err := os.Stat(filepath.Join(root, "trip_updates", "2026", "08")); !os.IsNotExist(err) {
		t.Errorf("emptied directory was left behind")
	}

	// Pruning an archive with nothing old enough is a no-op.
	if removed, err := PruneArchive(root, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)); err != nil || removed != 0 {
		t.Errorf("second prune = %d, %v; want 0, nil", removed, err)
	}
}
