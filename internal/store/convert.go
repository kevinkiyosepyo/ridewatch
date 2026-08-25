package store

import (
	"fmt"
	"time"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// dateFromServiceDate converts a domain YYYYMMDD string to a midnight-UTC
// time.Time suitable for a Postgres DATE parameter.
func dateFromServiceDate(d string) (time.Time, error) {
	y, m, dd, err := domain.ParseServiceDate(d)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: %w", err)
	}
	return time.Date(y, m, dd, 0, 0, 0, 0, time.UTC), nil
}

// serviceDateFromTime renders a DATE value scanned as time.Time back to YYYYMMDD.
func serviceDateFromTime(t time.Time) string {
	return t.Format("20060102")
}

// nullTime maps the zero time.Time to SQL NULL, everything else to UTC.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// timeOrZero maps a scanned nullable timestamp back to the zero-time sentinel.
func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}
