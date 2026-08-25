// Package gtfsstatic fetches the static GTFS feed and streams its contents
// into a domain.ScheduleWriter.
package gtfsstatic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// Download fetches url to destDir/gtfs-<first 8 hex of sha256>.zip via a temp
// file + rename, computing the sha256 while streaming. The request is bounded
// by ctx (attach a deadline for a timeout). A non-200 response is an error.
// Returns the final file path and the full hex sha256 of the bytes.
func Download(ctx context.Context, url, destDir string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("gtfsstatic: build request for %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("gtfsstatic: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("gtfsstatic: fetch %s: unexpected status %s", url, resp.Status)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", "", fmt.Errorf("gtfsstatic: create dest dir: %w", err)
	}
	tmp, err := os.CreateTemp(destDir, "gtfs-*.tmp")
	if err != nil {
		return "", "", fmt.Errorf("gtfsstatic: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		tmp.Close() // double close after success is harmless
		if !keep {
			os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return "", "", fmt.Errorf("gtfsstatic: download %s: %w", url, err)
	}
	if err := tmp.Close(); err != nil {
		return "", "", fmt.Errorf("gtfsstatic: close temp file: %w", err)
	}

	sum := hex.EncodeToString(h.Sum(nil))
	final := filepath.Join(destDir, "gtfs-"+sum[:8]+".zip")
	if err := os.Rename(tmpName, final); err != nil {
		return "", "", fmt.Errorf("gtfsstatic: rename into place: %w", err)
	}
	keep = true
	return final, sum, nil
}
