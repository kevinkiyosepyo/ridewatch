.PHONY: build run test vet lint db-up db-down migrate replay clean

build:
	go build -o bin/ridewatch ./cmd/ridewatch

run: build
	./bin/ridewatch serve

test:
	go test ./...

vet:
	go vet ./...

db-up:
	docker compose -f docker-compose.dev.yml up -d --wait

db-down:
	docker compose -f docker-compose.dev.yml down

migrate: build
	./bin/ridewatch migrate

vapid: build
	./bin/ridewatch vapid-keys

clean:
	rm -rf bin
