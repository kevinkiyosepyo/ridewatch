package api

import (
	"fmt"
	"math"
	"sort"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// ReliabilitySentences renders plain-language reliability sentences for a stop
// from its rollup rows. It is a pure function: routeNames maps route_id to a
// display noun ("Route 1", "Red Line"); missing entries fall back to
// "Route {route_id}".
//
// Up to five departure sentences are produced from rows with
// n >= domain.MinObservations, worst late5_pct first, followed by one overall
// sentence derived from the hourly rows (observation-weighted). late5_pct and
// early_pct are fractions in [0,1] per the schema ("share of observations").
func ReliabilitySentences(hourly []domain.HourlyStat, departures []domain.DepartureStat, routeNames map[string]string) []string {
	sentences := make([]string, 0, 6)

	eligible := make([]domain.DepartureStat, 0, len(departures))
	for _, d := range departures {
		if d.N >= domain.MinObservations {
			eligible = append(eligible, d)
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].Late5Pct != eligible[j].Late5Pct {
			return eligible[i].Late5Pct > eligible[j].Late5Pct
		}
		if eligible[i].N != eligible[j].N {
			return eligible[i].N > eligible[j].N
		}
		return eligible[i].ScheduledSecs < eligible[j].ScheduledSecs
	})
	if len(eligible) > 5 {
		eligible = eligible[:5]
	}
	for _, d := range eligible {
		sentences = append(sentences, fmt.Sprintf(
			"The %s %s departure is 5+ minutes late on %d%% of %s %ss.",
			formatClock12(d.ScheduledSecs),
			routeDisplayName(d.RouteID, routeNames),
			pct(d.Late5Pct),
			dayPhrase(d.DayClass),
			timeOfDayBucket(d.ScheduledSecs),
		))
	}

	var n int
	var weighted float64
	for _, h := range hourly {
		if h.N < domain.MinObservations {
			continue
		}
		n += h.N
		weighted += float64(h.N) * float64(h.Late5Pct)
	}
	if n > 0 {
		sentences = append(sentences, fmt.Sprintf(
			"Overall, %d%% of tracked arrivals here run 5+ minutes late.",
			pct(float32(weighted/float64(n))),
		))
	}
	return sentences
}

// pct converts a [0,1] share to a whole percentage.
func pct(share float32) int {
	return int(math.Round(float64(share) * 100))
}

// formatClock12 renders GTFS seconds-of-service-day as "h:MM AM/PM".
// Times past 24:00 wrap to the clock time riders actually see.
func formatClock12(secs int) string {
	h := (secs / 3600) % 24
	m := (secs % 3600) / 60
	ampm := "AM"
	if h >= 12 {
		ampm = "PM"
	}
	h12 := h % 12
	if h12 == 0 {
		h12 = 12
	}
	return fmt.Sprintf("%d:%02d %s", h12, m, ampm)
}

// timeOfDayBucket classifies a scheduled time: before noon is morning, before
// 5 PM afternoon, before 9 PM evening, everything else (including after-midnight
// GTFS times >= 24:00) night.
func timeOfDayBucket(secs int) string {
	switch {
	case secs < 12*3600:
		return "morning"
	case secs < 17*3600:
		return "afternoon"
	case secs < 21*3600:
		return "evening"
	default:
		return "night"
	}
}

func dayPhrase(dayClass string) string {
	switch dayClass {
	case "weekday":
		return "weekday"
	case "saturday":
		return "Saturday"
	case "sunday":
		return "Sunday"
	default:
		return dayClass
	}
}

func routeDisplayName(routeID string, routeNames map[string]string) string {
	if name := routeNames[routeID]; name != "" {
		return name
	}
	return "Route " + routeID
}
