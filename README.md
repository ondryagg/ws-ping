# ws-ping

The Ondrya latency probe: a small, internet-facing WebSocket service whose only job is to echo timestamped messages so a visitor's browser can measure round-trip time to a node. Written in Go, shipped as a static container.

The authoritative requirements are in [`spec.md`](spec.md). The reasoning behind the design is the failure it exists to prevent: an earlier service that went on returning "websocket timeout" until it was restarted, with no evidence left behind.

| Path | What |
|---|---|
| `probe/` | The Go module: service, tests, fuzz targets, Dockerfile, Makefile |
| `deploy/` | `docker-compose.yml`, Prometheus alert rules, Grafana dashboard |
| `docs/protocol.md` | Wire format and limits |
| `docs/runbook.md` | What to capture before restarting, and how to read it |
| `docs/latency-probe-setup.md` | DNS, TLS, firewall, VRF, scrubbing tunnel, add-a-node checklist |
| `docs/postmortem-template.md` | Pre-filled with the nine likely causes |
| `docs/verification.md` | What was verified, with measured numbers, and what could not be |

## Quick start

```bash
cd probe
make test          # vet, unit and integration tests
make race          # the same under the race detector, in a Go container
make image         # static, non-root, distroless image
make smoke         # starts deploy/docker-compose.yml and waits for the health check
make soak          # failure and soak tests against the real image
```

Deployment values (hostname, listen port, node ID, allowed origins, the certbot directory, and for docker the image digest) live in one file, `/etc/ondrya-probe/probe.env`, read by both the systemd unit (`deploy/ondrya-probe.service`) and `docker compose --env-file`. The certificate is the one Let's Encrypt already issued; renewals are picked up without a restart. See `deploy/probe.env.example` and `docs/latency-probe-setup.md`.
