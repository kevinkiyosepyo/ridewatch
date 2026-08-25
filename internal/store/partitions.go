package store

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// partitionedTables are the parents EnsurePartitions manages.
var partitionedTables = []string{"stop_events", "vehicle_positions"}

// weekStart returns 00:00 UTC on the ISO-week Monday containing t.
func weekStart(t time.Time) time.Time {
	t = t.UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

// isoWeekStart returns the Monday starting the given ISO year+week.
func isoWeekStart(isoYear, week int) time.Time {
	// January 4 is always inside ISO week 1 of its year.
	return weekStart(time.Date(isoYear, time.January, 4, 0, 0, 0, 0, time.UTC)).
		AddDate(0, 0, (week-1)*7)
}

// partitionName names the weekly partition of parent holding wkStart's ISO week,
// e.g. stop_events_y2026w35.
func partitionName(parent string, wkStart time.Time) string {
	y, w := wkStart.ISOWeek()
	return fmt.Sprintf("%s_y%dw%02d", parent, y, w)
}

// parsePartitionName inverts partitionName, returning the week's Monday.
// ok is false for names that are not weekly partitions of parent (in
// particular the DEFAULT partition).
func parsePartitionName(parent, name string) (wkStart time.Time, ok bool) {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(parent) + `_y(\d{4})w(\d{2})$`)
	m := re.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	year, _ := strconv.Atoi(m[1])
	week, _ := strconv.Atoi(m[2])
	ws := isoWeekStart(year, week)
	// Reject nonsense weeks (w00, w54, ...) that don't round-trip.
	if partitionName(parent, ws) != name {
		return time.Time{}, false
	}
	return ws, true
}

// EnsurePartitions creates the weekly partitions (ISO-Monday boundaries) of
// stop_events and vehicle_positions covering [from, to]. Idempotent. If a
// week cannot be attached because matching rows already sit in the DEFAULT
// partition, that week is logged and skipped — the rows stay in the default
// partition and correctness is unaffected.
func (s *Store) EnsurePartitions(ctx context.Context, from, to time.Time) error {
	for ws, end := weekStart(from), weekStart(to); !ws.After(end); ws = ws.AddDate(0, 0, 7) {
		lo := ws.Format("2006-01-02")
		hi := ws.AddDate(0, 0, 7).Format("2006-01-02")
		for _, parent := range partitionedTables {
			name := partitionName(parent, ws)
			sql := fmt.Sprintf(
				`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
				pgx.Identifier{name}.Sanitize(), pgx.Identifier{parent}.Sanitize(), lo, hi,
			)
			if _, err := s.pool.Exec(ctx, sql); err != nil {
				// SQLSTATE 23514: the DEFAULT partition already holds rows in
				// this range, so the new partition cannot be attached.
				if strings.Contains(err.Error(), "SQLSTATE 23514") {
					slog.Warn("partition not created: matching rows already in default partition",
						"partition", name, "err", err)
					continue
				}
				return fmt.Errorf("store: create partition %s: %w", name, err)
			}
		}
	}
	return nil
}

// DropOldPartitions detaches and drops weekly partitions whose whole week is
// older than now minus keepWeeks[parent] weeks. Parents with keepWeeks 0 (or
// absent) are skipped; the DEFAULT partition is never touched.
func (s *Store) DropOldPartitions(ctx context.Context, keepWeeks map[string]int) error {
	return s.dropOldPartitions(ctx, keepWeeks, time.Now())
}

func (s *Store) dropOldPartitions(ctx context.Context, keepWeeks map[string]int, now time.Time) error {
	for parent, keep := range keepWeeks {
		if keep <= 0 {
			continue
		}
		cutoff := now.UTC().AddDate(0, 0, -7*keep)
		children, err := s.childPartitions(ctx, parent)
		if err != nil {
			return err
		}
		for _, child := range children {
			ws, ok := parsePartitionName(parent, child)
			if !ok {
				continue // default partition or foreign naming
			}
			if ws.AddDate(0, 0, 7).After(cutoff) {
				continue // week not entirely older than the cutoff
			}
			detach := fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s`,
				pgx.Identifier{parent}.Sanitize(), pgx.Identifier{child}.Sanitize())
			if _, err := s.pool.Exec(ctx, detach); err != nil {
				return fmt.Errorf("store: detach partition %s: %w", child, err)
			}
			drop := fmt.Sprintf(`DROP TABLE %s`, pgx.Identifier{child}.Sanitize())
			if _, err := s.pool.Exec(ctx, drop); err != nil {
				return fmt.Errorf("store: drop partition %s: %w", child, err)
			}
		}
	}
	return nil
}

// childPartitions lists the relation names of parent's partitions.
func (s *Store) childPartitions(ctx context.Context, parent string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = $1
		ORDER BY c.relname`, parent)
	if err != nil {
		return nil, fmt.Errorf("store: list partitions of %s: %w", parent, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("store: list partitions of %s: %w", parent, err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list partitions of %s: %w", parent, err)
	}
	return names, nil
}
