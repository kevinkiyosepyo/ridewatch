package gtfsstatic

import (
	"archive/zip"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

var requiredFiles = []string{"agency.txt", "stops.txt", "routes.txt", "trips.txt", "stop_times.txt"}

// ParseZip streams a static GTFS zip into w. Meta fires as soon as agency.txt
// and feed_info.txt are read (regardless of where they sit in the archive),
// before any row callback. stop_times.txt is streamed row by row and never
// loaded into memory. Missing optional files (feed_info.txt, calendar.txt,
// calendar_dates.txt) are fine; missing required files are an error naming the
// file. Columns are located by header name, never by position; a callback
// error aborts parsing and propagates.
func ParseZip(zipPath string, w domain.ScheduleWriter) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("gtfsstatic: open %s: %w", zipPath, err)
	}
	defer zr.Close()

	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := path.Base(f.Name)
		// Prefer a root-level entry over one nested in a directory.
		if prev, ok := files[base]; !ok || (prev.Name != base && f.Name == base) {
			files[base] = f
		}
	}

	for _, name := range requiredFiles {
		if files[name] == nil {
			return fmt.Errorf("gtfsstatic: required file %s missing from %s", name, zipPath)
		}
	}

	agencyTZ, err := readAgencyTZ(files["agency.txt"])
	if err != nil {
		return err
	}
	feedVersion, err := readFeedVersion(files["feed_info.txt"])
	if err != nil {
		return err
	}
	if err := w.Meta(feedVersion, agencyTZ); err != nil {
		return fmt.Errorf("gtfsstatic: meta: %w", err)
	}

	if err := parseRoutes(files["routes.txt"], w); err != nil {
		return err
	}
	if err := parseStops(files["stops.txt"], w); err != nil {
		return err
	}
	if err := parseTrips(files["trips.txt"], w); err != nil {
		return err
	}
	if f := files["calendar.txt"]; f != nil {
		if err := parseCalendar(f, w); err != nil {
			return err
		}
	}
	if f := files["calendar_dates.txt"]; f != nil {
		if err := parseCalendarDates(f, w); err != nil {
			return err
		}
	}
	return parseStopTimes(files["stop_times.txt"], w)
}

// table streams one CSV member of the zip, addressing columns by header name.
// Records are reused between Next calls (encoding/csv ReuseRecord).
type table struct {
	name string
	rc   io.ReadCloser
	r    *csv.Reader
	cols map[string]int
	row  int // 1-based data row number, for error messages
}

func openTable(f *zip.File) (*table, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("gtfsstatic: open %s: %w", f.Name, err)
	}
	r := csv.NewReader(rc)
	r.ReuseRecord = true
	r.FieldsPerRecord = -1 // tolerate ragged rows; missing cells read as ""
	header, err := r.Read()
	if err != nil {
		rc.Close()
		return nil, fmt.Errorf("gtfsstatic: %s: read header: %w", path.Base(f.Name), err)
	}
	header[0] = strings.TrimPrefix(header[0], "\ufeff") // UTF-8 BOM (MBTA files have it)
	cols := make(map[string]int, len(header))
	for i, h := range header {
		cols[strings.TrimSpace(h)] = i
	}
	return &table{name: path.Base(f.Name), rc: rc, r: r, cols: cols}, nil
}

func (t *table) Close() error { return t.rc.Close() }

// Next returns the next data row, or io.EOF when the file is exhausted.
func (t *table) Next() ([]string, error) {
	rec, err := t.r.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("gtfsstatic: %s: %w", t.name, err)
	}
	t.row++
	return rec, nil
}

func (t *table) require(cols ...string) error {
	for _, c := range cols {
		if _, ok := t.cols[c]; !ok {
			return fmt.Errorf("gtfsstatic: %s: missing required column %s", t.name, c)
		}
	}
	return nil
}

// str returns the named column of rec, "" when the column or cell is absent.
func (t *table) str(rec []string, col string) string {
	i, ok := t.cols[col]
	if !ok || i >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[i])
}

func (t *table) intOr(rec []string, col string, def int) (int, error) {
	s := t.str(rec, col)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("gtfsstatic: %s row %d: bad %s %q: %w", t.name, t.row, col, s, err)
	}
	return n, nil
}

func (t *table) floatOr(rec []string, col string, def float64) (float64, error) {
	s := t.str(rec, col)
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("gtfsstatic: %s row %d: bad %s %q: %w", t.name, t.row, col, s, err)
	}
	return v, nil
}

// gtfsTime parses a GTFS time cell via domain.ParseGTFSTime (empty -> -1).
func (t *table) gtfsTime(rec []string, col string) (int, error) {
	secs, err := domain.ParseGTFSTime(t.str(rec, col))
	if err != nil {
		return -1, fmt.Errorf("gtfsstatic: %s row %d: %s: %w", t.name, t.row, col, err)
	}
	return secs, nil
}

func readAgencyTZ(f *zip.File) (string, error) {
	t, err := openTable(f)
	if err != nil {
		return "", err
	}
	defer t.Close()
	if err := t.require("agency_timezone"); err != nil {
		return "", err
	}
	rec, err := t.Next()
	if errors.Is(err, io.EOF) {
		return "", errors.New("gtfsstatic: agency.txt: no agency rows")
	}
	if err != nil {
		return "", err
	}
	tz := t.str(rec, "agency_timezone")
	if tz == "" {
		return "", errors.New("gtfsstatic: agency.txt: agency_timezone is empty")
	}
	return tz, nil
}

// readFeedVersion tolerates a nil file, a missing column, and an empty file,
// all of which yield "".
func readFeedVersion(f *zip.File) (string, error) {
	if f == nil {
		return "", nil
	}
	t, err := openTable(f)
	if err != nil {
		return "", err
	}
	defer t.Close()
	rec, err := t.Next()
	if errors.Is(err, io.EOF) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return t.str(rec, "feed_version"), nil
}

func parseRoutes(f *zip.File, w domain.ScheduleWriter) error {
	t, err := openTable(f)
	if err != nil {
		return err
	}
	defer t.Close()
	if err := t.require("route_id"); err != nil {
		return err
	}
	for {
		rec, err := t.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		routeType, err := t.intOr(rec, "route_type", 0)
		if err != nil {
			return err
		}
		sortOrder, err := t.intOr(rec, "route_sort_order", 0)
		if err != nil {
			return err
		}
		if err := w.Route(domain.Route{
			RouteID:   t.str(rec, "route_id"),
			ShortName: t.str(rec, "route_short_name"),
			LongName:  t.str(rec, "route_long_name"),
			RouteType: routeType,
			Color:     t.str(rec, "route_color"),
			TextColor: t.str(rec, "route_text_color"),
			SortOrder: sortOrder,
		}); err != nil {
			return fmt.Errorf("gtfsstatic: routes.txt row %d: %w", t.row, err)
		}
	}
}

func parseStops(f *zip.File, w domain.ScheduleWriter) error {
	t, err := openTable(f)
	if err != nil {
		return err
	}
	defer t.Close()
	if err := t.require("stop_id"); err != nil {
		return err
	}
	for {
		rec, err := t.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		lat, err := t.floatOr(rec, "stop_lat", 0)
		if err != nil {
			return err
		}
		lon, err := t.floatOr(rec, "stop_lon", 0)
		if err != nil {
			return err
		}
		locType, err := t.intOr(rec, "location_type", 0)
		if err != nil {
			return err
		}
		if err := w.Stop(domain.Stop{
			StopID:        t.str(rec, "stop_id"),
			Name:          t.str(rec, "stop_name"),
			Lat:           lat,
			Lon:           lon,
			ParentStation: t.str(rec, "parent_station"),
			LocationType:  locType,
			PlatformCode:  t.str(rec, "platform_code"),
		}); err != nil {
			return fmt.Errorf("gtfsstatic: stops.txt row %d: %w", t.row, err)
		}
	}
}

func parseTrips(f *zip.File, w domain.ScheduleWriter) error {
	t, err := openTable(f)
	if err != nil {
		return err
	}
	defer t.Close()
	if err := t.require("trip_id", "route_id", "service_id"); err != nil {
		return err
	}
	for {
		rec, err := t.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		dir, err := t.intOr(rec, "direction_id", -1)
		if err != nil {
			return err
		}
		if err := w.Trip(domain.Trip{
			TripID:      t.str(rec, "trip_id"),
			RouteID:     t.str(rec, "route_id"),
			ServiceID:   t.str(rec, "service_id"),
			Headsign:    t.str(rec, "trip_headsign"),
			DirectionID: int16(dir),
			ShapeID:     t.str(rec, "shape_id"),
		}); err != nil {
			return fmt.Errorf("gtfsstatic: trips.txt row %d: %w", t.row, err)
		}
	}
}

var weekdayCols = [7]string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}

func parseCalendar(f *zip.File, w domain.ScheduleWriter) error {
	t, err := openTable(f)
	if err != nil {
		return err
	}
	defer t.Close()
	if err := t.require("service_id"); err != nil {
		return err
	}
	for {
		rec, err := t.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var wd [7]bool
		for i, c := range weekdayCols {
			wd[i] = t.str(rec, c) == "1"
		}
		if err := w.Calendar(domain.CalendarEntry{
			ServiceID: t.str(rec, "service_id"),
			Weekdays:  wd,
			StartDate: t.str(rec, "start_date"),
			EndDate:   t.str(rec, "end_date"),
		}); err != nil {
			return fmt.Errorf("gtfsstatic: calendar.txt row %d: %w", t.row, err)
		}
	}
}

func parseCalendarDates(f *zip.File, w domain.ScheduleWriter) error {
	t, err := openTable(f)
	if err != nil {
		return err
	}
	defer t.Close()
	if err := t.require("service_id", "date"); err != nil {
		return err
	}
	for {
		rec, err := t.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		exc, err := t.intOr(rec, "exception_type", 0)
		if err != nil {
			return err
		}
		if err := w.CalendarDate(domain.CalendarDate{
			ServiceID:     t.str(rec, "service_id"),
			Date:          t.str(rec, "date"),
			ExceptionType: exc,
		}); err != nil {
			return fmt.Errorf("gtfsstatic: calendar_dates.txt row %d: %w", t.row, err)
		}
	}
}

// parseStopTimes streams row by row — the real file is ~2M rows and must never
// be slurped into memory.
func parseStopTimes(f *zip.File, w domain.ScheduleWriter) error {
	t, err := openTable(f)
	if err != nil {
		return err
	}
	defer t.Close()
	if err := t.require("trip_id", "stop_id", "stop_sequence"); err != nil {
		return err
	}
	for {
		rec, err := t.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		seqStr := t.str(rec, "stop_sequence")
		if seqStr == "" {
			return fmt.Errorf("gtfsstatic: stop_times.txt row %d: empty stop_sequence", t.row)
		}
		seq, err := strconv.Atoi(seqStr)
		if err != nil {
			return fmt.Errorf("gtfsstatic: stop_times.txt row %d: bad stop_sequence %q: %w", t.row, seqStr, err)
		}
		arr, err := t.gtfsTime(rec, "arrival_time")
		if err != nil {
			return err
		}
		dep, err := t.gtfsTime(rec, "departure_time")
		if err != nil {
			return err
		}
		if err := w.StopTime(domain.StopTime{
			TripID:        t.str(rec, "trip_id"),
			StopSequence:  seq,
			StopID:        t.str(rec, "stop_id"),
			ArrivalSecs:   arr,
			DepartureSecs: dep,
		}); err != nil {
			return fmt.Errorf("gtfsstatic: stop_times.txt row %d: %w", t.row, err)
		}
	}
}
