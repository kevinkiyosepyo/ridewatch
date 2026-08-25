# syntax=docker/dockerfile:1

# Build stage runs on the build host's native platform and cross-compiles for
# the target (buildx multi-arch stays fast: no emulated compiler).
FROM --platform=$BUILDPLATFORM golang:1.27 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/ridewatch ./cmd/ridewatch

# Web assets and migrations are embedded in the binary, so the runtime image is
# just the binary on a distroless base (nonroot: uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/ridewatch /ridewatch

EXPOSE 8080
ENTRYPOINT ["/ridewatch"]
CMD ["serve"]
