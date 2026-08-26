# Base images are digest-pinned rather than tag-pinned. A floating tag means two
# builds of the same commit are not the same build; the digest makes them
# identical. Dependabot's docker ecosystem watches a pinned digest and opens a PR
# when the tag moves, so the pin does not rot silently. The tag is kept next to
# the digest for readability only — Docker resolves the digest and ignores it.
FROM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build

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
#
# The `devmon` symlink is the operator-CLI alias: distroless has no shell and
# no ln, so the link must be created here and carried over by a *directory*
# COPY (a single-file COPY would dereference it into a second full copy of the
# binary). The relative target keeps the link valid wherever the directory
# lands. main.go dispatches on argv[0]: under the `devmon` name a bare
# invocation prints help instead of starting the daemon.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w \
      -X github.com/scnplt/devmon-agent/internal/version.Version=${VERSION} \
      -X github.com/scnplt/devmon-agent/internal/version.Commit=${COMMIT} \
      -X github.com/scnplt/devmon-agent/internal/version.BuildTime=${BUILD_TIME}" \
    -o /out/bin/devmon-agent ./cmd/devmon-agent \
 && ln -s devmon-agent /out/bin/devmon

# The default DEVMON_STATE_DIR (internal/config/config.go), staged here so the
# final stage can COPY it in with the right owner. distroless has no shell, so
# `RUN mkdir` is not available there and this is the only way to put a directory
# into the final image. 0700 because that directory holds the CA private key.
RUN mkdir -m 0700 -p /out/state

# No ca-certificates layer: the agent makes no outbound TLS connections, and
# distroless/static already carries a bundle if a later phase needs one.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# Directory COPY, deliberately: it preserves the `devmon` symlink next to the
# binary, so `docker exec devmon-agent devmon ...` resolves via the image PATH
# (/usr/local/bin is on distroless's default PATH) at the cost of a symlink,
# not a second copy of the binary.
COPY --from=build /out/bin/ /usr/local/bin/

# Pre-created and owned by the nonroot UID. UID 65532 cannot MkdirAll under a
# root-owned /var, so without this a bare `docker run` with no bind mount dies
# at startup with a permission-denied that reads as an agent fault. A bind mount
# over this path — what both compose files do — simply takes precedence.
COPY --from=build --chown=65532:65532 /out/state /var/lib/devmon

USER nonroot:nonroot
EXPOSE 8443

# The exec form with an absolute path is mandatory: distroless/static has no
# shell, so the shell form of CMD/HEALTHCHECK never runs there. `health`
# (cmd/devmon-agent/health.go) exists specifically to give this instruction
# something it can invoke without a shell or curl. --timeout=5s comfortably
# exceeds the subcommand's own 3-second client timeout, so a slow-but-alive
# listener is reported by the subcommand's own readable message rather than
# by Docker's generic timeout kill.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/devmon-agent", "health"]

ENTRYPOINT ["/usr/local/bin/devmon-agent"]
