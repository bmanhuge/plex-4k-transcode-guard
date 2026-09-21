# Developer entry points. CI runs the same targets.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
IMAGE ?= ghcr.io/bmanhuge/plex-4k-transcode-guard
PLATFORMS ?= linux/amd64,linux/arm64
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test race lint fmt vet staticcheck shellcheck image image-multiarch verify-image clean

all: lint test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/plex-4k-guard ./cmd/plex-4k-guard

build-all:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/plex-4k-guard-linux-amd64 ./cmd/plex-4k-guard
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/plex-4k-guard-linux-arm64 ./cmd/plex-4k-guard

test:
	go test -count=1 -timeout 5m ./...

race:
	go test -race -count=1 -timeout 10m ./...

fmt:
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo 'gofmt: files above need formatting' && exit 1)

vet:
	go vet ./...

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...

shellcheck:
	shellcheck -s bash root/etc/s6-overlay/s6-rc.d/*/run contrib/*.sh

lint: fmt vet staticcheck

image:
	docker build --build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION) \
		--build-arg CREATED=$$(date -u +%Y-%m-%dT%H:%M:%SZ) -t $(IMAGE):dev .

image-multiarch:
	docker buildx build --platform $(PLATFORMS) --provenance=false --sbom=false \
		--build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION) \
		--build-arg CREATED=$$(date -u +%Y-%m-%dT%H:%M:%SZ) -t $(IMAGE):dev .

# Asserts the single-layer contract the LinuxServer.io loader relies on.
verify-image:
	test "$$(docker image inspect $(IMAGE):dev --format '{{len .RootFS.Layers}}')" = "1"
	docker run --rm --entrypoint /usr/local/bin/plex-4k-guard $(IMAGE):dev --version

clean:
	rm -rf bin dist
