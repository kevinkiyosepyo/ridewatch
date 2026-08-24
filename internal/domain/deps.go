//go:build ridewatch_pin_deps

package domain

// Pins the module's dependency set for `go mod tidy` while components that
// import these are still being built. Never compiled (see build tag).
import (
	_ "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	_ "github.com/SherClockHolmes/webpush-go"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/prometheus/client_golang/prometheus/promhttp"
	_ "google.golang.org/protobuf/proto"
)
