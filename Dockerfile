# Build stage. GOTOOLCHAIN=auto lets the pinned Go 1.27.1 be fetched even when
# the base image ships something older.
FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=auto

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# VERSION is stamped into the binary, so a running gateway can say which build
# it is — and so can every member of the mesh, which reads it from metadata.
ARG VERSION=dev
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/viiwork-gateway ./cmd/viiwork-gateway

# Runtime. Distroless, non-root: the gateway needs no shell, no package
# manager and no write access to anything.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/viiwork-gateway /viiwork-gateway
USER nonroot:nonroot
# Informational only: the container runs with host networking, so these are
# not published ports. 8090 is the API on loopback; 7946 is gossip, tcp and
# udp both, bound to the tailnet advertise address.
EXPOSE 8090 7946/tcp 7946/udp
ENTRYPOINT ["/viiwork-gateway"]
