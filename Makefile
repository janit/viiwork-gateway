.PHONY: build test race integration docker up down deploy clean

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

# Deploy runs ON the deployment host, from a published tag.
# See scripts/deploy.sh --help.
deploy:
	./scripts/deploy.sh --latest

clean:
	rm -rf bin/
