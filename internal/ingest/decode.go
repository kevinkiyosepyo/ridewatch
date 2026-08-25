package ingest

import (
	"fmt"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// DecodeFeed is the single protobuf→domain mapping, shared by the poller and
// Replay. Absent optional fields map to the documented sentinels (-1 / "" / 0);
// schedule-relationship and vehicle-status enums render as their proto enum
// names. RawSHA256 and RawPath are left for the caller to fill in.
func DecodeFeed(feed domain.Feed, raw []byte, polledAt time.Time) (*domain.Snapshot, error) {
	var msg gtfs.FeedMessage
	if err := proto.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("decode %s: %w", feed, err)
	}
	snap := &domain.Snapshot{
		Feed:          feed,
		PolledAt:      polledAt,
		FeedTimestamp: msg.GetHeader().GetTimestamp(),
	}
	switch feed {
	case domain.FeedTripUpdates:
		for _, ent := range msg.GetEntity() {
			if tu := ent.GetTripUpdate(); tu != nil {
				snap.TripUpdates = append(snap.TripUpdates, decodeTripUpdate(tu))
			}
		}
	case domain.FeedVehiclePositions:
		for _, ent := range msg.GetEntity() {
			if vp := ent.GetVehicle(); vp != nil {
				snap.Vehicles = append(snap.Vehicles, decodeVehicle(vp))
			}
		}
	default:
		return nil, fmt.Errorf("decode: unknown feed %q", feed)
	}
	return snap, nil
}

func decodeTripUpdate(tu *gtfs.TripUpdate) domain.TripUpdate {
	td := tu.GetTrip()
	out := domain.TripUpdate{
		TripID:      td.GetTripId(),
		RouteID:     td.GetRouteId(),
		DirectionID: -1,
		StartDate:   td.GetStartDate(),
		StartTime:   td.GetStartTime(),
		// Nil-safe getter: an absent field yields the proto default, SCHEDULED.
		ScheduleRelationship: td.GetScheduleRelationship().String(),
		VehicleID:            tu.GetVehicle().GetId(),
		Timestamp:            tu.GetTimestamp(),
	}
	if td != nil && td.DirectionId != nil {
		out.DirectionID = int16(td.GetDirectionId())
	}
	if tu.Delay != nil {
		out.DelaySecs = tu.GetDelay()
		out.DelaySet = true
	}
	for _, stu := range tu.GetStopTimeUpdate() {
		out.StopTimeUpdates = append(out.StopTimeUpdates, decodeStopTimeUpdate(stu))
	}
	return out
}

func decodeStopTimeUpdate(stu *gtfs.TripUpdate_StopTimeUpdate) domain.StopTimeUpdate {
	out := domain.StopTimeUpdate{
		StopSequence: -1,
		StopID:       stu.GetStopId(),
		Relationship: stu.GetScheduleRelationship().String(),
	}
	if stu.StopSequence != nil {
		out.StopSequence = int(stu.GetStopSequence())
	}
	if a := stu.GetArrival(); a != nil {
		out.ArrivalTime = a.GetTime()
		if a.Delay != nil {
			out.ArrivalDelay = a.GetDelay()
			out.ArrivalDelaySet = true
		}
	}
	if d := stu.GetDeparture(); d != nil {
		out.DepartureTime = d.GetTime()
		if d.Delay != nil {
			out.DepartureDelay = d.GetDelay()
			out.DepartureDelaySet = true
		}
	}
	return out
}

func decodeVehicle(vp *gtfs.VehiclePosition) domain.VehiclePosition {
	td := vp.GetTrip()
	pos := vp.GetPosition()
	out := domain.VehiclePosition{
		VehicleID:    vp.GetVehicle().GetId(),
		Label:        vp.GetVehicle().GetLabel(),
		TripID:       td.GetTripId(),
		RouteID:      td.GetRouteId(),
		StartDate:    td.GetStartDate(),
		Lat:          float64(pos.GetLatitude()),
		Lon:          float64(pos.GetLongitude()),
		Bearing:      -1,
		SpeedMPS:     -1,
		StopSequence: -1,
		StopID:       vp.GetStopId(),
		Timestamp:    vp.GetTimestamp(),
	}
	if pos != nil && pos.Bearing != nil {
		out.Bearing = pos.GetBearing()
	}
	if pos != nil && pos.Speed != nil {
		out.SpeedMPS = pos.GetSpeed()
	}
	if vp.CurrentStopSequence != nil {
		out.StopSequence = int(vp.GetCurrentStopSequence())
	}
	// current_status defaults to IN_TRANSIT_TO in the proto, but when the field
	// is absent the domain sentinel is "" (status is only meaningful alongside
	// current_stop_sequence anyway).
	if vp.CurrentStatus != nil {
		out.Status = vp.GetCurrentStatus().String()
	}
	return out
}
