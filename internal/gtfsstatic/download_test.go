package gtfsstatic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownload_OK(t *testing.T) {
	body := []byte("pretend this is a GTFS zip")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path, sum, err := Download(context.Background(), srv.URL, dir)
	if err != nil {
		t.Fatal(err)
	}

	wantSum := sha256.Sum256(body)
	wantHex := hex.EncodeToString(wantSum[:])
	if sum != wantHex {
		t.Errorf("sha = %s, want %s", sum, wantHex)
	}
	wantPath := filepath.Join(dir, "gtfs-"+wantHex[:8]+".zip")
	if path != wantPath {
		t.Errorf("path = %s, want %s", path, wantPath)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("file content mismatch")
	}

	// Temp file must be gone after the rename.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestDownload_CreatesDestDir(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
	}))
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "nested", "dest")
	if _, _, err := Download(context.Background(), srv.URL, dir); err != nil {
		t.Fatal(err)
	}
}

func TestDownload_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if _, _, err := Download(context.Background(), srv.URL, dir); err == nil {
		t.Fatal("no error for 500 response")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("files left behind after failed download: %v", entries)
	}
}

func TestDownload_ContextTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stall mid-body until the client gives up
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	dir := t.TempDir()
	if _, _, err := Download(ctx, srv.URL, dir); err == nil {
		t.Fatal("no error for timed-out download")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("files left behind after aborted download: %v", entries)
	}
}
