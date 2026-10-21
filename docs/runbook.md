# Runbook: the probe stops answering

The earlier service ran in Docker and eventually returned "websocket timeout" until the container was stopped and started again. Nobody captured the process state first, so the cause was never confirmed.

**If it happens again, do not restart first.** The probe now restarts itself when it detects it is broken, and leaves a diagnostic bundle when it does. If it has not done that, capture first (9.1), then restart.

Replace `$HOST` and `$PORT` below with the node's probe hostname and port. The container is called `probe`.

## 9.1 Capture first, restart second

Save everything to one file and attach it to the incident (`docs/postmortem-template.md`).

```bash
{
  date -u
  # Is the process alive and what does Docker think?
  docker ps --filter name=probe
  docker inspect probe --format '{{.State.Status}} oom={{.State.OOMKilled}} restarts={{.RestartCount}} health={{.State.Health.Status}}'
  docker logs --tail 300 probe

  # Inside the process: goroutines, descriptors, connections
  curl -s 127.0.0.1:9109/metrics | grep -E 'probe_(connections_active|goroutines|open_fds|fd_limit|accept_errors|selftest|panics|crashloop|write_timeouts|read_timeouts|rejected)'
  curl -s 'http://127.0.0.1:9109/debug/pprof/goroutine?debug=2' > goroutines.txt
  ls /proc/$(pidof probe)/fd | wc -l            # host PID namespace

  # Sockets and kernel limits
  ss -s
  ss -tan "sport = :$PORT" | awk '{print $1}' | sort | uniq -c
  conntrack -C 2>/dev/null; sysctl net.netfilter.nf_conntrack_max net.core.somaxconn
  dmesg -T | grep -E -i 'conntrack|table full|oom|dropping' | tail -50

  # From outside the node and from the node itself
  openssl s_client -connect $HOST:$PORT -servername $HOST </dev/null 2>&1 | head -20
  curl -sv https://$HOST:$PORT/healthz

  # Evidence the probe left for itself
  ls -l /var/log/ondrya-probe/
} > incident-$(date -u +%Y%m%dT%H%M%SZ).txt 2>&1
```

If the probe exited by itself (exit code 70), the bundle is already on disk: `/var/log/ondrya-probe/diag-<timestamp>.txt`, and also in `docker logs probe`. It contains the goroutine dump, descriptor count and limit, active connections, the last 50 accept and upgrade errors, and memory statistics. Read that before anything else.

## How to read it

| You see | It means | Cause in the table |
|---|---|---|
| `probe_connections_active` and `probe_goroutines` high and growing, descriptors near the limit | Leak or exhaustion | 1, 2 |
| `accept: too many open files` in the logs, `probe_accept_errors_total{error="emfile"}` rising | Descriptor exhaustion | 2 |
| `conntrack -C` near `nf_conntrack_max`, `dmesg` shows "table full" | Conntrack. Switch to host networking | 3 |
| Goroutines mostly in `Write` or `net.(*conn).Write` | Blocked writes | 4 |
| Process alive, listener present, `probe_selftest_ok 0`, goroutine count normal | Look at TLS, certificate expiry, VRF or firewall | 5, 9 |
| Timeouts at a fixed interval (30, 60, 100 s) | A proxy or scrubber idle timeout in the path | 6 |
| Container `Up` for days, `health=unhealthy` | Plain Docker never restarts unhealthy containers; the process should have exited by itself, so find out why it did not | 7 |
| `OOMKilled=true`, `probe_memory_bytes` climbing | Memory | 8 |
| Works from the host, fails from outside, or the reverse | Network namespace or VRF mismatch | 9 |

## What the probe does by itself

- **Self-test every 10 s.** A real WSS round trip to its own listener through the same accept, TLS, upgrade and echo path visitors use. Result in `probe_selftest_ok` and `/healthz`.
- **After 3 consecutive failures (about 30 s):** it writes the bundle to stderr and `/var/log/ondrya-probe/`, records the exit, and **exits with code 70**. `restart: unless-stopped` brings it back.
- **Crash-loop guard.** If the watchdog has restarted the process more than 5 times in 10 minutes, the next start stays up in degraded mode: `/ws`, `/ping` and `/healthz` answer 503 and `probe_crashloop` is 1. It leaves degraded mode by itself after 6 consecutive passing self-tests (about a minute). Read the bundles; the cause is not transient.
- **Out of descriptors.** The process reserves three descriptors at startup and closes its listener before dumping, so the bundle can still be written when the node has none left.

## Kill switch

To stop serving without stopping the container (for example during an attack or a scrubber change):

```bash
touch /run/probe/disabled      # /ws and /ping answer 503; /healthz keeps answering
rm /run/probe/disabled         # resume
```

The file is checked at most once a second. `--maintenance` does the same from configuration.

## Checking one node end to end

```bash
curl -sv https://$HOST:$PORT/healthz            # 200 ok
curl -s  https://$HOST:$PORT/info               # node, region, version, proto
docker exec probe /probe healthcheck; echo $?   # 0 when healthy; this is what the Docker HEALTHCHECK runs
websocat --origin https://ondrya.com --protocol ondrya-latency.v1 wss://$HOST:$PORT/ws
# then type: {"t":"ping","seq":1}
```

## 9.2 Add a node, upgrade, roll back

See `docs/latency-probe-setup.md` for the add-a-node checklist, upgrade and rollback.
