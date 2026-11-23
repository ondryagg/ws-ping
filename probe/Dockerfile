# syntax=docker/dockerfile:1
#
# Build context is the probe/ directory.
#   docker build -t ondrya-probe:dev --build-arg VERSION=1.0.0 --build-arg COMMIT=$(git rev-parse HEAD) .
# The failure-injection build used only by the soak tests:
#   docker build -t ondrya-probe:fixture --build-arg TAGS=fixture .
#
# Both base images are pinned by digest so the build is reproducible. Update
# the digests deliberately (docs/latency-probe-setup.md, "Upgrading").

FROM golang:1.26@sha256:4165ee6bf3b7811fd7b40373290405c83d0167dcb87ee5f29c427cdcef45ff8f AS build
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG TAGS=""
RUN go build -trimpath -tags "${TAGS}" \
      -ldflags "-s -w -buildid= -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/probe .

# Distroless static: CA certificates, no shell, no package manager, and a
# built-in non-root user (65532).
FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
LABEL org.opencontainers.image.source="https://github.com/ondryagg/ws-ping" \
      org.opencontainers.image.description="Ondrya latency probe: WebSocket echo service" \
      org.opencontainers.image.licenses="NOASSERTION"
COPY --from=build /out/probe /probe
USER 65532:65532
ENTRYPOINT ["/probe"]
