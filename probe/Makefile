# Run from probe/. Needs Go, and Docker for image, soak and smoke targets.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
IMAGE   ?= ondrya-probe:dev
FIXTURE ?= ondrya-probe:fixture

.PHONY: build test race fuzz lint vuln vet image image-fixture soak soak-full smoke clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -buildid= -X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o bin/probe .

vet:
	go vet ./...
	go vet -tags fixture ./...
	go vet -tags soak ./test/...

test: vet
	go test -count=1 ./...

# The spec requires the race detector in CI. It needs cgo; this target runs it
# in a Go container so a host without a C compiler can still do it.
race:
	docker run --rm -v "$(CURDIR):/src" -w /src -e CGO_ENABLED=1 golang:1.26 go test -race -count=1 ./...

fuzz:
	go test -run '^$$' -fuzz FuzzParsePing -fuzztime 30s ./internal/proto
	go test -run '^$$' -fuzz FuzzOriginAllowed -fuzztime 30s ./internal/check
	go test -run '^$$' -fuzz FuzzHostMatches -fuzztime 30s ./internal/check

lint:
	golangci-lint run ./...

vuln:
	govulncheck ./...

image:
	docker build -t $(IMAGE) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

image-fixture:
	docker build -t $(FIXTURE) --build-arg TAGS=fixture --build-arg VERSION=$(VERSION)-fixture --build-arg COMMIT=$(COMMIT) .

# Failure and soak tests against the real image (spec section 10). The churn
# test runs 20 s here; soak-full runs the 30 minutes the spec asks for.
soak: image image-fixture
	SOAK_IMAGE=$(IMAGE) SOAK_FIXTURE_IMAGE=$(FIXTURE) SOAK_REPORT=$(CURDIR)/test/soak/report.jsonl \
	  go test -tags soak -count=1 -v -timeout 40m ./test/soak

soak-full: image image-fixture
	SOAK_CHURN_DURATION=30m SOAK_IMAGE=$(IMAGE) SOAK_FIXTURE_IMAGE=$(FIXTURE) SOAK_REPORT=$(CURDIR)/test/soak/report.jsonl \
	  go test -tags soak -count=1 -v -timeout 60m ./test/soak

# Starts the container with deploy/docker-compose.yml and waits for its own
# health check to pass.
smoke: image
	IMAGE=$(IMAGE) sh test/compose-smoke.sh

clean:
	rm -rf bin test/soak/report.jsonl
