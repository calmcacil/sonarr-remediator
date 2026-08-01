.PHONY: build test lint run clean docker-build

build:
	go build -ldflags="-s -w" -o sonarr-remediator ./cmd/sonarr-remediator

test:
	go test ./...

lint:
	go vet ./...

run:
	go run ./cmd/sonarr-remediator --config config.example.yaml

clean:
	rm -f sonarr-remediator

docker-build:
	docker build -t ghcr.io/calmcacil/sonarr-remediator:latest .
