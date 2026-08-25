package ingest

import (
	"fmt"
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

func marshalFeed(t *testing.T, headerTS uint64, entities ...*gtfs.FeedEntity) []byte {
	t.Helper()
	msg := &gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{
			GtfsRealtimeVersion: proto.String("2.0"),
			Timestamp:           proto.Uint64(headerTS),
		},
		Entity: entities,
	}
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return raw
}

func tripEntities(t *testing.T, headerTS uint64, updates ...*gtfs.TripUpdate) []byte {
	t.Helper()
	var ents []*gtfs.FeedEntity
	for i, tu := range updates {
		ents = append(ents, &gtfs.FeedEntity{Id: proto.String(fmt.Sprintf("e%d", i)), TripUpdate: tu})
	}
	return marshalFeed(t, headerTS, ents...)
}

func vehicleEntities(t *testing.T, headerTS uint64, vehicles ...*gtfs.VehiclePosition) []byte {
	t.Helper()
	var ents []*gtfs.FeedEntity
	for i, vp := range vehicles {
		ents = append(ents, &gtfs.FeedEntity{Id: proto.String(fmt.Sprintf("e%d", i)), Vehicle: vp})
	}
	return marshalFeed(t, headerTS, ents...)
}

func TestDecodeFeedTripUpdates(t *testing.T) {
	full := &gtfs.TripUpdate{
		Trip: &gtfs.TripDescriptor{
			TripId:               proto.String("trip-1"),
			RouteId:              proto.String("route-1"),
			DirectionId:          proto.Uint32(1),
			StartTime:            proto.String("08:15:00"),
			StartDate:            proto.String("20260824"),
			ScheduleRelationship: gtfs.TripDescriptor_CANCELED.Enum(),
		},
		Vehicle:   &gtfs.VehicleDescriptor{Id: proto.String("veh-9")},
		Timestamp: proto.Uint64(1756000123),
		Delay:     proto.Int32(-45),
		StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{
			{
				StopSequence:         proto.Uint32(5),
				StopId:               proto.String("stop-5"),
				Arrival:              &gtfs.TripUpdate_StopTimeEvent{Time: proto.Int64(1756000500), Delay: proto.Int32(60)},
				Departure:            &gtfs.TripUpdate_StopTimeEvent{Time: proto.Int64(1756000530)},
				ScheduleRelationship: gtfs.TripUpdate_StopTimeUpdate_SKIPPED.Enum(),
			},
			{}, // every optional field absent
		},
	}
	minimal := &gtfs.TripUpdate{Trip: &gtfs.TripDescriptor{TripId: proto.String("trip-2")}}

	raw := tripEntities(t, 1756000200, full, minimal)
	polledAt := time.Unix(1756000201, 0).UTC()
	snap, err := DecodeFeed(domain.FeedTripUpdates, raw, polledAt)
	if err != nil {
		t.Fatalf("DecodeFeed: %v", err)
	}
	if snap.Feed != domain.FeedTripUpdates || !snap.PolledAt.Equal(polledAt) {
		t.Errorf("snapshot identity: feed=%q polledAt=%v", snap.Feed, snap.PolledAt)
	}
	if snap.FeedTimestamp != 1756000200 {
		t.Errorf("FeedTimestamp = %d, want 1756000200", snap.FeedTimestamp)
	}
	if len(snap.Vehicles) != 0 || len(snap.TripUpdates) != 2 {
		t.Fatalf("got %d trip updates, %d vehicles", len(snap.TripUpdates), len(snap.Vehicles))
	}

	tu := snap.TripUpdates[0]
	if tu.TripID != "trip-1" || tu.RouteID != "route-1" || tu.DirectionID != 1 ||
		tu.StartDate != "20260824" || tu.StartTime != "08:15:00" ||
		tu.ScheduleRelationship != "CANCELED" || tu.VehicleID != "veh-9" ||
		tu.Timestamp != 1756000123 {
		t.Errorf("full trip update mapped wrong: %+v", tu)
	}
	if !tu.DelaySet || tu.DelaySecs != -45 {
		t.Errorf("trip delay: set=%v secs=%d, want set -45", tu.DelaySet, tu.DelaySecs)
	}
	if len(tu.StopTimeUpdates) != 2 {
		t.Fatalf("got %d stop time updates, want 2", len(tu.StopTimeUpdates))
	}
	stu := tu.StopTimeUpdates[0]
	if stu.StopSequence != 5 || stu.StopID != "stop-5" || stu.Relationship != "SKIPPED" {
		t.Errorf("stu[0] identity: %+v", stu)
	}
	if stu.ArrivalTime != 1756000500 || !stu.ArrivalDelaySet || stu.ArrivalDelay != 60 {
		t.Errorf("stu[0] arrival: %+v", stu)
	}
	if stu.DepartureTime != 1756000530 || stu.DepartureDelaySet || stu.DepartureDelay != 0 {
		t.Errorf("stu[0] departure: time+no delay expected, got %+v", stu)
	}

	// Sentinels for the all-absent StopTimeUpdate.
	empty := tu.StopTimeUpdates[1]
	want := domain.StopTimeUpdate{StopSequence: -1, Relationship: "SCHEDULED"}
	if empty != want {
		t.Errorf("empty stu = %+v, want %+v", empty, want)
	}

	// Sentinels for the minimal trip update.
	m := snap.TripUpdates[1]
	if m.TripID != "trip-2" || m.RouteID != "" || m.DirectionID != -1 ||
		m.StartDate != "" || m.StartTime != "" || m.ScheduleRelationship != "SCHEDULED" ||
		m.VehicleID != "" || m.Timestamp != 0 || m.DelaySet || m.DelaySecs != 0 ||
		len(m.StopTimeUpdates) != 0 {
		t.Errorf("minimal trip update sentinels wrong: %+v", m)
	}
}

func TestDecodeFeedVehicles(t *testing.T) {
	full := &gtfs.VehiclePosition{
		Trip: &gtfs.TripDescriptor{
			TripId:    proto.String("trip-1"),
			RouteId:   proto.String("route-1"),
			StartDate: proto.String("20260824"),
		},
		Vehicle: &gtfs.VehicleDescriptor{Id: proto.String("veh-1"), Label: proto.String("Car 1")},
		Position: &gtfs.Position{
			Latitude:  proto.Float32(42.5),
			Longitude: proto.Float32(-71.25),
			Bearing:   proto.Float32(90),
			Speed:     proto.Float32(12.5),
		},
		CurrentStopSequence: proto.Uint32(7),
		StopId:              proto.String("stop-7"),
		CurrentStatus:       gtfs.VehiclePosition_STOPPED_AT.Enum(),
		Timestamp:           proto.Uint64(1756000042),
	}
	minimal := &gtfs.VehiclePosition{}

	raw := vehicleEntities(t, 1756000100, full, minimal)
	snap, err := DecodeFeed(domain.FeedVehiclePositions, raw, time.Unix(1756000101, 0).UTC())
	if err != nil {
		t.Fatalf("DecodeFeed: %v", err)
	}
	if len(snap.TripUpdates) != 0 || len(snap.Vehicles) != 2 {
		t.Fatalf("got %d trip updates, %d vehicles", len(snap.TripUpdates), len(snap.Vehicles))
	}

	v := snap.Vehicles[0]
	if v.VehicleID != "veh-1" || v.Label != "Car 1" || v.TripID != "trip-1" ||
		v.RouteID != "route-1" || v.StartDate != "20260824" ||
		v.Lat != 42.5 || v.Lon != -71.25 || v.Bearing != 90 || v.SpeedMPS != 12.5 ||
		v.StopSequence != 7 || v.StopID != "stop-7" || v.Status != "STOPPED_AT" ||
		v.Timestamp != 1756000042 {
		t.Errorf("full vehicle mapped wrong: %+v", v)
	}

	m := snap.Vehicles[1]
	want := domain.VehiclePosition{Bearing: -1, SpeedMPS: -1, StopSequence: -1}
	if m != want {
		t.Errorf("minimal vehicle sentinels = %+v, want %+v", m, want)
	}
}

func TestDecodeFeedErrors(t *testing.T) {
	if _, err := DecodeFeed(domain.FeedTripUpdates, []byte{0xff, 0xff, 0xff, 0xff}, time.Now()); err == nil {
		t.Error("garbage payload: want error")
	}
	raw := vehicleEntities(t, 1, &gtfs.VehiclePosition{})
	if _, err := DecodeFeed(domain.Feed("alerts"), raw, time.Now()); err == nil {
		t.Error("unknown feed: want error")
	}
}
