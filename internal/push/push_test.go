package push

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// fakeSubs implements domain.SubscriptionStore. Only the methods the evaluator
// touches have behavior; the rest satisfy the interface.
type fakeSubs struct {
	subs    []domain.Subscription
	listErr error
	sent    map[int64][]domain.EventKey // MarkPushSent calls
	stale   map[domain.EventKey]bool    // keys reported as already-sent
	markErr error
	results []pushResult
}

type pushResult struct {
	subID int64
	ok    bool
	gone  bool
}

func (f *fakeSubs) SaveSubscription(context.Context, domain.Subscription) (int64, error) {
	return 0, nil
}
func (f *fakeSubs) DeleteSubscription(context.Context, string) error { return nil }
func (f *fakeSubs) AllSubscriptions(context.Context) ([]domain.Subscription, error) {
	return f.subs, f.listErr
}
func (f *fakeSubs) MarkPushSent(_ context.Context, id int64, key domain.EventKey) (bool, error) {
	if f.markErr != nil {
		return false, f.markErr
	}
	if f.sent == nil {
		f.sent = map[int64][]domain.EventKey{}
	}
	f.sent[id] = append(f.sent[id], key)
	return !f.stale[key], nil
}
func (f *fakeSubs) RecordPushResult(_ context.Context, id int64, ok, gone bool) error {
	f.results = append(f.results, pushResult{id, ok, gone})
	return nil
}

// fakeLive implements domain.LiveSource with a fixed per-stop event list.
type fakeLive struct {
	upcoming map[string][]domain.StopEvent
}

func (f *fakeLive) LiveVehicles() []domain.LiveVehicle { return nil }
func (f *fakeLive) UpcomingAtStop(stopID string, _ time.Duration) []domain.StopEvent {
	return f.upcoming[stopID]
}
func (f *fakeLive) FeedAge(domain.Feed) (time.Duration, bool) { return 0, false }

// fakeQueries implements domain.StopQueries; only Stop and Route matter here.
type fakeQueries struct {
	stops  map[string]string // id -> name
	routes map[string]string // id -> short name
}

func (f *fakeQueries) SearchStops(context.Context, string, int) ([]domain.Stop, error) {
	return nil, nil
}
func (f *fakeQueries) StopsInBBox(context.Context, float64, float64, float64, float64, int) ([]domain.Stop, error) {
	return nil, nil
}
func (f *fakeQueries) Stop(_ context.Context, id string) (*domain.Stop, error) {
	if name, ok := f.stops[id]; ok {
		return &domain.Stop{StopID: id, Name: name}, nil
	}
	return nil, domain.ErrNotFound
}
func (f *fakeQueries) Routes(context.Context) ([]domain.Route, error) { return nil, nil }
func (f *fakeQueries) Route(_ context.Context, id string) (*domain.Route, error) {
	if name, ok := f.routes[id]; ok {
		return &domain.Route{RouteID: id, ShortName: name}, nil
	}
	return nil, domain.ErrNotFound
}
func (f *fakeQueries) RoutesServingStop(context.Context, string) ([]domain.Route, error) {
	return nil, nil
}
func (f *fakeQueries) StopHourly(context.Context, string) ([]domain.HourlyStat, error) {
	return nil, nil
}
func (f *fakeQueries) StopDepartures(context.Context, string) ([]domain.DepartureStat, error) {
	return nil, nil
}
func (f *fakeQueries) RouteHourly(context.Context, string) ([]domain.HourlyStat, error) {
	return nil, nil
}
func (f *fakeQueries) RecentStopEvents(context.Context, string, int) ([]domain.StopEvent, error) {
	return nil, nil
}

type sentPush struct {
	sub     domain.Subscription
	payload payload
}

// newTestEvaluator wires an evaluator whose sends are captured, returning each
// given status in order (the last repeats).
func newTestEvaluator(subs *fakeSubs, live *fakeLive, q *fakeQueries, statuses ...int) (*Evaluator, *[]sentPush, *[]error) {
	e := NewEvaluator(Config{Subject: "mailto:t@example.com"}, subs, live, q)
	var sent []sentPush
	var sendErrs []error
	i := 0
	e.send = func(_ context.Context, sub domain.Subscription, body []byte) (int, error) {
		var p payload
		if err := json.Unmarshal(body, &p); err != nil {
			return 0, err
		}
		sent = append(sent, sentPush{sub, p})
		status := 201
		if len(statuses) > 0 {
			if i < len(statuses) {
				status = statuses[i]
			} else {
				status = statuses[len(statuses)-1]
			}
		}
		i++
		if status == 0 {
			return 0, errors.New("push service unreachable")
		}
		return status, nil
	}
	return e, &sent, &sendErrs
}

func delayed(secs int) *int { return &secs }

func event(stopID, routeID string, direction int16, delay *int) domain.StopEvent {
	return domain.StopEvent{
		ServiceDate:      "20260826",
		TripID:           "trip-" + routeID,
		StopSequence:     5,
		StopID:           stopID,
		RouteID:          routeID,
		DirectionID:      direction,
		ScheduledArrival: time.Date(2026, 8, 26, 8, 12, 0, 0, time.UTC),
		PredictedArrival: time.Date(2026, 8, 26, 8, 18, 0, 0, time.UTC),
		DelaySecs:        delay,
	}
}

func baseSub() domain.Subscription {
	return domain.Subscription{
		ID:            1,
		Endpoint:      "https://push.example/ep1",
		StopID:        "stop-1",
		DirectionID:   -1,
		ThresholdSecs: 300,
	}
}

func TestEvaluateSendsWhenThresholdCrossed(t *testing.T) {
	subs := &fakeSubs{subs: []domain.Subscription{baseSub()}}
	live := &fakeLive{upcoming: map[string][]domain.StopEvent{
		"stop-1": {event("stop-1", "route-1", 0, delayed(360))},
	}}
	q := &fakeQueries{stops: map[string]string{"stop-1": "Harvard Sq"}, routes: map[string]string{"route-1": "1"}}
	e, sent, _ := newTestEvaluator(subs, live, q, 201)

	if err := e.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(*sent) != 1 {
		t.Fatalf("sent = %d pushes, want 1", len(*sent))
	}
	p := (*sent)[0].payload
	if p.Title != "Route 1 delayed" {
		t.Errorf("title = %q", p.Title)
	}
	if !strings.Contains(p.Body, "Harvard Sq") || !strings.Contains(p.Body, "8:12 AM") || !strings.Contains(p.Body, "6 min late") {
		t.Errorf("body = %q, want stop name, clock time, and rounded minutes", p.Body)
	}
	if p.URL != "/stop/stop-1" {
		t.Errorf("url = %q", p.URL)
	}
	if len(subs.results) != 1 || !subs.results[0].ok || subs.results[0].gone {
		t.Errorf("results = %+v, want one ok result", subs.results)
	}
}

func TestEvaluateFiltersBelowThresholdSkippedAndUnknown(t *testing.T) {
	subs := &fakeSubs{subs: []domain.Subscription{baseSub()}}
	skipped := event("stop-1", "route-1", 0, delayed(600))
	skipped.Skipped = true
	live := &fakeLive{upcoming: map[string][]domain.StopEvent{
		"stop-1": {
			event("stop-1", "route-1", 0, delayed(299)), // below threshold
			event("stop-1", "route-1", 0, nil),          // unknown delay
			skipped,                                     // skipped stop
		},
	}}
	e, sent, _ := newTestEvaluator(subs, live, &fakeQueries{})

	if err := e.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(*sent) != 0 {
		t.Fatalf("sent = %d pushes, want 0", len(*sent))
	}
	if len(subs.sent) != 0 {
		t.Errorf("MarkPushSent called %d times for filtered events, want 0", len(subs.sent))
	}
}

func TestEvaluateRouteAndDirectionFilters(t *testing.T) {
	sub := baseSub()
	sub.RouteID = "route-1"
	sub.DirectionID = 1
	subs := &fakeSubs{subs: []domain.Subscription{sub}}
	live := &fakeLive{upcoming: map[string][]domain.StopEvent{
		"stop-1": {
			event("stop-1", "route-2", 1, delayed(600)), // wrong route
			event("stop-1", "route-1", 0, delayed(600)), // wrong direction
			event("stop-1", "route-1", 1, delayed(600)), // match
		},
	}}
	e, sent, _ := newTestEvaluator(subs, live, &fakeQueries{})

	if err := e.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(*sent) != 1 {
		t.Fatalf("sent = %d pushes, want exactly the route+direction match", len(*sent))
	}
	if got := (*sent)[0].payload.Title; !strings.Contains(got, "route-1") {
		t.Errorf("title = %q, want fallback to route id when Route lookup fails", got)
	}
}

func TestEvaluateDedupesViaMarkPushSent(t *testing.T) {
	ev := event("stop-1", "route-1", 0, delayed(600))
	subs := &fakeSubs{
		subs:  []domain.Subscription{baseSub()},
		stale: map[domain.EventKey]bool{ev.Key(): true},
	}
	live := &fakeLive{upcoming: map[string][]domain.StopEvent{"stop-1": {ev}}}
	e, sent, _ := newTestEvaluator(subs, live, &fakeQueries{})

	if err := e.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(*sent) != 0 {
		t.Fatalf("sent = %d pushes for an already-alerted event, want 0", len(*sent))
	}
	if len(subs.results) != 0 {
		t.Errorf("RecordPushResult called for a deduped event")
	}
}

func TestEvaluateMarkErrorSkipsSend(t *testing.T) {
	subs := &fakeSubs{
		subs:    []domain.Subscription{baseSub()},
		markErr: errors.New("db down"),
	}
	live := &fakeLive{upcoming: map[string][]domain.StopEvent{
		"stop-1": {event("stop-1", "route-1", 0, delayed(600))},
	}}
	e, sent, _ := newTestEvaluator(subs, live, &fakeQueries{})

	if err := e.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate should not fail on a per-event mark error: %v", err)
	}
	if len(*sent) != 0 {
		t.Fatalf("sent despite MarkPushSent error")
	}
}

func TestEvaluateGoneEndpoint(t *testing.T) {
	subs := &fakeSubs{subs: []domain.Subscription{baseSub()}}
	live := &fakeLive{upcoming: map[string][]domain.StopEvent{
		"stop-1": {event("stop-1", "route-1", 0, delayed(600))},
	}}
	e, _, _ := newTestEvaluator(subs, live, &fakeQueries{}, 410)

	if err := e.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(subs.results) != 1 || subs.results[0].ok || !subs.results[0].gone {
		t.Fatalf("results = %+v, want gone=true ok=false", subs.results)
	}
}

func TestEvaluateOneFailureDoesNotAbortPass(t *testing.T) {
	sub2 := baseSub()
	sub2.ID = 2
	sub2.Endpoint = "https://push.example/ep2"
	sub2.StopID = "stop-2"
	subs := &fakeSubs{subs: []domain.Subscription{baseSub(), sub2}}
	live := &fakeLive{upcoming: map[string][]domain.StopEvent{
		"stop-1": {event("stop-1", "route-1", 0, delayed(600))},
		"stop-2": {event("stop-2", "route-9", 0, delayed(600))},
	}}
	// First send errors (status 0 sentinel), second succeeds.
	e, sent, _ := newTestEvaluator(subs, live, &fakeQueries{}, 0, 201)

	if err := e.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(*sent) != 2 {
		t.Fatalf("sent attempts = %d, want 2 (second sub still evaluated)", len(*sent))
	}
	if len(subs.results) != 2 {
		t.Fatalf("results = %+v, want 2", subs.results)
	}
	if subs.results[0].ok || subs.results[0].gone {
		t.Errorf("first result = %+v, want plain error (ok=false gone=false)", subs.results[0])
	}
	if !subs.results[1].ok {
		t.Errorf("second result = %+v, want ok", subs.results[1])
	}
}

func TestEvaluateListError(t *testing.T) {
	subs := &fakeSubs{listErr: errors.New("db gone")}
	e, _, _ := newTestEvaluator(subs, &fakeLive{}, &fakeQueries{})
	if err := e.Evaluate(context.Background()); err == nil {
		t.Fatal("Evaluate should surface AllSubscriptions errors")
	}
}

func TestArrivalClock(t *testing.T) {
	cases := []struct {
		name string
		ev   domain.StopEvent
		want string
	}{
		{"morning", domain.StopEvent{ScheduledArrival: time.Date(2026, 8, 26, 8, 5, 0, 0, time.UTC)}, "8:05 AM"},
		{"noon", domain.StopEvent{ScheduledArrival: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)}, "12:00 PM"},
		{"midnight", domain.StopEvent{ScheduledArrival: time.Date(2026, 8, 26, 0, 30, 0, 0, time.UTC)}, "12:30 AM"},
		{"evening", domain.StopEvent{ScheduledArrival: time.Date(2026, 8, 26, 17, 45, 0, 0, time.UTC)}, "5:45 PM"},
		{"falls back to predicted", domain.StopEvent{PredictedArrival: time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)}, "9:00 AM"},
		{"no times", domain.StopEvent{}, "upcoming"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := arrivalClock(tc.ev); got != tc.want {
				t.Errorf("arrivalClock = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGenerateVAPIDKeys(t *testing.T) {
	pub, priv, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}
	if pub == "" || priv == "" || pub == priv {
		t.Fatalf("want distinct nonempty keys, got pub=%q priv=%q", pub, priv)
	}
}
