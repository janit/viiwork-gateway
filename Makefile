.PHONY: build test race docker up down clean

build:
	go build -trimpath -o bin/viiwork-gateway ./cmd/viiwork-gateway

test:
	go test ./...

race:
	go test -race ./...

integration:
	go test -tags=integration -race ./...

docker:
	docker build -t viiwork-gateway:latest .

up:
	docker compose up -d

down:
	docker compose down

clean:
	rm -rf bin/
