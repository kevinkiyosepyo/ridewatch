// Package push evaluates Web Push subscriptions against the live feed state
// and sends VAPID-signed delay notifications.
package push

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
	"github.com/kevinkiyosepyo/ridewatch/internal/metrics"
)

// Config holds the VAPID credentials and how far ahead one evaluation looks.
type Config struct {
	VAPIDPublic  string
	VAPIDPrivate string
	Subject      string // mailto: or https: contact for the push service
	Horizon      time.Duration
}

const (
	defaultHorizon  = 45 * time.Minute
	notificationTTL = 900 // seconds; a delay alert is stale after ~15 min
	sendTimeout     = 20 * time.Second
)

// sendFunc performs one Web Push delivery and returns the push service's HTTP
// status code. Injectable so tests never touch the network.
type sendFunc func(ctx context.Context, sub domain.Subscription, payload []byte) (status int, err error)

// Evaluator runs periodic passes over all subscriptions.
type Evaluator struct {
	cfg  Config
	subs domain.SubscriptionStore
	live domain.LiveSource
	q    domain.StopQueries
	send sendFunc
	log  *slog.Logger
}

// NewEvaluator wires an evaluator with the real webpush sender.
func NewEvaluator(cfg Config, subs domain.SubscriptionStore, live domain.LiveSource, q domain.StopQueries) *Evaluator {
	if cfg.Horizon <= 0 {
		cfg.Horizon = defaultHorizon
	}
	e := &Evaluator{cfg: cfg, subs: subs, live: live, q: q, log: slog.Default()}
	e.send = e.webpushSend
	return e
}

func (e *Evaluator) webpushSend(ctx context.Context, sub domain.Subscription, payload []byte) (int, error) {
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{Auth: sub.Auth, P256dh: sub.P256dh},
	}, &webpush.Options{
		Subscriber:      e.cfg.Subject,
		TTL:             notificationTTL,
		Urgency:         webpush.UrgencyHigh,
		VAPIDPublicKey:  e.cfg.VAPIDPublic,
		VAPIDPrivateKey: e.cfg.VAPIDPrivate,
	})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

type payload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
}

// Evaluate runs one pass: for every subscription, find upcoming events at its
// stop whose delay crosses the threshold, dedupe via MarkPushSent, and deliver.
// One subscription's failure never aborts the pass; only listing subscriptions
// (or a canceled context) returns an error.
func (e *Evaluator) Evaluate(ctx context.Context) error {
	subs, err := e.subs.AllSubscriptions(ctx)
	if err != nil {
		return fmt.Errorf("list subscriptions: %w", err)
	}
	stopNames := map[string]string{}
	routeNames := map[string]string{}
	for _, sub := range subs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.evaluateSubscription(ctx, sub, stopNames, routeNames)
	}
	return nil
}

func (e *Evaluator) evaluateSubscription(ctx context.Context, sub domain.Subscription, stopNames, routeNames map[string]string) {
	for _, ev := range e.live.UpcomingAtStop(sub.StopID, e.cfg.Horizon) {
		if sub.RouteID != "" && ev.RouteID != sub.RouteID {
			continue
		}
		if sub.DirectionID >= 0 && ev.DirectionID != sub.DirectionID {
			continue
		}
		if ev.Skipped || ev.DelaySecs == nil || *ev.DelaySecs < sub.ThresholdSecs {
			continue
		}
		fresh, err := e.subs.MarkPushSent(ctx, sub.ID, ev.Key())
		if err != nil {
			e.log.Error("mark push sent", "subscription", sub.ID, "err", err)
			continue
		}
		if !fresh {
			continue
		}
		e.deliver(ctx, sub, ev, stopNames, routeNames)
	}
}

func (e *Evaluator) deliver(ctx context.Context, sub domain.Subscription, ev domain.StopEvent, stopNames, routeNames map[string]string) {
	body, err := json.Marshal(payload{
		Title: fmt.Sprintf("Route %s delayed", e.routeName(ctx, ev.RouteID, routeNames)),
		Body: fmt.Sprintf("%s: %s arrival running %d min late",
			e.stopName(ctx, sub.StopID, stopNames),
			arrivalClock(ev),
			int(math.Round(float64(*ev.DelaySecs)/60))),
		URL: "/stop/" + sub.StopID,
	})
	if err != nil {
		e.log.Error("marshal push payload", "subscription", sub.ID, "err", err)
		return
	}

	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	status, err := e.send(sendCtx, sub, body)
	cancel()

	var outcome string
	var ok, gone bool
	switch {
	case err != nil:
		outcome = "error"
		e.log.Error("push send", "subscription", sub.ID, "err", err)
	case status == 404 || status == 410:
		outcome, gone = "gone", true
	case status >= 200 && status < 300:
		outcome, ok = "ok", true
	default:
		outcome = "error"
		e.log.Warn("push send rejected", "subscription", sub.ID, "status", status)
	}
	metrics.PushSent.WithLabelValues(outcome).Inc()
	if err := e.subs.RecordPushResult(ctx, sub.ID, ok, gone); err != nil {
		e.log.Error("record push result", "subscription", sub.ID, "err", err)
	}
}

// arrivalClock renders the event's scheduled (or, failing that, predicted)
// arrival as "h:MM AM/PM" in the time's own location — the reconcile engine
// resolves scheduled times in the agency timezone.
func arrivalClock(ev domain.StopEvent) string {
	t := ev.ScheduledArrival
	if t.IsZero() {
		t = ev.PredictedArrival
	}
	if t.IsZero() {
		return "upcoming"
	}
	h, m := t.Hour(), t.Minute()
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

func (e *Evaluator) stopName(ctx context.Context, stopID string, cache map[string]string) string {
	if name, ok := cache[stopID]; ok {
		return name
	}
	name := stopID
	if stop, err := e.q.Stop(ctx, stopID); err == nil && stop.Name != "" {
		name = stop.Name
	}
	cache[stopID] = name
	return name
}

func (e *Evaluator) routeName(ctx context.Context, routeID string, cache map[string]string) string {
	if name, ok := cache[routeID]; ok {
		return name
	}
	name := routeID
	if route, err := e.q.Route(ctx, routeID); err == nil && route.ShortName != "" {
		name = route.ShortName
	}
	cache[routeID] = name
	return name
}

// GenerateVAPIDKeys creates a new VAPID key pair, public key first.
func GenerateVAPIDKeys() (publicKey, privateKey string, err error) {
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", "", fmt.Errorf("generate VAPID keys: %w", err)
	}
	return pub, priv, nil
}
