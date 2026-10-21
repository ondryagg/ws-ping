#!/bin/sh
# Starts deploy/docker-compose.yml with a throwaway certificate laid out the way
# certbot does (live/<host>/ symlinks into archive/<host>/) and waits for
# the container's own HEALTHCHECK (`/probe healthcheck`) to report healthy.
# Needs docker, docker compose and openssl.
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
IMAGE=${IMAGE:-ondrya-probe:dev}
WORK=$(mktemp -d)
PORT=${PORT:-18443}
PROJECT=probe-smoke
trap 'docker compose -p "$PROJECT" -f "$ROOT/deploy/docker-compose.yml" down -v >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

mkdir -p "$WORK/le/archive/probe.test" "$WORK/le/live/probe.test" "$WORK/state" "$WORK/run"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 \
  -subj "/CN=probe.test" -addext "subjectAltName=DNS:probe.test" \
  -keyout "$WORK/le/archive/probe.test/privkey1.pem" -out "$WORK/le/archive/probe.test/fullchain1.pem" 2>/dev/null
ln -s ../../archive/probe.test/fullchain1.pem "$WORK/le/live/probe.test/fullchain.pem"
ln -s ../../archive/probe.test/privkey1.pem "$WORK/le/live/probe.test/privkey.pem"
chmod -R a+rX "$WORK/le"; chmod 0755 "$WORK" "$WORK/run"
chmod 0777 "$WORK/state"

export PROBE_IMAGE="$IMAGE" PROBE_HOSTNAME=probe.test PROBE_LISTEN=":$PORT" PROBE_CERT_GID=65532 \
  PROBE_ALLOWED_ORIGINS=https://ondrya.com PROBE_NODE_ID=smoke-01 \
  PROBE_TLS_LETSENCRYPT_DIR="$WORK/le" PROBE_STATE_DIR="$WORK/state" PROBE_RUN_DIR="$WORK/run" \
  PROBE_METRICS_LISTEN=127.0.0.1:19109

docker compose -p "$PROJECT" -f "$ROOT/deploy/docker-compose.yml" up -d

i=0
while [ "$i" -lt 40 ]; do
  status=$(docker inspect "$PROJECT-probe-1" --format '{{.State.Health.Status}}' 2>/dev/null || echo starting)
  echo "health: $status"
  [ "$status" = healthy ] && break
  i=$((i + 1)); sleep 2
done
[ "$status" = healthy ] || { docker compose -p "$PROJECT" -f "$ROOT/deploy/docker-compose.yml" logs; echo "FAIL: not healthy"; exit 1; }

docker inspect "$PROJECT-probe-1" --format 'user={{.Config.User}} readonly={{.HostConfig.ReadonlyRootfs}} network={{.HostConfig.NetworkMode}} capdrop={{.HostConfig.CapDrop}} nofile={{(index .HostConfig.Ulimits 0).Soft}} mem={{.HostConfig.Memory}} restart={{.HostConfig.RestartPolicy.Name}}'
echo "OK: container healthy under the compose file"
