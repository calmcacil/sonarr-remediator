.PHONY: build test run clean docker-build

BINARY=sonarr-remediator
CMD_DIR=./cmd/sonarr-remediator

build:
	go build -ldflags="-s -w" -o $(BINARY) $(CMD_DIR)

test:
	go test ./...

test-cover:
	go test -coverprofile=coverage.out ./...

lint:
	go vet ./...

run:
	go run $(CMD_DIR) --config config.example.yaml

clean:
	rm -f $(BINARY)

docker-build:
	docker build -t ghcr.io/calmcacil/sonarr-remediator:latest .