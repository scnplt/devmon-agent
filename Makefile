# devmon-agent — single source of truth for build, test, lint and scan commands.

MODULE     := github.com/scnplt/devmon-agent
BIN        := bin/devmon-agent
VERSION    ?= dev
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Stamped into internal/version at link time. Defined once here so it is never retyped.
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildTime=$(BUILD_TIME)

# CGO_ENABLED=0 keeps the binary static so it runs on distroless/static.
# modernc.org/sqlite is pure Go, so nothing is lost by disabling cgo.
export CGO_ENABLED := 0

# Coverage is measured over ./internal/... only. cmd/ is wiring exercised by the
# manual checklist, not by unit tests; including it would understate the honest
# number for the code that is actually testable.
COVER_PKGS := ./internal/...

.PHONY: all build test test-race cover lint sec image fmt clean

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/devmon-agent

test:
	go test $(COVER_PKGS)

# -race is implemented in cgo, so these two targets must re-enable it. They need a
# C toolchain on PATH; the shipped binary is still built with CGO_ENABLED=0.
test-race:
	CGO_ENABLED=1 go test $(COVER_PKGS) -race

cover:
	CGO_ENABLED=1 go test $(COVER_PKGS) -race -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -1

fmt:
	gofmt -l -w .

# golangci-lint is preferred. When it is not installed, `go vet` is the minimum bar.
lint:
	gofmt -l .
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 \
		&& golangci-lint run ./... \
		|| echo "golangci-lint not installed — go vet was the only lint run"

sec:
	gosec ./...

image:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t devmon-agent:$(VERSION) .

clean:
	rm -rf bin coverage.out
