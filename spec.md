# Spec: Ondrya latency probe WebSocket service (Go, Docker)

Companion to `ondrya-latency-map-prompt.md`. That file describes the website and the measurement client. **This file is the authoritative spec for the server side (`probe/`)** and replaces the "Go probe service" section there wherever the two differ.

Hand this to Claude Code at the root of the `probe/` module. Items in `[[ double brackets ]]` are values the owner must fill in. Do not invent them.

---

## 0. The problem this spec exists to solve

A previous WebSocket service ran in Docker and eventually started returning **"websocket timeout"** errors. It never recovered until the container was stopped and started again. Nobody captured the state of the process before restarting it, so the cause was never confirmed.

**Treat this as a reliability requirement, not a feature.** The service must either keep working under abuse and failure, or detect that it is broken and recover by itself within seconds. It must also leave enough evidence behind that the root cause can be found.

### Likely causes of "fine until restart" (ranked, none confirmed)

Design against all of these. §3 to §8 map to them.

| # | Likely cause | What it looks like | Prevented or exposed by |
|---|---|---|---|
| 1 | **Leaked connections or goroutines.** No read or write deadline, so dead or half-open clients are never cleaned up. | Open connection count and goroutine count only go up. New connections time out once a limit is reached. | §4 deadlines, §5 lifecycle, §8 metrics, §10 leak tests |
| 2 | **File descriptor exhaustion.** Container `nofile` limit (often 1024) hit. | `accept: too many open files` in logs. Existing clients fine, new ones hang. | §6 limits, `ulimits` in compose, §8 FD metric |
| 3 | **Docker NAT / `docker-proxy` / conntrack table full.** Published ports go through NAT. Half-open and short-lived connections fill the conntrack table. | `nf_conntrack: table full, dropping packet` in `dmesg`. Timeouts only to the published port. | §7 host networking, §9 runbook |
| 4 | **Blocked write.** A slow or stalled client makes a write block forever, and a shared mutex or hub goroutine stalls everyone. | Latency climbs, then everything freezes. Goroutine dump shows many goroutines stuck in `Write`. | §5 no shared writer, §4 write deadline |
| 5 | **Accept loop dead or stuck.** A panic in a goroutine, or an unbounded handler, stops the service from accepting. | Process is "Up" but the port does not answer. | §5 panic recovery, §8 self-test watchdog |
| 6 | **Reverse proxy or DDoS panel idle timeouts.** A proxy in front closes WebSockets it considers idle and then holds broken state. | Timeouts at a fixed interval (30, 60, 100 s). | §7 no proxy in the path, short-lived sessions |
| 7 | **Docker health check missing.** Docker marks the container "Up" while the app is broken, and plain Docker does not restart unhealthy containers. | Container shows `Up` for days while not serving. | §8 self-exit watchdog, §7 compose |
| 8 | **Memory pressure / OOM kill loop.** Unbounded buffers. | RSS climbs. `docker inspect` shows `OOMKilled: true`. | §6 limits, `GOMEMLIMIT` |
| 9 | **Network namespace or VRF mismatch.** A listener in the wrong VRF or network namespace never sees traffic. | Works from the host, fails from outside, or the reverse. | §7, §9 |

**If this happens again, do not restart first.** Run the capture steps in §9. That is what identifies the cause.

---

## 1. Purpose and scope

A tiny, hostile-internet-facing service whose only job is to echo timestamped messages so a visitor's browser can measure round-trip latency to this node. It does nothing else.

In scope: WebSocket echo, HTTP fallback `/ping`, health, metrics, hardening, container packaging, self-monitoring.
Out of scope: authentication, user accounts, persistence, any game or panel logic, any data storage.

It must be safe to expose to the whole internet and boring enough to run unattended for months.

---

## 2. Stack

- Latest stable Go. `CGO_ENABLED=0`. Single static binary, target under 15 MB.
- Standard library plus **one** WebSocket library: `github.com/coder/websocket` (confirm it is current and maintained before adding; otherwise `gorilla/websocket`). Prometheus client for metrics. Anything else needs a one-line justification in the PR.
- `log/slog` JSON logging. Never log message contents.
- Config via flags and environment variables, validated at startup. Refuse to start on bad config.

---

## 3. Protocol (version 1)

Transport: **WSS only** on a configured hostname and port. Subprotocol `ondrya-latency.v1`. Reject plain HTTP and any other subprotocol.

1. Client opens `wss://{hostname}:{port}/ws`.
2. Server upgrades after the checks in §4.1, then sends one text frame:
   `{"t":"hello","proto":1,"node":"<id>","maxPings":30,"serverTimeMs":<unix ms>}`
3. Client sends `{"t":"ping","seq":<uint32>}`.
4. Server replies immediately with `{"t":"pong","seq":<uint32>}`. Same `seq`, nothing else added.
5. The client may send up to `maxPings` pings. The server closes with status **1000** after the last one is answered, after **20 s** total, or on the first malformed frame (status **1008**).
6. If the client stops sending for **5 s** the server closes with **1001**.

Rules:
- Messages are **text only**, at most **64 bytes**. Larger or binary frames close the connection with **1009**.
- The server **never initiates** anything except `hello` and `pong`. No server-side periodic pings, no broadcast, no shared state between connections.
- Sessions are **short-lived by design (20 s)**. This removes the whole class of long-lived idle-connection problems.
- `permessage-deflate` is **off**.
- Echo must be handled inline in the connection's own goroutine: read a frame, write the reply, repeat. No channels, no queues, no worker pool on this path.

---

## 4. Request handling and timeouts

### 4.1 Checks before the upgrade (cheap checks first, fail fast, no allocation-heavy work)

In this order:
1. **Capacity:** if active connections are at the global or per-IP cap, return **503** (global) or **429** (per-IP) *before* upgrading. Do not accept work you cannot finish.
2. **Host:** the `Host` header must equal the configured hostname (case-insensitive, port stripped). Otherwise **421**. Also verify the TLS SNI matches via `tls.Config.GetConfigForClient`.
3. **Method and path:** `GET /ws` only.
4. **Origin:** the `Origin` header must exactly match one of the configured allowed origins. Missing or unknown means **403**. WebSockets are not protected by CORS, so this check is the protection.
5. **Subprotocol:** must offer `ondrya-latency.v1`.

### 4.2 Timeouts (every layer, no exceptions)

`http.Server` timeouts do **not** apply after a connection is hijacked into a WebSocket, so the service must apply deadlines itself.

| Where | Value | Behaviour on expiry |
|---|---|---|
| TLS handshake | 5 s | Drop connection |
| `ReadHeaderTimeout` | 5 s | Drop connection |
| `IdleTimeout` (plain HTTP endpoints) | 10 s | Close |
| WebSocket **read** | 5 s per frame, via `context.WithTimeout` | Close 1001 |
| WebSocket **write** | 2 s per frame | Close, count `write_timeout` |
| Close handshake | 2 s, then hard close | Hard close |
| Total session lifetime | 20 s, via a context that cancels everything | Close 1000 |
| Graceful shutdown | 10 s | Force close remaining |

A connection that cannot make progress is closed. **No code path may block without a deadline.** Add a lint rule or test that greps for unbounded `Read`/`Write` calls on connections.

---

## 5. Connection lifecycle and concurrency model

- **One goroutine per connection. That goroutine owns the connection** and is the only one that reads or writes it. No separate writer goroutine, no write channel, no shared hub.
- Every handler runs under `defer` that: recovers from panics (log the stack once, increment a counter, close the connection), closes the connection, decrements counters, and releases the per-IP slot. A panic in one session must never affect another.
- Active-connection accounting uses an atomic counter plus a per-IP map guarded by a mutex held only for the increment or decrement. **No lock is ever held across I/O.**
- Per-IP state is evicted when its count reaches zero. The map cannot grow without bound.
- Use a bounded semaphore for the global cap. Acquire before upgrade, release in `defer`.
- Shutdown: on `SIGTERM`, stop accepting, wait up to 10 s for sessions to finish, then force close. Exit code 0.
- No global mutable state. Inject the clock and the metrics registry for testing.
- Every goroutine has a documented owner and exit path.

---

## 6. Limits and abuse controls (all configurable, safe defaults)

| Limit | Default |
|---|---|
| Global concurrent connections | 2000 |
| Concurrent connections per client IP | 4 |
| New connections per client IP | 10 per minute (token bucket) |
| `/ping` requests per client IP | 20 per second burst, 5 sustained |
| Frame size | 64 bytes |
| Pings per session | 30 |
| Session lifetime | 20 s |
| Go memory limit | set `GOMEMLIMIT` to about 80% of the container memory limit |

- Client IP is the **TCP peer address**. Do not trust `X-Forwarded-For` unless an explicit `--trusted-proxies` list is set (default empty). With host networking (§7) the peer address is the real client address.
- IPv6: rate-limit per **/64**, not per address.
- Rejected requests are counted by reason, not logged individually. Log at most one line per reason per 10 s.
- Never reveal internal details in error bodies. Plain short status text only.
- Add a **kill switch**: if `--maintenance` is set or the file `[[ /run/probe/disabled ]]` exists, answer **503** to `/ws` and `/ping` but keep `/healthz` answering.

---

## 7. Networking and Docker (this is where the earlier failure most likely lived)

### 7.1 Network mode

**Use `network_mode: host`** and have the Go binary terminate TLS itself. Reasons:
- Docker's published ports go through NAT, `docker-proxy` and conntrack. Under connection churn the conntrack table can fill, and all new connections to the published port then time out while the process looks healthy. Host networking removes that failure mode.
- NAT and `docker-proxy` add latency and can mask the real client IP, which breaks per-IP limits. Both matter for a latency tool.
- No reverse proxy (nginx, Traefik, Cloudflare proxy) sits in the path. Proxies bring idle timeouts, header rewrites and a second place to fail.

If host networking is not possible, document why, and then also document: `userland-proxy: false` in `daemon.json`, a raised `nf_conntrack_max`, and a conntrack monitoring alert (§9).

**Node-specific checks** `[[ owner to confirm per node ]]`:
- If the node uses **VRFs** (an earlier node, gn12-nyc, needed a VRF fix), verify the listener is reachable in the right VRF (`tcp_l3mdev_accept`, or run the container in that VRF). A listener in the wrong VRF looks exactly like a timeout.
- If a **DDoS scrubbing tunnel** fronts the node, confirm the probe hostname and port take the same path as game traffic, and that the scrubber does not apply a short WebSocket or idle timeout to this port. Otherwise latency numbers will not match what players see.
- The probe hostname is **DNS-only (grey cloud) in Cloudflare**, never proxied.
- If the host runs a default-drop firewall with `DOCKER-USER` rules, open only the probe port for TCP. Host networking bypasses Docker's iptables insertion, so the **host firewall must explicitly allow the port**.

### 7.2 Compose file (deliver as `deploy/docker-compose.yml`)

```yaml
services:
  probe:
    image: ghcr.io/[[ org ]]/ondrya-probe:[[ pinned version, never latest ]]
    network_mode: host
    restart: unless-stopped
    read_only: true
    user: "65532:65532"
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    ulimits:
      nofile: { soft: 65536, hard: 65536 }
    mem_limit: 256m
    environment:
      GOMEMLIMIT: 200MiB
      PROBE_HOSTNAME: "[[ lg-dal.example.com ]]"
      PROBE_LISTEN: ":[[ 8443 ]]"
      PROBE_ALLOWED_ORIGINS: "https://ondrya.com"
      PROBE_NODE_ID: "[[ dal-01 ]]"
    volumes:
      - /etc/ondrya-probe/tls:/tls:ro
    healthcheck:
      test: ["CMD", "/probe", "healthcheck"]
      interval: 15s
      timeout: 5s
      retries: 3
      start_period: 10s
    logging:
      driver: json-file
      options: { max-size: "10m", max-file: "3" }
```

Notes the implementation must respect:
- Binding a port above 1024 means no `NET_BIND_SERVICE` capability is needed. Keep `cap_drop: [ALL]`.
- Image: `FROM scratch` or distroless static, with CA certificates, non-root user, no shell.
- **Plain Docker does not restart a container just because it is `unhealthy`.** `restart: unless-stopped` only reacts to the process exiting. That is why §8.3 makes the process **exit itself** when it detects it is broken, and Docker then restarts it. Do not rely on the health check alone.
- Pin the image by digest in production.

### 7.3 TLS

- Option A (simplest): certificate and key files mounted read-only, reloaded on file change without restart.
- Option B: built-in ACME using `certmagic` with **DNS-01 through the Cloudflare DNS API** (scoped token from an env var or secret file). DNS-01 is required because the service is not on 443 and HTTP-01 and TLS-ALPN-01 would not work.
- TLS 1.2 minimum, 1.3 preferred, modern cipher defaults. HTTP/1.1 only on the WebSocket path.
- `[[ owner: choose A or B ]]`

---

## 8. Observability and self-healing

### 8.1 Endpoints

- `GET /healthz`: `200 ok` only if the **internal self-test** (§8.3) passed within the last 30 s. Otherwise `503`. Never cached.
- `GET /ping`: `204`, `Cache-Control: no-store`, CORS and `Timing-Allow-Origin` set to the matched allowed origin. HTTP fallback for the browser.
- `GET /info`: `{"node":"...","region":"...","version":"...","proto":1}`. Nothing else.
- **Loopback only (separate listener on `127.0.0.1:[[ 9109 ]]`):** `/metrics` (Prometheus) and `/debug/pprof/`. Never on the public listener.

### 8.2 Metrics (Prometheus)

`probe_connections_active`, `probe_connections_total{result}`, `probe_rejected_total{reason}` (capacity, per_ip, host, origin, subprotocol, rate_limit, maintenance), `probe_session_duration_seconds` (histogram), `probe_pings_total`, `probe_read_timeouts_total`, `probe_write_timeouts_total`, `probe_panics_total`, `probe_accept_errors_total{error}`, `probe_goroutines`, `probe_open_fds`, `probe_fd_limit`, `probe_memory_bytes`, `probe_selftest_ok` (gauge), `probe_selftest_failures_consecutive`, `probe_cert_expiry_seconds`, `probe_build_info{version,commit}`. Ship an example Grafana dashboard and alert rules: FD use over 70%, goroutines over 3x baseline, self-test failing, certificate expiring in under 14 days, accept errors rising.

### 8.3 Self-test watchdog (the key recovery mechanism)

Every **10 s** the service runs a real round trip against **its own public listener**: open a WSS connection to `127.0.0.1:[[ port ]]` using the configured hostname as TLS server name and `Host`, send one ping, expect the matching pong within 2 s, close. This exercises the same accept, TLS, upgrade and echo path that visitors use.

- Result feeds `probe_selftest_ok` and `/healthz`.
- After **3 consecutive failures** (about 30 s): write a **diagnostic bundle** to stderr and to `[[ /var/log/ondrya-probe/ ]]` (or the container log): goroutine dump (`pprof.Lookup("goroutine").WriteTo(w, 2)`), open FD count and limit, active connection count, last 50 accept and upgrade errors, memory stats. Then **exit with code 70**. Docker restarts the container.
- Self-test connections do not count toward per-IP or rate limits. They use the loopback address and a reserved internal marker that is **not** accepted from the network.
- A bounded cooldown prevents restart storms: if the process has restarted more than 5 times in 10 minutes, it stays up in a degraded `503` mode instead of looping, and raises `probe_crashloop` for the alert.

### 8.4 Logging

JSON, structured, at startup (config summary with secrets redacted), at config errors, at watchdog events, and rate-limited reject summaries. No per-connection logging by default. `--log-level debug` may log connection open and close with the IP, off by default.

---

## 9. Runbook (deliver as `docs/runbook.md`)

### 9.1 If the probe stops answering: capture first, restart second

Run these **before** restarting. Save the output to a file and attach it to the incident.

```bash
# Is the process alive and what does Docker think?
docker ps --filter name=probe
docker inspect probe --format '{{.State.Status}} oom={{.State.OOMKilled}} restarts={{.RestartCount}} health={{.State.Health.Status}}'
docker logs --tail 300 probe

# Inside the process: goroutines, FDs, connections
curl -s 127.0.0.1:9109/metrics | egrep 'probe_(connections_active|goroutines|open_fds|fd_limit|accept_errors|selftest|panics)'
curl -s 'http://127.0.0.1:9109/debug/pprof/goroutine?debug=2' > goroutines.txt
ls /proc/$(pidof probe)/fd | wc -l          # on the host, if host network/PID visible

# Sockets and kernel limits
ss -s
ss -tan 'sport = :[[ 8443 ]]' | awk '{print $1}' | sort | uniq -c
conntrack -C 2>/dev/null; sysctl net.netfilter.nf_conntrack_max net.core.somaxconn
dmesg -T | egrep -i 'conntrack|table full|oom|dropping' | tail -50

# From outside the node and from the node itself
openssl s_client -connect [[ host ]]:[[ 8443 ]] -servername [[ host ]] </dev/null 2>&1 | head -20
curl -sv https://[[ host ]]:[[ 8443 ]]/healthz
```

How to read it:
- `connections_active` and `goroutines` high and growing, FDs near the limit: **leak or exhaustion** (causes 1, 2).
- `conntrack -C` near `nf_conntrack_max`, `dmesg` shows "table full": **conntrack** (cause 3). Switch to host networking.
- Goroutines mostly in `Write` or `net.(*conn).Write`: **blocked writes** (cause 4).
- Process alive, listener present, `selftest` failing, goroutine count normal: look at TLS, certificate expiry, VRF or firewall (causes 5, 9).
- `OOMKilled=true`: **memory** (cause 8).

### 9.2 Add a node, upgrade, rollback
Checklist for DNS record, certificate, firewall rule, `docker compose up -d`, the `healthcheck` subcommand, an external smoke test with `websocat` or the site's test page, and entry in `latency-nodes.json`. Upgrade by pulling the pinned digest and `docker compose up -d`. Rollback by redeploying the previous digest.

---

## 10. Testing (all of it must run, in CI, with `-race`)

**Unit and table tests:** config validation; Host and SNI mismatch gives 421; missing or wrong Origin gives 403; wrong subprotocol rejected; echo returns identical `seq` in order; binary and oversized frames close 1009; malformed JSON closes 1008; limits enforced (global, per-IP, per-/64, rate); lifetime and `maxPings` close normally; maintenance mode; panic in a handler does not affect other sessions; per-IP map shrinks to zero.

**Fuzz targets:** the message parser and the Origin and Host comparers.

**Failure and soak tests (the ones that would have caught the old bug).** Run these against the real binary in a container:

1. **Idle clients:** open 1000 WebSockets and send nothing. All must be closed by the server within 6 s. Goroutines and FDs return to baseline.
2. **Half-open clients:** connect, then freeze the client process (`SIGSTOP`) or drop its packets (`iptables`/`tc`). The server must free those connections within the deadlines.
3. **Clients that never read:** send pings without reading pongs until buffers fill. The write deadline must fire at 2 s and the connection must close. Other sessions must stay fast.
4. **Connection flood:** 5000 connection attempts from a few IPs. Rejections must happen before the upgrade, memory must stay bounded, and **after the flood stops the service must serve normal requests within 10 s without a restart**.
5. **Churn soak:** 50 connections per second for 30 minutes, each doing a full session. FDs, goroutines and memory must stay flat (within 10% of the first five minutes).
6. **Slowloris:** open TLS and send headers one byte at a time. Must be dropped by `ReadHeaderTimeout`.
7. **FD exhaustion drill:** run with a very low `nofile` (for example 64) and flood it. The service must reject cleanly, log `accept` errors once per interval, fail the self-test if it cannot recover, and **exit by itself** so Docker restarts it.
8. **Watchdog test:** inject a hang into the accept path (test build tag). `/healthz` must go 503 within 30 s, a diagnostic bundle must appear, and the process must exit 70.
9. **Crashloop guard:** force repeated failures and confirm the degraded mode after 5 restarts in 10 minutes.
10. **Graceful shutdown:** `SIGTERM` with active sessions. Sessions finish or are cut at 10 s. Exit code 0.
11. **Test-only delay flag:** a build tag `fixture` adds `--test-delay`. Prove the production binary rejects that flag.

**Acceptance criteria**
- After every abuse test above, the service returns to baseline (goroutines, FDs, memory) within 30 s **with no restart**, except tests 7 to 9, which must self-restart.
- No unbounded blocking call exists on any connection path.
- `go vet`, `golangci-lint`, `govulncheck` and `go test -race ./...` are clean.
- Image builds reproducibly, runs as non-root with a read-only filesystem, and the container starts and passes its own health check with the compose file in §7.2.

---

## 11. Deliverables

- `probe/` Go module with tests and fuzz targets.
- `Dockerfile` (multi-stage, static, non-root) and `deploy/docker-compose.yml`.
- `deploy/prometheus-alerts.yml` and an example Grafana dashboard JSON.
- `docs/runbook.md` (§9), `docs/protocol.md` (§3) and `docs/latency-probe-setup.md` (DNS, TLS, firewall, VRF, scrubbing-tunnel notes, add-a-node checklist).
- A short `docs/postmortem-template.md` pre-filled with the §0 cause table so the next incident gets written down.
- PR description listing what was verified in the soak tests (with the measured numbers) and what could not be verified without real nodes.

## 12. Questions for the owner (ask before coding if unclear)

1. Can the nodes use `network_mode: host`? Any VRF on these nodes?
2. Which port, and does the DDoS scrubber on each node allow it with no short idle timeout?
3. Certificates: mounted files or built-in DNS-01 with a Cloudflare token?
4. Where should metrics go? (Existing Prometheus and Grafana, or none for now.)
5. Does the old service's code or compose file still exist? If so, share it and its last error logs. It would confirm which cause in §0 was real.