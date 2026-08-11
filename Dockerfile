FROM golang:1.26-alpine AS build

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

# No ca-certificates layer: the agent makes no outbound TLS connections, and
# distroless/static already carries a bundle if a later phase needs one.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/devmon-agent /usr/local/bin/devmon-agent

USER nonroot:nonroot
EXPOSE 8443

ENTRYPOINT ["/usr/local/bin/devmon-agent"]
