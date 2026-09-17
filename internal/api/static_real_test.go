package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kevinkiyosepyo/ridewatch/internal/config"
	"github.com/kevinkiyosepyo/ridewatch/web"
)

// TestRealBundleRevalidates runs the caching contract against the actual
// embedded frontend rather than a fixture, because the cost being avoided is
// specific to the real tree: web/vendor/maplibre-gl.js is ~800 KB and every
// page load pulls it. A fixture of six tiny files would pass this test while
// the shipped site still re-sent a megabyte per visit.
func TestRealBundleRevalidates(t *testing.T) {
	q, live, subs := defaultFakes()
	srv := httptest.NewServer(New(config.Config{}, q, live, subs, web.FS))
	t.Cleanup(srv.Close)

	const bundle = "/vendor/maplibre-gl.js"
	resp, err := http.Get(srv.URL + bundle)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%s = %d, want 200", bundle, resp.StatusCode)
	}
	if len(body) < 500_000 {
		t.Fatalf("%s is %d bytes; expected the real vendored bundle — has it moved?", bundle, len(body))
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("%s has no ETag: every visit re-downloads %d bytes", bundle, len(body))
	}

	req, _ := http.NewRequest("GET", srv.URL+bundle, nil)
	req.Header.Set("If-None-Match", etag)
	cond, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	condBody, _ := io.ReadAll(cond.Body)
	cond.Body.Close()
	if cond.StatusCode != http.StatusNotModified {
		t.Errorf("repeat visit = %d, want 304 (%d bytes resent)", cond.StatusCode, len(body))
	}
	if len(condBody) != 0 {
		t.Errorf("304 carried %d bytes", len(condBody))
	}
	t.Logf("repeat visit: %d bytes -> %d bytes", len(body), len(condBody))
}
