# CSI Driver for VergeOS
BINARY_NAME := csi-vergeos
IMAGE_NAME := ghcr.io/encore-platform-engineering/csi-vergeos
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GOFLAGS := -ldflags="-s -w -X github.com/verge-io/csi-vergeos/pkg/driver.DriverVersion=$(VERSION)"

.PHONY: all build build-linux test vet lint clean docker-build docker-push help

all: test build

## Build the binary for the current platform
build:
	go build $(GOFLAGS) -o bin/$(BINARY_NAME) ./cmd/csi-vergeos/

## Build a Linux amd64 binary (for Docker / Kubernetes)
build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build $(GOFLAGS) -o bin/$(BINARY_NAME)-linux-amd64 ./cmd/csi-vergeos/

## Run all tests
test:
	go test ./... -v -race

## Run go vet
vet:
	go vet ./...

## Run static analysis (requires golangci-lint)
lint:
	golangci-lint run ./...

## Build Docker image for linux/amd64 (local only)
docker-build:
	docker buildx build --platform linux/amd64 --provenance=false -t $(IMAGE_NAME):$(VERSION) --load .

## Build and push Docker image for linux/amd64
docker-push:
	docker buildx build --platform linux/amd64 --provenance=false -t $(IMAGE_NAME):$(VERSION) --push .

## Remove build artifacts
clean:
	rm -rf bin/

## Show help
help:
	@echo "Targets:"
	@grep -E '^## ' Makefile | sed 's/## /  /'
