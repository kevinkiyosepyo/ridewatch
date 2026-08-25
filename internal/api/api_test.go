package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/kevinkiyosepyo/ridewatch/internal/config"
	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// --- fakes ---

type fakeQueries struct {
	stops        map[string]domain.Stop
	routes       map[string]domain.Route
	routeList    []domain.Route
	routesByStop map[string][]domain.Route
	searchResult []domain.Stop
	bboxResult   []domain.Stop
	hourly       map[string][]domain.HourlyStat
	departures   map[string][]domain.DepartureStat
	routeHourly  map[string][]domain.HourlyStat

	lastSearch string
	lastBBox   [4]float64 // minLat, minLon, maxLat, maxLon
}

func (f *fakeQueries) SearchStops(_ context.Context, q string, _ int) ([]domain.Stop, error) {
	f.lastSearch = q
	return f.searchResult, nil
}

func (f *fakeQueries) StopsInBBox(_ context.Context, minLat, minLon, maxLat, maxLon float64, _ int) ([]domain.Stop, error) {
	f.lastBBox = [4]float64{minLat, minLon, maxLat, maxLon}
	return f.bboxResult, nil
}

func (f *fakeQueries) Stop(_ context.Context, stopID string) (*domain.Stop, error) {
	if s, ok := f.stops[stopID]; ok {
		return &s, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakeQueries) Routes(context.Context) ([]domain.Route, error) { return f.routeList, nil }

func (f *fakeQueries) Route(_ context.Context, routeID string) (*domain.Route, error) {
	if r, ok := f.routes[routeID]; ok {
		return &r, nil
	}
	return nil, domain.ErrNotFound
}

func (f *fakeQueries) RoutesServingStop(_ context.Context, stopID string) ([]domain.Route, error) {
	return f.routesByStop[stopID], nil
}

func (f *fakeQueries) StopHourly(_ context.Context, stopID string) ([]domain.HourlyStat, error) {
	return f.hourly[stopID], nil
}

func (f *fakeQueries) StopDepartures(_ context.Context, stopID string) ([]domain.DepartureStat, error) {
	return f.departures[stopID], nil
}

func (f *fakeQueries) RouteHourly(_ context.Context, routeID string) ([]domain.HourlyStat, error) {
	return f.routeHourly[routeID], nil
}

func (f *fakeQueries) RecentStopEvents(context.Context, string, int) ([]domain.StopEvent, error) {
	return nil, nil
}

type fakeLive struct {
	vehicles    []domain.LiveVehicle
	upcoming    map[string][]domain.StopEvent
	ages        map[domain.Feed]time.Duration
	panicOnCall bool
	lastHorizon time.Duration
}

func (f *fakeLive) LiveVehicles() []domain.LiveVehicle {
	if f.panicOnCall {
		panic("boom")
	}
	return f.vehicles
}

func (f *fakeLive) UpcomingAtStop(stopID string, horizon time.Duration) []domain.StopEvent {
	f.lastHorizon = horizon
	return f.upcoming[stopID]
}

func (f *fakeLive) FeedAge(feed domain.Feed) (time.Duration, bool) {
	age, ok := f.ages[feed]
	return age, ok
}

type fakeSubs struct {
	saved   []domain.Subscription
	deleted []string
	nextID  int64
	missing bool // DeleteSubscription returns ErrNotFound
}

func (f *fakeSubs) SaveSubscription(_ context.Context, s domain.Subscription) (int64, error) {
	f.nextID++
	f.saved = append(f.saved, s)
	return f.nextID, nil
}

func (f *fakeSubs) DeleteSubscription(_ context.Context, endpoint string) error {
	if f.missing {
		return domain.ErrNotFound
	}
	f.deleted = append(f.deleted, endpoint)
	return nil
}

func (f *fakeSubs) AllSubscriptions(context.Context) ([]domain.Subscription, error) { return nil, nil }

func (f *fakeSubs) MarkPushSent(context.Context, int64, domain.EventKey) (bool, error) {
	return true, nil
}

func (f *fakeSubs) RecordPushResult(context.Context, int64, bool, bool) error { return nil }

// --- fixtures ---

func intPtr(n int) *int { return &n }

func testStatic() fstest.MapFS {
	return fstest.MapFS{
		"index.html":         {Data: []byte("<html>index</html>")},
		"stop.html":          {Data: []byte("<html>stop</html>")},
		"app.js":             {Data: []byte("console.log('app')")},
		"sw.js":              {Data: []byte("self.addEventListener('push',()=>{})")},
		"style.css":          {Data: []byte("body{}")},
		"vendor/maplibre.js": {Data: []byte("var maplibregl={}")},
	}
}

func newTestServer(t *testing.T, q *fakeQueries, live *fakeLive, subs *fakeSubs) *httptest.Server {
	t.Helper()
	cfg := config.Config{
		TileURL:         "https://tiles.example/{z}/{x}/{y}.png",
		VAPIDPublicKey:  "pubkey",
		VAPIDPrivateKey: "privkey",
	}
	srv := httptest.NewServer(New(cfg, q, live, subs, testStatic()))
	t.Cleanup(srv.Close)
	return srv
}

func defaultFakes() (*fakeQueries, *fakeLive, *fakeSubs) {
	q := &fakeQueries{
		stops: map[string]domain.Stop{
			"S1": {StopID: "S1", Name: "Elm St", Lat: 42.1, Lon: -71.2},
		},
		routes: map[string]domain.Route{
			"1": {RouteID: "1", ShortName: "1", LongName: "Harvard - Nubian"},
		},
		routeList: []domain.Route{{RouteID: "1", ShortName: "1"}},
		routesByStop: map[string][]domain.Route{
			"S1": {{RouteID: "1", ShortName: "1"}},
		},
		hourly:      map[string][]domain.HourlyStat{},
		departures:  map[string][]domain.DepartureStat{},
		routeHourly: map[string][]domain.HourlyStat{},
	}
	live := &fakeLive{
		upcoming: map[string][]domain.StopEvent{},
		ages: map[domain.Feed]time.Duration{
			domain.FeedVehiclePositions: 12 * time.Second,
			domain.FeedTripUpdates:      18 * time.Second,
		},
	}
	return q, live, &fakeSubs{}
}

func doJSON(t *testing.T, method, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if len(data) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("invalid JSON body %q: %v", data, err)
		}
	}
	return resp, decoded
}

// --- endpoint tests ---

func TestVehiclesGeoJSON(t *testing.T) {
	q, live, subs := defaultFakes()
	updated := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	live.vehicles = []domain.LiveVehicle{
		{
			VehiclePosition: domain.VehiclePosition{
				VehicleID: "v1", RouteID: "1", Lat: 42.35, Lon: -71.06,
				Bearing: 90, Status: "IN_TRANSIT_TO",
			},
			RouteShortName: "1",
			Headsign:       "Harvard",
			DelaySecs:      intPtr(120),
			UpdatedAt:      updated,
		},
		{
			VehiclePosition: domain.VehiclePosition{VehicleID: "v2", Lat: 1, Lon: 2},
			UpdatedAt:       updated,
		},
	}
	srv := newTestServer(t, q, live, subs)

	resp, body := doJSON(t, "GET", srv.URL+"/api/vehicles", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if body["type"] != "FeatureCollection" {
		t.Errorf("type = %v", body["type"])
	}
	features := body["features"].([]any)
	if len(features) != 2 {
		t.Fatalf("features = %d, want 2", len(features))
	}
	f0 := features[0].(map[string]any)
	if f0["type"] != "Feature" {
		t.Errorf("feature type = %v", f0["type"])
	}
	geom := f0["geometry"].(map[string]any)
	coords := geom["coordinates"].([]any)
	if geom["type"] != "Point" || coords[0].(float64) != -71.06 || coords[1].(float64) != 42.35 {
		t.Errorf("geometry = %v", geom)
	}
	props := f0["properties"].(map[string]any)
	for k, want := range map[string]any{
		"vehicle_id":       "v1",
		"route_id":         "1",
		"route_short_name": "1",
		"headsign":         "Harvard",
		"delay_secs":       float64(120),
		"status":           "IN_TRANSIT_TO",
		"bearing":          float64(90),
		"updated_at":       "2026-08-24T12:00:00Z",
	} {
		if props[k] != want {
			t.Errorf("props[%q] = %v, want %v", k, props[k], want)
		}
	}
	p1 := features[1].(map[string]any)["properties"].(map[string]any)
	if v, present := p1["delay_secs"]; !present || v != nil {
		t.Errorf("unknown delay should be null, got %v (present=%v)", v, present)
	}
}

func TestStopsSearchAndBBox(t *testing.T) {
	q, live, subs := defaultFakes()
	q.searchResult = []domain.Stop{{StopID: "S1", Name: "Elm St"}}
	q.bboxResult = []domain.Stop{{StopID: "S2", Name: "Oak St"}}
	srv := newTestServer(t, q, live, subs)

	resp, body := doJSON(t, "GET", srv.URL+"/api/stops?q=elm", "")
	if resp.StatusCode != 200 {
		t.Fatalf("search status = %d", resp.StatusCode)
	}
	if q.lastSearch != "elm" {
		t.Errorf("search term = %q", q.lastSearch)
	}
	stops := body["stops"].([]any)
	if len(stops) != 1 || stops[0].(map[string]any)["stop_id"] != "S1" {
		t.Errorf("stops = %v", stops)
	}

	resp, body = doJSON(t, "GET", srv.URL+"/api/stops?bbox=-71.2,42.1,-71.0,42.4", "")
	if resp.StatusCode != 200 {
		t.Fatalf("bbox status = %d", resp.StatusCode)
	}
	// bbox param is minLon,minLat,maxLon,maxLat; StopsInBBox takes lat-first.
	if q.lastBBox != [4]float64{42.1, -71.2, 42.4, -71.0} {
		t.Errorf("bbox args = %v", q.lastBBox)
	}
	if body["stops"].([]any)[0].(map[string]any)["stop_id"] != "S2" {
		t.Errorf("bbox stops = %v", body["stops"])
	}

	for _, bad := range []string{
		"/api/stops",                            // neither param
		"/api/stops?bbox=1,2,3",                 // wrong arity
		"/api/stops?bbox=a,b,c,d",               // not numbers
		"/api/stops?bbox=-190,42,-71,43",        // out of range
		"/api/stops?bbox=-71.0,42.4,-71.2,42.1", // min > max
		"/api/stops?q=",                         // empty query
	} {
		resp, body := doJSON(t, "GET", srv.URL+bad, "")
		if resp.StatusCode != 400 {
			t.Errorf("%s status = %d, want 400", bad, resp.StatusCode)
		}
		if body["error"] == "" {
			t.Errorf("%s: missing error message", bad)
		}
	}
}

func TestStopByID(t *testing.T) {
	q, live, subs := defaultFakes()
	srv := newTestServer(t, q, live, subs)

	resp, body := doJSON(t, "GET", srv.URL+"/api/stops/S1", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	stop := body["stop"].(map[string]any)
	if stop["stop_id"] != "S1" || stop["name"] != "Elm St" {
		t.Errorf("stop = %v", stop)
	}
	routes := body["routes"].([]any)
	if len(routes) != 1 || routes[0].(map[string]any)["route_id"] != "1" {
		t.Errorf("routes = %v", routes)
	}

	resp, body = doJSON(t, "GET", srv.URL+"/api/stops/NOPE", "")
	if resp.StatusCode != 404 {
		t.Fatalf("missing stop status = %d, want 404", resp.StatusCode)
	}
	if body["error"] != "stop not found" {
		t.Errorf("error = %v", body["error"])
	}
}

func TestUpcoming(t *testing.T) {
	q, live, subs := defaultFakes()
	sched := time.Date(2026, 8, 24, 8, 12, 0, 0, time.UTC)
	live.upcoming["S1"] = []domain.StopEvent{
		{
			TripID: "t1", RouteID: "1", DirectionID: 0, StopSequence: 5,
			ScheduledArrival: sched, PredictedArrival: sched.Add(3 * time.Minute),
			DelaySecs: intPtr(180), VehicleID: "v1",
		},
		{TripID: "t2", RouteID: "1", DirectionID: 1, StopSequence: 2, Skipped: true},
	}
	srv := newTestServer(t, q, live, subs)

	resp, body := doJSON(t, "GET", srv.URL+"/api/stops/S1/upcoming", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if live.lastHorizon != 90*time.Minute {
		t.Errorf("horizon = %v, want 90m", live.lastHorizon)
	}
	ups := body["upcoming"].([]any)
	if len(ups) != 2 {
		t.Fatalf("upcoming = %d entries", len(ups))
	}
	u0 := ups[0].(map[string]any)
	if u0["trip_id"] != "t1" || u0["route_id"] != "1" || u0["route_short_name"] != "1" {
		t.Errorf("u0 = %v", u0)
	}
	if u0["scheduled"] != "2026-08-24T08:12:00Z" || u0["predicted"] != "2026-08-24T08:15:00Z" {
		t.Errorf("times = %v / %v", u0["scheduled"], u0["predicted"])
	}
	if u0["delay_secs"] != float64(180) || u0["vehicle_id"] != "v1" || u0["skipped"] != false {
		t.Errorf("u0 = %v", u0)
	}
	u1 := ups[1].(map[string]any)
	if u1["skipped"] != true || u1["scheduled"] != nil || u1["predicted"] != nil || u1["delay_secs"] != nil {
		t.Errorf("u1 = %v", u1)
	}

	resp, _ = doJSON(t, "GET", srv.URL+"/api/stops/NOPE/upcoming", "")
	if resp.StatusCode != 404 {
		t.Errorf("missing stop status = %d, want 404", resp.StatusCode)
	}
}

func TestStopReliability(t *testing.T) {
	q, live, subs := defaultFakes()
	q.hourly["S1"] = []domain.HourlyStat{
		{RouteID: "1", StopID: "S1", HourOfWeek: 8, N: 50, Late5Pct: 0.2, EarlyPct: 0.05, P50DelaySecs: intPtr(60)},
	}
	q.departures["S1"] = []domain.DepartureStat{
		{RouteID: "1", StopID: "S1", ScheduledSecs: 8*3600 + 12*60, DayClass: "weekday", N: 40, Late5Pct: 0.38},
	}
	srv := newTestServer(t, q, live, subs)

	resp, body := doJSON(t, "GET", srv.URL+"/api/stops/S1/reliability", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	hourly := body["hourly"].([]any)
	if len(hourly) != 1 {
		t.Fatalf("hourly = %v", hourly)
	}
	h0 := hourly[0].(map[string]any)
	if h0["hour_of_week"] != float64(8) || h0["n"] != float64(50) || h0["p50_delay_secs"] != float64(60) {
		t.Errorf("h0 = %v", h0)
	}
	deps := body["departures"].([]any)
	if len(deps) != 1 || deps[0].(map[string]any)["scheduled_secs"] != float64(8*3600+12*60) {
		t.Errorf("departures = %v", deps)
	}
	sentences := body["sentences"].([]any)
	want := []any{
		"The 8:12 AM Route 1 departure is 5+ minutes late on 38% of weekday mornings.",
		"Overall, 20% of tracked arrivals here run 5+ minutes late.",
	}
	if len(sentences) != len(want) || sentences[0] != want[0] || sentences[1] != want[1] {
		t.Errorf("sentences = %v, want %v", sentences, want)
	}

	resp, _ = doJSON(t, "GET", srv.URL+"/api/stops/NOPE/reliability", "")
	if resp.StatusCode != 404 {
		t.Errorf("missing stop status = %d, want 404", resp.StatusCode)
	}
}

func TestRoutes(t *testing.T) {
	q, live, subs := defaultFakes()
	q.routeHourly["1"] = []domain.HourlyStat{{RouteID: "1", HourOfWeek: 40, N: 30, Late5Pct: 0.1}}
	srv := newTestServer(t, q, live, subs)

	resp, body := doJSON(t, "GET", srv.URL+"/api/routes", "")
	if resp.StatusCode != 200 {
		t.Fatalf("routes status = %d", resp.StatusCode)
	}
	if routes := body["routes"].([]any); len(routes) != 1 {
		t.Errorf("routes = %v", routes)
	}

	resp, body = doJSON(t, "GET", srv.URL+"/api/routes/1/reliability", "")
	if resp.StatusCode != 200 {
		t.Fatalf("route reliability status = %d", resp.StatusCode)
	}
	if hourly := body["hourly"].([]any); len(hourly) != 1 {
		t.Errorf("hourly = %v", hourly)
	}
	if body["route"].(map[string]any)["route_id"] != "1" {
		t.Errorf("route = %v", body["route"])
	}

	resp, _ = doJSON(t, "GET", srv.URL+"/api/routes/NOPE/reliability", "")
	if resp.StatusCode != 404 {
		t.Errorf("missing route status = %d, want 404", resp.StatusCode)
	}
}

func TestFeedInfoAndHealthz(t *testing.T) {
	q, live, subs := defaultFakes()
	delete(live.ages, domain.FeedTripUpdates) // trip updates never polled
	srv := newTestServer(t, q, live, subs)

	resp, body := doJSON(t, "GET", srv.URL+"/api/feedinfo", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	feeds := body["feeds"].(map[string]any)
	vp := feeds["vehicle_positions"].(map[string]any)
	if vp["ok"] != true || vp["age_secs"] != float64(12) {
		t.Errorf("vehicle_positions = %v", vp)
	}
	tu := feeds["trip_updates"].(map[string]any)
	if tu["ok"] != false || tu["age_secs"] != nil {
		t.Errorf("trip_updates = %v", tu)
	}
	if body["tile_url"] != "https://tiles.example/{z}/{x}/{y}.png" {
		t.Errorf("tile_url = %v", body["tile_url"])
	}
	if body["push_enabled"] != true {
		t.Errorf("push_enabled = %v", body["push_enabled"])
	}

	resp, body = doJSON(t, "GET", srv.URL+"/healthz", "")
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
	if body["status"] != "ok" || body["feeds"] == nil {
		t.Errorf("healthz = %v", body)
	}
}

func TestFeedInfoPushDisabled(t *testing.T) {
	q, live, subs := defaultFakes()
	cfg := config.Config{TileURL: "t"} // no VAPID keys
	srv := httptest.NewServer(New(cfg, q, live, subs, testStatic()))
	defer srv.Close()

	_, body := doJSON(t, "GET", srv.URL+"/api/feedinfo", "")
	if body["push_enabled"] != false {
		t.Errorf("push_enabled = %v, want false", body["push_enabled"])
	}
}

func TestVAPIDPublicKey(t *testing.T) {
	q, live, subs := defaultFakes()
	srv := newTestServer(t, q, live, subs)
	resp, body := doJSON(t, "GET", srv.URL+"/api/vapid-public-key", "")
	if resp.StatusCode != 200 || body["key"] != "pubkey" {
		t.Errorf("status = %d, body = %v", resp.StatusCode, body)
	}
}

func TestSubscribe(t *testing.T) {
	valid := `{"endpoint":"https://push.example/abc","keys":{"p256dh":"pk","auth":"ak"},"stop_id":"S1","route_id":"1","direction_id":0,"threshold_secs":30}`

	t.Run("happy path clamps threshold", func(t *testing.T) {
		q, live, subs := defaultFakes()
		srv := newTestServer(t, q, live, subs)
		resp, body := doJSON(t, "POST", srv.URL+"/api/subscriptions", valid)
		if resp.StatusCode != 201 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if body["id"] != float64(1) {
			t.Errorf("id = %v", body["id"])
		}
		if len(subs.saved) != 1 {
			t.Fatalf("saved = %d", len(subs.saved))
		}
		got := subs.saved[0]
		if got.Endpoint != "https://push.example/abc" || got.P256dh != "pk" || got.Auth != "ak" ||
			got.StopID != "S1" || got.RouteID != "1" || got.DirectionID != 0 {
			t.Errorf("saved = %+v", got)
		}
		if got.ThresholdSecs != 60 {
			t.Errorf("threshold = %d, want clamped 60", got.ThresholdSecs)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		q, live, subs := defaultFakes()
		srv := newTestServer(t, q, live, subs)
		resp, _ := doJSON(t, "POST", srv.URL+"/api/subscriptions",
			`{"endpoint":"https://push.example/abc","keys":{"p256dh":"pk","auth":"ak"},"stop_id":"S1"}`)
		if resp.StatusCode != 201 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		got := subs.saved[0]
		if got.ThresholdSecs != 300 || got.DirectionID != -1 || got.RouteID != "" {
			t.Errorf("saved = %+v", got)
		}
	})

	t.Run("threshold clamped high", func(t *testing.T) {
		q, live, subs := defaultFakes()
		srv := newTestServer(t, q, live, subs)
		resp, _ := doJSON(t, "POST", srv.URL+"/api/subscriptions",
			`{"endpoint":"https://push.example/abc","keys":{"p256dh":"pk","auth":"ak"},"stop_id":"S1","threshold_secs":99999}`)
		if resp.StatusCode != 201 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if subs.saved[0].ThresholdSecs != 3600 {
			t.Errorf("threshold = %d, want 3600", subs.saved[0].ThresholdSecs)
		}
	})

	rejections := []struct {
		name string
		body string
		want int
	}{
		{"unknown field", `{"endpoint":"https://p.example/x","keys":{"p256dh":"pk","auth":"ak"},"stop_id":"S1","surprise":1}`, 400},
		{"http endpoint", `{"endpoint":"http://p.example/x","keys":{"p256dh":"pk","auth":"ak"},"stop_id":"S1"}`, 400},
		{"garbage endpoint", `{"endpoint":"::","keys":{"p256dh":"pk","auth":"ak"},"stop_id":"S1"}`, 400},
		{"missing keys", `{"endpoint":"https://p.example/x","keys":{"p256dh":"","auth":""},"stop_id":"S1"}`, 400},
		{"missing stop", `{"endpoint":"https://p.example/x","keys":{"p256dh":"pk","auth":"ak"}}`, 400},
		{"bad direction", `{"endpoint":"https://p.example/x","keys":{"p256dh":"pk","auth":"ak"},"stop_id":"S1","direction_id":7}`, 400},
		{"unknown stop", `{"endpoint":"https://p.example/x","keys":{"p256dh":"pk","auth":"ak"},"stop_id":"NOPE"}`, 404},
		{"not json", `hello`, 400},
		{"oversized body", `{"endpoint":"https://p.example/` + strings.Repeat("x", maxSubscriptionBody) + `"}`, 413},
	}
	for _, tt := range rejections {
		t.Run(tt.name, func(t *testing.T) {
			q, live, subs := defaultFakes()
			srv := newTestServer(t, q, live, subs)
			resp, body := doJSON(t, "POST", srv.URL+"/api/subscriptions", tt.body)
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			if body["error"] == "" {
				t.Errorf("missing error message")
			}
			if len(subs.saved) != 0 {
				t.Errorf("subscription saved on invalid input: %+v", subs.saved)
			}
		})
	}
}

func TestUnsubscribe(t *testing.T) {
	q, live, subs := defaultFakes()
	srv := newTestServer(t, q, live, subs)

	resp, _ := doJSON(t, "DELETE", srv.URL+"/api/subscriptions", `{"endpoint":"https://push.example/abc"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if len(subs.deleted) != 1 || subs.deleted[0] != "https://push.example/abc" {
		t.Errorf("deleted = %v", subs.deleted)
	}

	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/subscriptions", `{}`)
	if resp.StatusCode != 400 {
		t.Errorf("empty endpoint status = %d, want 400", resp.StatusCode)
	}

	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/subscriptions", `{"endpoint":"https://x","bogus":1}`)
	if resp.StatusCode != 400 {
		t.Errorf("unknown field status = %d, want 400", resp.StatusCode)
	}

	subs.missing = true
	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/subscriptions", `{"endpoint":"https://push.example/gone"}`)
	if resp.StatusCode != 404 {
		t.Errorf("missing subscription status = %d, want 404", resp.StatusCode)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	q, live, subs := defaultFakes()
	srv := newTestServer(t, q, live, subs)
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(data), "go_goroutines") {
		t.Errorf("metrics status = %d", resp.StatusCode)
	}
}

func TestStatic(t *testing.T) {
	q, live, subs := defaultFakes()
	srv := newTestServer(t, q, live, subs)

	tests := []struct {
		path        string
		wantStatus  int
		wantType    string
		wantContain string
	}{
		{"/", 200, "text/html; charset=utf-8", "index"},
		{"/index.html", 200, "text/html; charset=utf-8", "index"},
		{"/stop/S1", 200, "text/html; charset=utf-8", "stop"},
		{"/about", 200, "text/html; charset=utf-8", "index"}, // SPA fallback
		{"/app.js", 200, "text/javascript; charset=utf-8", "console"},
		{"/sw.js", 200, "text/javascript; charset=utf-8", "addEventListener"},
		{"/style.css", 200, "text/css; charset=utf-8", "body"},
		{"/vendor/maplibre.js", 200, "text/javascript; charset=utf-8", "maplibregl"},
		{"/missing.png", 404, "", ""},
	}
	for _, tt := range tests {
		resp, err := http.Get(srv.URL + tt.path)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tt.wantStatus {
			t.Errorf("%s status = %d, want %d", tt.path, resp.StatusCode, tt.wantStatus)
			continue
		}
		if tt.wantType != "" && resp.Header.Get("Content-Type") != tt.wantType {
			t.Errorf("%s Content-Type = %q, want %q", tt.path, resp.Header.Get("Content-Type"), tt.wantType)
		}
		if tt.wantContain != "" && !strings.Contains(string(data), tt.wantContain) {
			t.Errorf("%s body = %q", tt.path, data)
		}
	}

	// Traversal attempts must not escape the FS root.
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.URL.Opaque = "//" + req.URL.Host + "/../go.mod"
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		if resp.StatusCode == 200 {
			body, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(body), "module ") {
				t.Errorf("path traversal escaped the static FS")
			}
		}
		resp.Body.Close()
	}

	// POST to a static path is rejected.
	respPost, err := http.Post(srv.URL+"/index.html", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	respPost.Body.Close()
	if respPost.StatusCode != 405 {
		t.Errorf("POST static status = %d, want 405", respPost.StatusCode)
	}
}

func TestAPIUnknownPathIs404(t *testing.T) {
	q, live, subs := defaultFakes()
	srv := newTestServer(t, q, live, subs)
	resp, body := doJSON(t, "GET", srv.URL+"/api/definitely-not-real", "")
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if body["error"] != "not found" {
		t.Errorf("body = %v", body)
	}
}

func TestPanicRecoversTo500(t *testing.T) {
	q, live, subs := defaultFakes()
	live.panicOnCall = true
	srv := newTestServer(t, q, live, subs)
	resp, body := doJSON(t, "GET", srv.URL+"/api/vehicles", "")
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if body["error"] != "internal server error" {
		t.Errorf("body = %v", body)
	}
}
