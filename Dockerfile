# Build stage. GOTOOLCHAIN=auto lets the pinned Go 1.27.0 be fetched even when
# the base image ships something older.
FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=auto

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/viiwork-gateway ./cmd/viiwork-gateway

# Runtime. Distroless, non-root: the gateway needs no shell, no package
# manager and no write access to anything.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/viiwork-gateway /viiwork-gateway
USER nonroot:nonroot
EXPOSE 8090
ENTRYPOINT ["/viiwork-gateway"]
