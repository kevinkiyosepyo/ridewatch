package domain

import (
	"fmt"
	"time"
)

// GTFS time semantics: a stop_time like "25:30:00" means 1:30 AM on the civil day
// after the service date. The spec anchors times to "noon minus 12 hours" on the
// service date — on DST transition days that origin is NOT local midnight (the
// day is 23 or 25 real hours long), and anchoring there is what keeps "08:00:00"
// meaning 8 AM wall clock on those days. Always resolve times through
// NoonMinus12; never add seconds to local midnight.

// ParseServiceDate parses a YYYYMMDD service date.
func ParseServiceDate(d string) (year int, month time.Month, day int, err error) {
	if len(d) != 8 {
		return 0, 0, 0, fmt.Errorf("bad service date %q", d)
	}
	var y, m, dd int
	if _, err := fmt.Sscanf(d, "%4d%2d%2d", &y, &m, &dd); err != nil {
		return 0, 0, 0, fmt.Errorf("bad service date %q: %w", d, err)
	}
	if m < 1 || m > 12 || dd < 1 || dd > 31 {
		return 0, 0, 0, fmt.Errorf("bad service date %q", d)
	}
	return y, time.Month(m), dd, nil
}

// NoonMinus12 returns the GTFS time origin for a service date in loc:
// 12:00 wall clock minus 12 hours. On normal days this is midnight; on DST
// transition days it differs by an hour, which is exactly what keeps schedule
// comparisons correct on those days.
func NoonMinus12(serviceDate string, loc *time.Location) (time.Time, error) {
	y, m, d, err := ParseServiceDate(serviceDate)
	if err != nil {
		return time.Time{}, err
	}
	noon := time.Date(y, m, d, 12, 0, 0, 0, loc)
	return noon.Add(-12 * time.Hour), nil
}

// ScheduledTime resolves a GTFS seconds-of-service-day value (may exceed 86400)
// to an absolute instant.
func ScheduledTime(serviceDate string, secs int, loc *time.Location) (time.Time, error) {
	base, err := NoonMinus12(serviceDate, loc)
	if err != nil {
		return time.Time{}, err
	}
	return base.Add(time.Duration(secs) * time.Second), nil
}

// ParseGTFSTime parses "H:MM:SS" or "HH:MM:SS" (hours may exceed 24) into
// seconds of the service day. Returns -1 for an empty string.
func ParseGTFSTime(s string) (int, error) {
	if s == "" {
		return -1, nil
	}
	var h, m, sec int
	if _, err := fmt.Sscanf(s, "%d:%2d:%2d", &h, &m, &sec); err != nil {
		return -1, fmt.Errorf("bad GTFS time %q: %w", s, err)
	}
	if h < 0 || m < 0 || m > 59 || sec < 0 || sec > 59 {
		return -1, fmt.Errorf("bad GTFS time %q", s)
	}
	return h*3600 + m*60 + sec, nil
}

// FormatGTFSTime renders seconds-of-service-day as HH:MM:SS (hours may exceed 24).
func FormatGTFSTime(secs int) string {
	return fmt.Sprintf("%02d:%02d:%02d", secs/3600, secs%3600/60, secs%60)
}

// CivilDate renders t's calendar date in loc as YYYYMMDD.
func CivilDate(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("20060102")
}

// AddDays shifts a YYYYMMDD date by n calendar days.
func AddDays(serviceDate string, n int) (string, error) {
	y, m, d, err := ParseServiceDate(serviceDate)
	if err != nil {
		return "", err
	}
	return time.Date(y, m, d+n, 12, 0, 0, 0, time.UTC).Format("20060102"), nil
}

// HourOfWeek maps an instant to 0..167 (0 = Monday 00:00) in loc.
func HourOfWeek(t time.Time, loc *time.Location) int {
	lt := t.In(loc)
	wd := (int(lt.Weekday()) + 6) % 7 // Monday = 0
	return wd*24 + lt.Hour()
}

// DayClass classifies a service date for departure rollups.
func DayClass(serviceDate string) (string, error) {
	y, m, d, err := ParseServiceDate(serviceDate)
	if err != nil {
		return "", err
	}
	switch time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Weekday() {
	case time.Saturday:
		return "saturday", nil
	case time.Sunday:
		return "sunday", nil
	default:
		return "weekday", nil
	}
}
