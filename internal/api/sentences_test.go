package api

import (
	"reflect"
	"testing"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

func dep(routeID string, secs int, dayClass string, n int, late5 float32) domain.DepartureStat {
	return domain.DepartureStat{
		RouteID:       routeID,
		StopID:        "S1",
		ScheduledSecs: secs,
		DayClass:      dayClass,
		N:             n,
		Late5Pct:      late5,
	}
}

func hourlyStat(n int, late5 float32) domain.HourlyStat {
	return domain.HourlyStat{RouteID: "1", StopID: "S1", N: n, Late5Pct: late5}
}

func TestReliabilitySentences(t *testing.T) {
	names := map[string]string{"1": "Route 1", "Red": "Red Line"}

	tests := []struct {
		name       string
		hourly     []domain.HourlyStat
		departures []domain.DepartureStat
		want       []string
	}{
		{
			name: "empty inputs",
			want: []string{},
		},
		{
			name:       "weekday morning departure plus overall",
			hourly:     []domain.HourlyStat{hourlyStat(100, 0.21)},
			departures: []domain.DepartureStat{dep("1", 8*3600+12*60, "weekday", 40, 0.38)},
			want: []string{
				"The 8:12 AM Route 1 departure is 5+ minutes late on 38% of weekday mornings.",
				"Overall, 21% of tracked arrivals here run 5+ minutes late.",
			},
		},
		{
			name:       "saturday afternoon",
			departures: []domain.DepartureStat{dep("Red", 14*3600+30*60, "saturday", 25, 0.5)},
			want: []string{
				"The 2:30 PM Red Line departure is 5+ minutes late on 50% of Saturday afternoons.",
			},
		},
		{
			name:       "sunday evening",
			departures: []domain.DepartureStat{dep("1", 20*3600+59*60, "sunday", 25, 0.1)},
			want: []string{
				"The 8:59 PM Route 1 departure is 5+ minutes late on 10% of Sunday evenings.",
			},
		},
		{
			name:       "night bucket at 21h",
			departures: []domain.DepartureStat{dep("1", 21*3600, "weekday", 25, 0.1)},
			want: []string{
				"The 9:00 PM Route 1 departure is 5+ minutes late on 10% of weekday nights.",
			},
		},
		{
			name:       "after-midnight GTFS time wraps clock and is night",
			departures: []domain.DepartureStat{dep("1", 25*3600+30*60, "weekday", 25, 0.2)},
			want: []string{
				"The 1:30 AM Route 1 departure is 5+ minutes late on 20% of weekday nights.",
			},
		},
		{
			name: "noon is PM afternoon and midnight is 12 AM morning",
			departures: []domain.DepartureStat{
				dep("1", 12*3600, "weekday", 25, 0.4),
				dep("1", 0, "weekday", 25, 0.3),
			},
			want: []string{
				"The 12:00 PM Route 1 departure is 5+ minutes late on 40% of weekday afternoons.",
				"The 12:00 AM Route 1 departure is 5+ minutes late on 30% of weekday mornings.",
			},
		},
		{
			name: "small-n departures suppressed",
			departures: []domain.DepartureStat{
				dep("1", 9*3600, "weekday", domain.MinObservations-1, 0.9),
				dep("1", 10*3600, "weekday", domain.MinObservations, 0.2),
			},
			want: []string{
				"The 10:00 AM Route 1 departure is 5+ minutes late on 20% of weekday mornings.",
			},
		},
		{
			name: "worst first, capped at five",
			departures: []domain.DepartureStat{
				dep("1", 6*3600, "weekday", 20, 0.10),
				dep("1", 7*3600, "weekday", 20, 0.60),
				dep("1", 8*3600, "weekday", 20, 0.30),
				dep("1", 9*3600, "weekday", 20, 0.50),
				dep("1", 10*3600, "weekday", 20, 0.40),
				dep("1", 11*3600, "weekday", 20, 0.20),
			},
			want: []string{
				"The 7:00 AM Route 1 departure is 5+ minutes late on 60% of weekday mornings.",
				"The 9:00 AM Route 1 departure is 5+ minutes late on 50% of weekday mornings.",
				"The 10:00 AM Route 1 departure is 5+ minutes late on 40% of weekday mornings.",
				"The 8:00 AM Route 1 departure is 5+ minutes late on 30% of weekday mornings.",
				"The 11:00 AM Route 1 departure is 5+ minutes late on 20% of weekday mornings.",
			},
		},
		{
			name: "overall sentence is observation-weighted",
			hourly: []domain.HourlyStat{
				hourlyStat(90, 0.10),
				hourlyStat(10, 1.00),
				hourlyStat(domain.MinObservations-1, 0.0), // suppressed
			},
			want: []string{
				"Overall, 19% of tracked arrivals here run 5+ minutes late.",
			},
		},
		{
			name:       "unknown route falls back to route id",
			departures: []domain.DepartureStat{dep("77X", 9*3600, "weekday", 25, 0.25)},
			want: []string{
				"The 9:00 AM Route 77X departure is 5+ minutes late on 25% of weekday mornings.",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReliabilitySentences(tt.hourly, tt.departures, names)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ReliabilitySentences() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}
