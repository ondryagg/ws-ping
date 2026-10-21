# Verification: what was checked, with measured numbers

This is the PR description the spec asks for (section 11): what was verified in the soak tests, with the measured numbers, and what could not be verified without real nodes.

All soak figures come from `probe/test/soak` run against the **built container image** (`ondrya-probe:dev`, distroless, uid 65532, read-only rootfs, `cap_drop: ALL`, 256 MB, `nofile` 65536, host networking) on one Linux host with 16 cores, clients on loopback. Failure-injection tests use the `fixture` build of the same code (`ondrya-probe:fixture`); the production binary rejects every `--test-*` flag (exit 2, tested).

## Build and static checks

| Check | Result |
|---|---|
| `go vet` (production, `-tags fixture`, `-tags soak`) | clean |
| `golangci-lint` (errcheck, govet, staticcheck, gosec, bodyclose, noctx, errorlint, nilerr, misspell, ...) | 0 issues |
| `govulncheck` (production and fixture tags) | no vulnerabilities |
| `go test ./...` | 19 packages, all pass |
| `go test -race ./...` (in a Go container; the host has no C compiler) | clean |
| Fuzz targets (`FuzzParsePing`, `FuzzOriginAllowed`, `FuzzHostMatches`) | about 4.6M executions of `FuzzHostMatches` in 10 s with no failure; the fuzz-style table test found a real gap earlier (a Host such as `name:80@x` was accepted), now fixed |
| Binary size | 10.2 MiB (target: under 15 MB) |
| Image size | 20.7 MB |
| Build reproducibility | two local builds of the binary are byte-identical (`-trimpath -buildid=`) |
| Source-level guard tests (`internal/lint`) | every WebSocket read/write goes through `readFrame`/`writeFrame` with a net deadline and a context deadline; close is bounded; no `http.Get`, `http.ListenAndServe`, `http.DefaultClient` or `http.Server` without `ReadHeaderTimeout`; `context.Background()` only at process roots |

## Failure and soak tests (spec section 10)

Run on the final code. "Baseline" means goroutines and open descriptors measured from `/metrics` before the test.

| # | Test | Spec requirement | Measured |
|---|---|---|---|
| 1 | 1000 idle clients | all closed by the server within 6 s; goroutines and FDs return to baseline | slowest close **5.02 s**; goroutines 14 -> 14, FDs 12 -> 12 |
| 2 | 200 half-open clients (client process frozen with SIGSTOP) | freed within the deadlines | freed after **7.18 s** (5 s idle read + 2 s close handshake); back to baseline |
| 3 | client sends pings and never reads | write deadline fires at 2 s; other sessions stay fast | write deadline fired after **2.63 s** (includes ramp-up until buffers fill); a healthy session's p99 latency during it was **0.30 ms** (63 samples) |
| 4 | 5000 connection attempts from 5 IPs | rejected before upgrade; memory bounded; normal service within 10 s without restart | **4980 rejected with 429**, 20 upgrades, 0 other errors, in 1.2 s; memory +22.9 MiB; normal service again after **2.5 ms**; same process (start-time metric unchanged); baseline after |
| 5 | churn soak, 50 sessions/s, full sessions | FDs, goroutines, memory flat within 10% | 20 s run on the final image: 1000/1000 completed. **30 min run (90,000 sessions, 0 failures)**: FDs 12.3 -> 12.2, goroutines 14.3 -> 14.3, memory 22.8 -> 23.3 MiB; pong RTT p50 0.076 ms, p99 0.177 ms. See the note below on which build ran the 30 minutes |
| 6 | slowloris (20 + 20 connections) | dropped by the header/handshake deadlines | TCP-only worst **5.00 s**, TLS-then-one-byte-per-second worst **5.00 s** |
| 7 | FD exhaustion drill (`nofile` 64, 200 flooding clients) | reject cleanly, log accept errors once per interval, exit by itself | exit code **70**, 3 `accept:emfile` log lines in ~30 s (one per 10 s window), diagnostic bundle written to the state dir. The first run of this drill found that the bundle could not be written without a free descriptor; the process now reserves three and closes its listener first |
| 8 | watchdog with a hung upgrade path | `/healthz` 503 within 30 s, bundle, exit 70 | `/healthz` unhealthy at **29.99 s**, exit **70** at 33.1 s; the bundle's goroutine dump shows the stuck `handleWS` goroutines |
| 8b | watchdog with a hung accept loop | exit 70 | exit **70** at 33.1 s with a bundle |
| 9 | crash-loop guard | degraded 503 after 5 restarts in 10 minutes | six consecutive exits of 70, then the seventh start stayed up for 20 s answering 503 on `/healthz`, `/ping` and `/ws`, with `probe_crashloop` 1 |
| 10 | SIGTERM with 20 active sessions | sessions cut at 10 s, exit 0 | exit code **0** after **10.1 s**; all 20 sessions ended |
| 11 | test-only flag isolation | production binary rejects `--test-delay` | exit **2**, "flag provided but not defined"; the fixture build applies the delay |

Also verified: `deploy/docker-compose.yml` starts the container and its own `HEALTHCHECK` (`/probe healthcheck`) reports `healthy` (`make smoke`), running as 65532:65532, read-only, `cap_drop: ALL`, host network, `nofile` 65536, 256 MB.

**Note on the 30-minute run.** The first 30-minute run (above) used the image built before the post-audit remediation (which changed the socket-close path and the self-test locality check). A second 30-minute run on the final image then gave: 90,000 sessions, 0 failures, FDs 12.3 -> 12.3, goroutines 14.2 -> 14.3, memory 23.4 -> 23.4 MiB, pong RTT p50 0.078 ms, p99 0.190 ms.

## Security review

The Cloudflare `security-audit` skill was run in full mode against the code before the remediation above. Results and the full ledger are in `~/security-audit-skill/ws-ping/run-1/` (`REPORT.md`). Summary: no confirmed vulnerability; one configuration-dependent availability weakness (self-test with a specific-IP `--listen`), reproduced and fixed with a regression test; one claim rejected as documented design (IPv6 limiting is per /64 as the spec asks); several hardening items fixed. The audit was source-only: no agent executed code.

## What could not be verified without real nodes

- **VRF reachability** (`tcp_l3mdev_accept`, container in the right VRF): needs a node that uses VRFs. The self-test only proves the loopback path and cannot see this; use an external check.
- **DDoS scrubbing tunnel behaviour**: whether it applies a short WebSocket or idle timeout to the probe port, and whether probe traffic takes the same path as game traffic.
- **Conntrack and NAT behaviour under churn** (cause 3 in the postmortem table): the soak tests ran on loopback with host networking, which does not exercise conntrack. Real traffic through a real NIC and firewall is untested.
- **Public IPv6 reachability and per-subscriber allocation sizes**: limits are per /64; whether a single actor can hold many /64s depends on who can reach the node.
- **Real browsers and real latency**: the client side (website and measurement code) is out of scope for this repository; only the protocol was tested, with a Go client.
- **TLS options**: only option A (mounted certificate files with reload) is built. Option B (built-in ACME with a Cloudflare DNS-01 token) was deliberately not built.
- **Grafana dashboard and Prometheus rules** were validated syntactically (`promtool check rules`, JSON parse), not against a live Prometheus or Grafana.
- **The old service's logs and compose file** were not available, so which cause in the postmortem table was the real one remains unconfirmed.
- **GitHub Actions**: the workflow has not run; it is written and its actions are pinned by commit SHA, but only the commands it calls were run locally.
- **Race detector on the host**: needs a C compiler. It ran in a Go container instead.
