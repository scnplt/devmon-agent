# Base images are digest-pinned rather than tag-pinned. A floating tag means two
# builds of the same commit are not the same build; the digest makes them
# identical. Dependabot's docker ecosystem watches a pinned digest and opens a PR
# when the tag moves, so the pin does not rot silently. The tag is kept next to
# the digest for readability only — Docker resolves the digest and ignores it.
FROM golang:1.26-alpine@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS build

# GOTOOLCHAIN=local, not the default `auto`. go.mod requires go 1.26.5, bumped
# specifically for GO-2026-5856. Under `auto`, a base image that has drifted
# below that requirement does not fail the build — Go quietly downloads the
# newer toolchain mid-build, turning a hermetic build into one with a network
# dependency and an unpinned compiler. `local` turns exactly that situation into
# a loud error naming both versions, which is the signal we want.
ENV GOTOOLCHAIN=local

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_TIME=unknown

# CGO_ENABLED=0 is mandatory, not a preference. modernc.org/sqlite is pure Go,
# so disabling cgo costs nothing and keeps the binary static — which is what lets
# it run on distroless/static. With cgo left on, the binary links against musl
# and distroless reports "no such file or directory" for a file that plainly
# exists, which is a genuinely confusing way to spend an afternoon.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w \
      -X github.com/scnplt/devmon-agent/internal/version.Version=${VERSION} \
      -X github.com/scnplt/devmon-agent/internal/version.Commit=${COMMIT} \
      -X github.com/scnplt/devmon-agent/internal/version.BuildTime=${BUILD_TIME}" \
    -o /out/devmon-agent ./cmd/devmon-agent

# The default DEVMON_STATE_DIR (internal/config/config.go), staged here so the
# final stage can COPY it in with the right owner. distroless has no shell, so
# `RUN mkdir` is not available there and this is the only way to put a directory
# into the final image. 0700 because that directory holds the CA private key.
RUN mkdir -m 0700 -p /out/state

# No ca-certificates layer: the agent makes no outbound TLS connections, and
# distroless/static already carries a bundle if a later phase needs one.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a

COPY --from=build /out/devmon-agent /usr/local/bin/devmon-agent

# Pre-created and owned by the nonroot UID. UID 65532 cannot MkdirAll under a
# root-owned /var, so without this a bare `docker run` with no bind mount dies
# at startup with a permission-denied that reads as an agent fault. A bind mount
# over this path — what both compose files do — simply takes precedence.
COPY --from=build --chown=65532:65532 /out/state /var/lib/devmon

USER nonroot:nonroot
EXPOSE 8443

ENTRYPOINT ["/usr/local/bin/devmon-agent"]
