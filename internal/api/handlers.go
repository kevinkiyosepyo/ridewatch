package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// --- shared JSON shapes ---

type stopJSON struct {
	StopID        string  `json:"stop_id"`
	Name          string  `json:"name"`
	Lat           float64 `json:"lat"`
	Lon           float64 `json:"lon"`
	ParentStation string  `json:"parent_station"`
	LocationType  int     `json:"location_type"`
	PlatformCode  string  `json:"platform_code"`
}

func toStopJSON(s domain.Stop) stopJSON {
	return stopJSON{
		StopID:        s.StopID,
		Name:          s.Name,
		Lat:           s.Lat,
		Lon:           s.Lon,
		ParentStation: s.ParentStation,
		LocationType:  s.LocationType,
		PlatformCode:  s.PlatformCode,
	}
}

type routeJSON struct {
	RouteID   string `json:"route_id"`
	ShortName string `json:"short_name"`
	LongName  string `json:"long_name"`
	RouteType int    `json:"route_type"`
	Color     string `json:"color"`
	TextColor string `json:"text_color"`
	SortOrder int    `json:"sort_order"`
}

func toRouteJSON(r domain.Route) routeJSON {
	return routeJSON{
		RouteID:   r.RouteID,
		ShortName: r.ShortName,
		LongName:  r.LongName,
		RouteType: r.RouteType,
		Color:     r.Color,
		TextColor: r.TextColor,
		SortOrder: r.SortOrder,
	}
}

type hourlyJSON struct {
	RouteID      string  `json:"route_id"`
	StopID       string  `json:"stop_id,omitempty"`
	DirectionID  int16   `json:"direction_id"`
	HourOfWeek   int     `json:"hour_of_week"`
	N            int     `json:"n"`
	P50DelaySecs *int    `json:"p50_delay_secs"`
	P90DelaySecs *int    `json:"p90_delay_secs"`
	Late5Pct     float32 `json:"late5_pct"`
	EarlyPct     float32 `json:"early_pct"`
}

func toHourlyJSON(rows []domain.HourlyStat) []hourlyJSON {
	out := make([]hourlyJSON, 0, len(rows))
	for _, h := range rows {
		out = append(out, hourlyJSON{
			RouteID:      h.RouteID,
			StopID:       h.StopID,
			DirectionID:  h.DirectionID,
			HourOfWeek:   h.HourOfWeek,
			N:            h.N,
			P50DelaySecs: h.P50DelaySecs,
			P90DelaySecs: h.P90DelaySecs,
			Late5Pct:     h.Late5Pct,
			EarlyPct:     h.EarlyPct,
		})
	}
	return out
}

type departureJSON struct {
	RouteID       string  `json:"route_id"`
	StopID        string  `json:"stop_id"`
	DirectionID   int16   `json:"direction_id"`
	ScheduledSecs int     `json:"scheduled_secs"`
	DayClass      string  `json:"day_class"`
	N             int     `json:"n"`
	P50DelaySecs  *int    `json:"p50_delay_secs"`
	P90DelaySecs  *int    `json:"p90_delay_secs"`
	Late5Pct      float32 `json:"late5_pct"`
}

// --- /api/vehicles ---

type pointGeometry struct {
	Type        string     `json:"type"`
	Coordinates [2]float64 `json:"coordinates"` // lon, lat
}

type vehicleProperties struct {
	VehicleID      string  `json:"vehicle_id"`
	RouteID        string  `json:"route_id"`
	RouteShortName string  `json:"route_short_name"`
	Headsign       string  `json:"headsign"`
	DelaySecs      *int    `json:"delay_secs"`
	Status         string  `json:"status"`
	Bearing        float32 `json:"bearing"`
	UpdatedAt      string  `json:"updated_at"`
}

type vehicleFeature struct {
	Type       string            `json:"type"`
	Geometry   pointGeometry     `json:"geometry"`
	Properties vehicleProperties `json:"properties"`
}

type featureCollection struct {
	Type     string           `json:"type"`
	Features []vehicleFeature `json:"features"`
}

func (s *server) handleVehicles(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	vehicles := s.live.LiveVehicles()
	fc := featureCollection{Type: "FeatureCollection", Features: make([]vehicleFeature, 0, len(vehicles))}
	for _, v := range vehicles {
		fc.Features = append(fc.Features, vehicleFeature{
			Type:     "Feature",
			Geometry: pointGeometry{Type: "Point", Coordinates: [2]float64{v.Lon, v.Lat}},
			Properties: vehicleProperties{
				VehicleID:      v.VehicleID,
				RouteID:        v.RouteID,
				RouteShortName: v.RouteShortName,
				Headsign:       v.Headsign,
				DelaySecs:      v.DelaySecs,
				Status:         v.Status,
				Bearing:        v.Bearing,
				UpdatedAt:      v.UpdatedAt.UTC().Format(time.RFC3339),
			},
		})
	}
	writeJSON(w, http.StatusOK, fc)
}

// --- /api/stops (search + bbox) ---

func (s *server) handleStops(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	switch {
	case query.Has("bbox"):
		minLon, minLat, maxLon, maxLat, err := parseBBox(query.Get("bbox"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		stops, err := s.q.StopsInBBox(r.Context(), minLat, minLon, maxLat, maxLon, bboxLimit)
		if err != nil {
			s.serverError(w, "stops in bbox", err)
			return
		}
		writeStops(w, stops)
	case query.Has("q"):
		q := strings.TrimSpace(query.Get("q"))
		if q == "" {
			writeError(w, http.StatusBadRequest, "q must not be empty")
			return
		}
		stops, err := s.q.SearchStops(r.Context(), q, searchLimit)
		if err != nil {
			s.serverError(w, "search stops", err)
			return
		}
		writeStops(w, stops)
	default:
		writeError(w, http.StatusBadRequest, "bbox or q parameter required")
	}
}

func writeStops(w http.ResponseWriter, stops []domain.Stop) {
	out := make([]stopJSON, 0, len(stops))
	for _, st := range stops {
		out = append(out, toStopJSON(st))
	}
	writeJSON(w, http.StatusOK, map[string]any{"stops": out})
}

// parseBBox parses "minLon,minLat,maxLon,maxLat".
func parseBBox(raw string) (minLon, minLat, maxLon, maxLat float64, err error) {
	parts := strings.Split(raw, ",")
	if len(parts) != 4 {
		return 0, 0, 0, 0, errors.New("bbox must be minLon,minLat,maxLon,maxLat")
	}
	vals := make([]float64, 4)
	for i, p := range parts {
		v, perr := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if perr != nil {
			return 0, 0, 0, 0, fmt.Errorf("bbox: bad number %q", p)
		}
		vals[i] = v
	}
	minLon, minLat, maxLon, maxLat = vals[0], vals[1], vals[2], vals[3]
	if minLon < -180 || maxLon > 180 || minLat < -90 || maxLat > 90 ||
		minLon > maxLon || minLat > maxLat {
		return 0, 0, 0, 0, errors.New("bbox out of range")
	}
	return minLon, minLat, maxLon, maxLat, nil
}

// --- /api/stops/{id} ---

func (s *server) handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	stop, err := s.q.Stop(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "stop not found")
			return
		}
		s.serverError(w, "load stop", err)
		return
	}
	routes, err := s.q.RoutesServingStop(r.Context(), id)
	if err != nil {
		s.serverError(w, "routes serving stop", err)
		return
	}
	routesOut := make([]routeJSON, 0, len(routes))
	for _, rt := range routes {
		routesOut = append(routesOut, toRouteJSON(rt))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stop":   toStopJSON(*stop),
		"routes": routesOut,
	})
}

// --- /api/stops/{id}/upcoming ---

type upcomingJSON struct {
	TripID         string  `json:"trip_id"`
	RouteID        string  `json:"route_id"`
	RouteShortName string  `json:"route_short_name"`
	Headsign       string  `json:"headsign"`
	DirectionID    int16   `json:"direction_id"`
	StopSequence   int     `json:"stop_sequence"`
	Scheduled      *string `json:"scheduled"`
	Predicted      *string `json:"predicted"`
	DelaySecs      *int    `json:"delay_secs"`
	VehicleID      string  `json:"vehicle_id"`
	Skipped        bool    `json:"skipped"`
}

func rfc3339OrNil(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}

func (s *server) handleUpcoming(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	id := r.PathValue("id")
	if _, err := s.q.Stop(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "stop not found")
			return
		}
		s.serverError(w, "load stop", err)
		return
	}
	shortNames := map[string]string{}
	if routes, err := s.q.RoutesServingStop(r.Context(), id); err == nil {
		for _, rt := range routes {
			shortNames[rt.RouteID] = rt.ShortName
		}
	}
	events := s.live.UpcomingAtStop(id, upcomingHorizon)
	out := make([]upcomingJSON, 0, len(events))
	for _, ev := range events {
		out = append(out, upcomingJSON{
			TripID:         ev.TripID,
			RouteID:        ev.RouteID,
			RouteShortName: shortNames[ev.RouteID],
			Headsign:       ev.Headsign,
			DirectionID:    ev.DirectionID,
			StopSequence:   ev.StopSequence,
			Scheduled:      rfc3339OrNil(ev.ScheduledArrival),
			Predicted:      rfc3339OrNil(ev.PredictedArrival),
			DelaySecs:      ev.DelaySecs,
			VehicleID:      ev.VehicleID,
			Skipped:        ev.Skipped,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"upcoming": out})
}

// --- /api/stops/{id}/reliability ---

func (s *server) handleStopReliability(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.q.Stop(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "stop not found")
			return
		}
		s.serverError(w, "load stop", err)
		return
	}
	hourly, err := s.q.StopHourly(r.Context(), id)
	if err != nil {
		s.serverError(w, "stop hourly rollups", err)
		return
	}
	departures, err := s.q.StopDepartures(r.Context(), id)
	if err != nil {
		s.serverError(w, "stop departure rollups", err)
		return
	}
	routeNames := map[string]string{}
	if routes, err := s.q.RoutesServingStop(r.Context(), id); err == nil {
		for _, rt := range routes {
			routeNames[rt.RouteID] = displayNameForRoute(rt)
		}
	}

	depsOut := make([]departureJSON, 0, len(departures))
	for _, d := range departures {
		depsOut = append(depsOut, departureJSON{
			RouteID:       d.RouteID,
			StopID:        d.StopID,
			DirectionID:   d.DirectionID,
			ScheduledSecs: d.ScheduledSecs,
			DayClass:      d.DayClass,
			N:             d.N,
			P50DelaySecs:  d.P50DelaySecs,
			P90DelaySecs:  d.P90DelaySecs,
			Late5Pct:      d.Late5Pct,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hourly":     toHourlyJSON(hourly),
		"departures": depsOut,
		"sentences":  ReliabilitySentences(hourly, departures, routeNames),
	})
}

func displayNameForRoute(r domain.Route) string {
	switch {
	case r.ShortName != "":
		return "Route " + r.ShortName
	case r.LongName != "":
		return r.LongName
	default:
		return "Route " + r.RouteID
	}
}

// --- /api/routes ---

func (s *server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	routes, err := s.q.Routes(r.Context())
	if err != nil {
		s.serverError(w, "list routes", err)
		return
	}
	out := make([]routeJSON, 0, len(routes))
	for _, rt := range routes {
		out = append(out, toRouteJSON(rt))
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": out})
}

func (s *server) handleRouteReliability(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	route, err := s.q.Route(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "route not found")
			return
		}
		s.serverError(w, "load route", err)
		return
	}
	hourly, err := s.q.RouteHourly(r.Context(), id)
	if err != nil {
		s.serverError(w, "route hourly rollups", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"route":  toRouteJSON(*route),
		"hourly": toHourlyJSON(hourly),
	})
}

// --- /api/feedinfo + /healthz ---

type feedStatusJSON struct {
	AgeSecs *int `json:"age_secs"`
	OK      bool `json:"ok"`
}

func (s *server) feedStatus(feed domain.Feed) feedStatusJSON {
	age, ok := s.live.FeedAge(feed)
	st := feedStatusJSON{OK: ok}
	if ok {
		secs := int(age.Seconds())
		st.AgeSecs = &secs
	}
	return st
}

func (s *server) feedStatuses() map[string]feedStatusJSON {
	return map[string]feedStatusJSON{
		string(domain.FeedVehiclePositions): s.feedStatus(domain.FeedVehiclePositions),
		string(domain.FeedTripUpdates):      s.feedStatus(domain.FeedTripUpdates),
	}
}

func (s *server) handleFeedInfo(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"feeds":        s.feedStatuses(),
		"tile_url":     s.cfg.TileURL,
		"push_enabled": s.cfg.VAPIDPublicKey != "" && s.cfg.VAPIDPrivateKey != "",
	})
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"feeds":  s.feedStatuses(),
	})
}

func (s *server) handleVAPIDPublicKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"key": s.cfg.VAPIDPublicKey})
}

// --- subscriptions ---

const maxSubscriptionBody = 16 << 10

type subscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	StopID        string `json:"stop_id"`
	RouteID       string `json:"route_id"`
	DirectionID   *int16 `json:"direction_id"`
	ThresholdSecs *int   `json:"threshold_secs"`
}

// decodeStrict decodes a JSON body rejecting unknown fields, trailing data, and
// bodies over maxSubscriptionBody.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxSubscriptionBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected trailing data")
	}
	return nil
}

func badRequestOrTooLarge(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
}

func (s *server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	var req subscribeRequest
	if err := decodeStrict(w, r, &req); err != nil {
		badRequestOrTooLarge(w, err)
		return
	}
	u, err := url.Parse(req.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		writeError(w, http.StatusBadRequest, "endpoint must be an https URL")
		return
	}
	if req.Keys.P256dh == "" || req.Keys.Auth == "" {
		writeError(w, http.StatusBadRequest, "keys.p256dh and keys.auth are required")
		return
	}
	if req.StopID == "" {
		writeError(w, http.StatusBadRequest, "stop_id is required")
		return
	}
	direction := int16(-1)
	if req.DirectionID != nil {
		direction = *req.DirectionID
		if direction != -1 && direction != 0 && direction != 1 {
			writeError(w, http.StatusBadRequest, "direction_id must be 0, 1, or -1")
			return
		}
	}
	threshold := 300
	if req.ThresholdSecs != nil {
		threshold = min(max(*req.ThresholdSecs, 60), 3600)
	}
	if _, err := s.q.Stop(r.Context(), req.StopID); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "stop not found")
			return
		}
		s.serverError(w, "load stop", err)
		return
	}
	id, err := s.subs.SaveSubscription(r.Context(), domain.Subscription{
		Endpoint:      req.Endpoint,
		P256dh:        req.Keys.P256dh,
		Auth:          req.Keys.Auth,
		StopID:        req.StopID,
		RouteID:       req.RouteID,
		DirectionID:   direction,
		ThresholdSecs: threshold,
	})
	if err != nil {
		s.serverError(w, "save subscription", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := decodeStrict(w, r, &req); err != nil {
		badRequestOrTooLarge(w, err)
		return
	}
	if req.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "endpoint is required")
		return
	}
	if err := s.subs.DeleteSubscription(r.Context(), req.Endpoint); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, "subscription not found")
			return
		}
		s.serverError(w, "delete subscription", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) serverError(w http.ResponseWriter, op string, err error) {
	s.log.Error("api error", "op", op, "err", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}
