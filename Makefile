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

# The end-to-end suite needs a real Docker Engine and is excluded from every
# default target by the `e2e` build tag, so `go build ./...`, `go vet ./...` and
# `go test ./internal/...` never compile or run a line of it.
E2E_PKGS := ./internal/e2e/...

# -count=1 defeats the test cache: e2e results depend entirely on state outside
# the module, so a cached PASS from before a change is a false green.
#
# -v is here for one reason: `go test` prints a test's SKIP and its reason only
# under -v. Without it, a package whose tests all skipped reports a bare `ok`,
# indistinguishable in the log from one that ran and passed them — so a green
# run silently implies coverage it may not have. This suite skips for real and
# specific reasons (no Engine reachable, a group that needs a Linux Engine, an
# Engine too old for the field under test), and each has to be readable rather
# than inferred. It is the argument DEVMON_E2E_REQUIRE=1 already makes one
# level up, applied to the skips that flag deliberately does not convert into
# failures.
E2E_TESTFLAGS := -race -count=1 -v

.PHONY: all build test test-race cover lint sec shellcheck image fmt clean e2e e2e-container e2e-endurance e2e-lint e2e-clean

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

# install.sh is the one shipped artifact no Go gate covers, and it runs on an
# operator's host with sudo. -s sh, not the default, because the script is
# POSIX sh: shellcheck would otherwise let a bashism through that dash rejects.
shellcheck:
	shellcheck -s sh install.sh

# -race instruments the test binary only; the agent binary it builds and runs as
# a child process is still built with CGO_ENABLED=0, matching the shipped
# artifact. Requires a Linux Engine over unix:// or tcp:// — on Windows, run
# these from a WSL2 shell.
e2e:
	CGO_ENABLED=1 go test -tags e2e $(E2E_PKGS) $(E2E_TESTFLAGS) -timeout 15m

e2e-container:
	CGO_ENABLED=1 go test -tags e2e ./internal/e2e/incontainer/... $(E2E_TESTFLAGS) -timeout 15m

# The 30-minute stream and the retention budget. Both are compiled by every e2e
# run and skip unless DEVMON_E2E_ENDURANCE=1, which this target sets along with
# the longer timeout they need room inside.
e2e-endurance:
	DEVMON_E2E_ENDURANCE=1 CGO_ENABLED=1 go test -tags e2e ./internal/e2e/api/... $(E2E_TESTFLAGS) -timeout 45m

# `make lint` already covers the e2e files with gofmt, which ignores build tags.
# go vet and golangci-lint do not, which is the only reason this target exists.
# Do not fold `--build-tags e2e` into `lint`: it would pull the e2e packages into
# every ordinary lint run and slow the fast dev-PR path for no benefit.
# Removes containers a run that crashed hard enough to skip its own t.Cleanup
# left behind. Deliberately an EXPLICIT operator action and never automatic:
# the label matches every run's containers, so an implicit version could not
# tell a dead run's leftovers from a concurrent run's live agent container.
# Run it only when no e2e run is in flight.
e2e-clean:
	@ids=$$(docker ps -aq --filter label=com.devmon.e2e); \
	if [ -n "$$ids" ]; then \
		echo "$$ids" | xargs docker rm -f; \
	else \
		echo "no com.devmon.e2e containers to remove"; \
	fi

e2e-lint:
	go vet -tags e2e ./...
	@command -v golangci-lint >/dev/null 2>&1 \
		&& golangci-lint run --build-tags e2e $(E2E_PKGS) \
		|| echo "golangci-lint not installed — go vet -tags e2e was the only lint run"

image:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t devmon-agent:$(VERSION) .

clean:
	rm -rf bin coverage.out
