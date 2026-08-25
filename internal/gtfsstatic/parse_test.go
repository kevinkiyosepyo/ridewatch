package gtfsstatic

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

type zipEntry struct{ name, body string }

// buildZipOrdered writes a zip fixture with entries in the given archive order.
func buildZipOrdered(t *testing.T, entries []zipEntry) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		f, err := zw.Create(e.name)
		if err != nil {
			t.Fatalf("zip create %s: %v", e.name, err)
		}
		if _, err := f.Write([]byte(e.body)); err != nil {
			t.Fatalf("zip write %s: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	p := filepath.Join(t.TempDir(), "fixture.zip")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func buildZip(t *testing.T, files map[string]string) string {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	entries := make([]zipEntry, 0, len(names))
	for _, n := range names {
		entries = append(entries, zipEntry{n, files[n]})
	}
	return buildZipOrdered(t, entries)
}

// recorder implements domain.ScheduleWriter, recording call order and rows.
type recorder struct {
	calls       []string
	feedVersion string
	agencyTZ    string
	routes      []domain.Route
	stops       []domain.Stop
	trips       []domain.Trip
	stopTimes   []domain.StopTime
	cals        []domain.CalendarEntry
	calDates    []domain.CalendarDate
	failOn      string
	failErr     error
}

func (r *recorder) mark(name string) error {
	r.calls = append(r.calls, name)
	if r.failOn == name {
		return r.failErr
	}
	return nil
}

func (r *recorder) Meta(fv, tz string) error {
	r.feedVersion, r.agencyTZ = fv, tz
	return r.mark("meta")
}
func (r *recorder) Route(x domain.Route) error {
	r.routes = append(r.routes, x)
	return r.mark("route")
}
func (r *recorder) Stop(x domain.Stop) error { r.stops = append(r.stops, x); return r.mark("stop") }
func (r *recorder) Trip(x domain.Trip) error { r.trips = append(r.trips, x); return r.mark("trip") }
func (r *recorder) StopTime(x domain.StopTime) error {
	r.stopTimes = append(r.stopTimes, x)
	return r.mark("stoptime")
}
func (r *recorder) Calendar(x domain.CalendarEntry) error {
	r.cals = append(r.cals, x)
	return r.mark("calendar")
}
func (r *recorder) CalendarDate(x domain.CalendarDate) error {
	r.calDates = append(r.calDates, x)
	return r.mark("calendardate")
}

func baseFixture() map[string]string {
	return map[string]string{
		"agency.txt":    "agency_id,agency_name,agency_url,agency_timezone\n1,MBTA,https://mbta.com,America/New_York\n",
		"feed_info.txt": "feed_publisher_name,feed_publisher_url,feed_lang,feed_version\nMBTA,https://mbta.com,en,2026-08-22\n",
		"routes.txt":    "route_id,route_short_name,route_long_name,route_type,route_color,route_text_color,route_sort_order\nRed,,Red Line,1,DA291C,FFFFFF,10\n",
		"stops.txt":     "stop_id,stop_name,stop_lat,stop_lon,location_type,parent_station,platform_code\nplace-alfcl,Alewife,42.3954,-71.1425,1,,\n70061,Alewife,42.3954,-71.1425,0,place-alfcl,1\n",
		"trips.txt":     "route_id,service_id,trip_id,trip_headsign,direction_id,shape_id\nRed,weekday,trip-1,Ashmont,0,shape-1\n",
		"stop_times.txt": "trip_id,arrival_time,departure_time,stop_id,stop_sequence\n" +
			"trip-1,08:00:00,08:01:00,70061,1\n" +
			"trip-1,25:30:00,,70062,2\n",
		"calendar.txt":       "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\nweekday,1,1,1,1,1,0,0,20260801,20261231\n",
		"calendar_dates.txt": "service_id,date,exception_type\nweekday,20260904,2\n",
	}
}

func TestParseZip_FullFeed(t *testing.T) {
	rec := &recorder{}
	if err := ParseZip(buildZip(t, baseFixture()), rec); err != nil {
		t.Fatal(err)
	}
	if rec.feedVersion != "2026-08-22" || rec.agencyTZ != "America/New_York" {
		t.Errorf("meta = (%q, %q)", rec.feedVersion, rec.agencyTZ)
	}
	if len(rec.calls) == 0 || rec.calls[0] != "meta" {
		t.Fatalf("first call = %v, want meta", rec.calls)
	}
	if len(rec.routes) != 1 || len(rec.stops) != 2 || len(rec.trips) != 1 ||
		len(rec.stopTimes) != 2 || len(rec.cals) != 1 || len(rec.calDates) != 1 {
		t.Fatalf("row counts: routes=%d stops=%d trips=%d stopTimes=%d cals=%d calDates=%d",
			len(rec.routes), len(rec.stops), len(rec.trips), len(rec.stopTimes), len(rec.cals), len(rec.calDates))
	}
	wantRoute := domain.Route{RouteID: "Red", LongName: "Red Line", RouteType: 1, Color: "DA291C", TextColor: "FFFFFF", SortOrder: 10}
	if rec.routes[0] != wantRoute {
		t.Errorf("route = %+v, want %+v", rec.routes[0], wantRoute)
	}
	wantStop := domain.Stop{StopID: "70061", Name: "Alewife", Lat: 42.3954, Lon: -71.1425, ParentStation: "place-alfcl", LocationType: 0, PlatformCode: "1"}
	if rec.stops[1] != wantStop {
		t.Errorf("stop = %+v, want %+v", rec.stops[1], wantStop)
	}
	wantTrip := domain.Trip{TripID: "trip-1", RouteID: "Red", ServiceID: "weekday", Headsign: "Ashmont", DirectionID: 0, ShapeID: "shape-1"}
	if rec.trips[0] != wantTrip {
		t.Errorf("trip = %+v, want %+v", rec.trips[0], wantTrip)
	}
	wantCal := domain.CalendarEntry{ServiceID: "weekday", Weekdays: [7]bool{true, true, true, true, true, false, false}, StartDate: "20260801", EndDate: "20261231"}
	if rec.cals[0] != wantCal {
		t.Errorf("calendar = %+v, want %+v", rec.cals[0], wantCal)
	}
	wantCD := domain.CalendarDate{ServiceID: "weekday", Date: "20260904", ExceptionType: 2}
	if rec.calDates[0] != wantCD {
		t.Errorf("calendar date = %+v, want %+v", rec.calDates[0], wantCD)
	}
}

func TestParseZip_BOMOnFirstHeaderCell(t *testing.T) {
	files := baseFixture()
	files["agency.txt"] = "\ufeff" + files["agency.txt"]
	files["stops.txt"] = "\ufeff" + files["stops.txt"]
	rec := &recorder{}
	if err := ParseZip(buildZip(t, files), rec); err != nil {
		t.Fatal(err)
	}
	if rec.agencyTZ != "America/New_York" {
		t.Errorf("agencyTZ = %q (BOM not stripped?)", rec.agencyTZ)
	}
	if len(rec.stops) != 2 || rec.stops[0].StopID != "place-alfcl" {
		t.Errorf("stops parsed under BOM header = %+v", rec.stops)
	}
}

func TestParseZip_ShuffledAndUnknownColumns(t *testing.T) {
	files := baseFixture()
	files["routes.txt"] = "wheelchair_boarding,route_type,route_id,route_long_name,mystery_column\n1,3,66,Harvard - Nubian,whatever\n"
	files["trips.txt"] = "shape_id,trip_id,block_id,direction_id,service_id,route_id\ns-1,trip-1,b-1,1,weekday,66\n"
	rec := &recorder{}
	if err := ParseZip(buildZip(t, files), rec); err != nil {
		t.Fatal(err)
	}
	wantRoute := domain.Route{RouteID: "66", LongName: "Harvard - Nubian", RouteType: 3}
	if rec.routes[0] != wantRoute {
		t.Errorf("route = %+v, want %+v", rec.routes[0], wantRoute)
	}
	wantTrip := domain.Trip{TripID: "trip-1", RouteID: "66", ServiceID: "weekday", DirectionID: 1, ShapeID: "s-1"}
	if rec.trips[0] != wantTrip {
		t.Errorf("trip = %+v, want %+v", rec.trips[0], wantTrip)
	}
}

func TestParseZip_TimesPast24HAndEmpty(t *testing.T) {
	rec := &recorder{}
	if err := ParseZip(buildZip(t, baseFixture()), rec); err != nil {
		t.Fatal(err)
	}
	st := rec.stopTimes[1]
	if st.ArrivalSecs != 25*3600+30*60 {
		t.Errorf("25:30:00 arrival = %d, want %d", st.ArrivalSecs, 25*3600+30*60)
	}
	if st.DepartureSecs != -1 {
		t.Errorf("empty departure = %d, want -1", st.DepartureSecs)
	}
}

func TestParseZip_MissingOptionalFiles(t *testing.T) {
	files := baseFixture()
	delete(files, "feed_info.txt")
	delete(files, "calendar.txt")
	delete(files, "calendar_dates.txt")
	rec := &recorder{}
	if err := ParseZip(buildZip(t, files), rec); err != nil {
		t.Fatal(err)
	}
	if rec.feedVersion != "" {
		t.Errorf("feedVersion = %q, want \"\"", rec.feedVersion)
	}
	if rec.agencyTZ != "America/New_York" {
		t.Errorf("agencyTZ = %q", rec.agencyTZ)
	}
	if len(rec.cals) != 0 || len(rec.calDates) != 0 {
		t.Errorf("unexpected calendar callbacks: %d, %d", len(rec.cals), len(rec.calDates))
	}
}

func TestParseZip_MissingOptionalColumns(t *testing.T) {
	files := baseFixture()
	files["trips.txt"] = "route_id,service_id,trip_id\nRed,weekday,trip-1\n"
	files["stops.txt"] = "stop_id,stop_name\n70061,Alewife\n"
	files["routes.txt"] = "route_id\nRed\n"
	files["feed_info.txt"] = "feed_publisher_name\nMBTA\n" // no feed_version column
	rec := &recorder{}
	if err := ParseZip(buildZip(t, files), rec); err != nil {
		t.Fatal(err)
	}
	if got := rec.trips[0].DirectionID; got != -1 {
		t.Errorf("missing direction_id = %d, want -1", got)
	}
	wantStop := domain.Stop{StopID: "70061", Name: "Alewife"}
	if rec.stops[0] != wantStop {
		t.Errorf("stop = %+v, want %+v", rec.stops[0], wantStop)
	}
	if rec.feedVersion != "" {
		t.Errorf("feedVersion = %q, want \"\"", rec.feedVersion)
	}
}

func TestParseZip_MissingRequiredFile(t *testing.T) {
	for _, name := range []string{"agency.txt", "stops.txt", "routes.txt", "trips.txt", "stop_times.txt"} {
		t.Run(name, func(t *testing.T) {
			files := baseFixture()
			delete(files, name)
			err := ParseZip(buildZip(t, files), &recorder{})
			if err == nil {
				t.Fatalf("no error with %s missing", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name %s", err, name)
			}
		})
	}
}

func TestParseZip_CRLFAndQuotedFields(t *testing.T) {
	files := baseFixture()
	files["stops.txt"] = "stop_id,stop_name,stop_lat,stop_lon\r\n" +
		"70061,\"Alewife, Busway\",42.3954,-71.1425\r\n"
	rec := &recorder{}
	if err := ParseZip(buildZip(t, files), rec); err != nil {
		t.Fatal(err)
	}
	if got := rec.stops[0].Name; got != "Alewife, Busway" {
		t.Errorf("quoted name = %q", got)
	}
	if got := rec.stops[0].Lon; got != -71.1425 {
		t.Errorf("lon under CRLF = %v", got)
	}
}

func TestParseZip_MetaFiresFirstDespiteArchiveOrder(t *testing.T) {
	files := baseFixture()
	// agency.txt and feed_info.txt placed last in the archive.
	entries := []zipEntry{
		{"routes.txt", files["routes.txt"]},
		{"stops.txt", files["stops.txt"]},
		{"trips.txt", files["trips.txt"]},
		{"stop_times.txt", files["stop_times.txt"]},
		{"calendar.txt", files["calendar.txt"]},
		{"calendar_dates.txt", files["calendar_dates.txt"]},
		{"agency.txt", files["agency.txt"]},
		{"feed_info.txt", files["feed_info.txt"]},
	}
	rec := &recorder{}
	if err := ParseZip(buildZipOrdered(t, entries), rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) == 0 || rec.calls[0] != "meta" {
		t.Fatalf("calls = %v, want meta first", rec.calls)
	}
	if rec.feedVersion != "2026-08-22" || rec.agencyTZ != "America/New_York" {
		t.Errorf("meta = (%q, %q)", rec.feedVersion, rec.agencyTZ)
	}
}

func TestParseZip_MissingAgencyTimezone(t *testing.T) {
	files := baseFixture()
	files["agency.txt"] = "agency_id,agency_name\n1,MBTA\n"
	err := ParseZip(buildZip(t, files), &recorder{})
	if err == nil || !strings.Contains(err.Error(), "agency_timezone") {
		t.Fatalf("err = %v, want agency_timezone error", err)
	}

	files["agency.txt"] = "agency_id,agency_name,agency_timezone\n1,MBTA,\n"
	err = ParseZip(buildZip(t, files), &recorder{})
	if err == nil || !strings.Contains(err.Error(), "agency_timezone") {
		t.Fatalf("err = %v, want empty agency_timezone error", err)
	}
}

func TestParseZip_CallbackErrorAborts(t *testing.T) {
	boom := errors.New("boom")
	rec := &recorder{failOn: "stoptime", failErr: boom}
	err := ParseZip(buildZip(t, baseFixture()), rec)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped boom", err)
	}
	if len(rec.stopTimes) != 1 {
		t.Errorf("stop_times callbacks after failure = %d, want 1", len(rec.stopTimes))
	}
}

func TestParseZip_NotAZip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "junk.zip")
	if err := os.WriteFile(p, []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ParseZip(p, &recorder{}); err == nil {
		t.Fatal("no error for corrupt zip")
	}
}

func TestParseZip_NestedDirectoryEntries(t *testing.T) {
	files := baseFixture()
	entries := make([]zipEntry, 0, len(files))
	for _, n := range []string{"agency.txt", "feed_info.txt", "routes.txt", "stops.txt", "trips.txt", "stop_times.txt", "calendar.txt", "calendar_dates.txt"} {
		entries = append(entries, zipEntry{"gtfs/" + n, files[n]})
	}
	rec := &recorder{}
	if err := ParseZip(buildZipOrdered(t, entries), rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.stopTimes) != 2 || rec.agencyTZ != "America/New_York" {
		t.Errorf("nested parse: stopTimes=%d tz=%q", len(rec.stopTimes), rec.agencyTZ)
	}
}
