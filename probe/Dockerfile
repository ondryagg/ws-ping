# syntax=docker/dockerfile:1
#
# Build context is the probe/ directory.
#   docker build -t ondrya-probe:dev --build-arg VERSION=1.0.0 --build-arg COMMIT=$(git rev-parse HEAD) .
# The failure-injection build used only by the soak tests:
#   docker build -t ondrya-probe:fixture --build-arg TAGS=fixture .
#
# Both base images are pinned by digest so the build is reproducible. Update
# the digests deliberately (docs/latency-probe-setup.md, "Upgrading").

FROM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
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
COPY --from=build /out/probe /probe
USER 65532:65532
ENTRYPOINT ["/probe"]
