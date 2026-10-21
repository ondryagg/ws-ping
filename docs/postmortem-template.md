# Postmortem: probe stopped answering on `[[ node ]]`

| | |
|---|---|
| Date and time (UTC) | `[[ ]]` |
| Detected by | alert / visitor report / watchdog exit |
| Duration of impact | `[[ ]]` |
| Recovered by | self-restart (exit 70) / manual restart / other |
| Was state captured before the restart? | yes / no. Attach `incident-*.txt` and any `/var/log/ondrya-probe/diag-*.txt` |
| Image digest | `[[ ]]` |

## Timeline

| Time (UTC) | Event |
|---|---|
| | |

## Which cause was it?

Mark one, and write down the evidence that confirms or rules it out. "Nothing confirmed" is a valid answer, and so is a cause that is not in the table.

| # | Likely cause | What it looks like | Prevented or exposed by | Confirmed? | Evidence |
|---|---|---|---|---|---|
| 1 | **Leaked connections or goroutines.** No read or write deadline, so dead or half-open clients are never cleaned up. | Open connection count and goroutine count only go up. New connections time out once a limit is reached. | Deadlines, session lifecycle, metrics, leak tests | | |
| 2 | **File descriptor exhaustion.** Container `nofile` limit hit. | `accept: too many open files` in logs. Existing clients fine, new ones hang. | Limits, `ulimits` in compose, FD metric | | |
| 3 | **Docker NAT / `docker-proxy` / conntrack table full.** | `nf_conntrack: table full, dropping packet` in `dmesg`. Timeouts only to the published port. | Host networking, runbook | | |
| 4 | **Blocked write.** A stalled client blocks a write, and a shared mutex or hub stalls everyone. | Latency climbs, then everything freezes. Goroutine dump shows many goroutines stuck in `Write`. | No shared writer, write deadline | | |
| 5 | **Accept loop dead or stuck.** A panic or unbounded handler stops accepting. | Process is "Up" but the port does not answer. | Panic recovery, self-test watchdog | | |
| 6 | **Reverse proxy or DDoS panel idle timeouts.** | Timeouts at a fixed interval (30, 60, 100 s). | No proxy in the path, short-lived sessions | | |
| 7 | **Docker health check missing.** Docker marks the container "Up" while the app is broken. | Container shows `Up` for days while not serving. | Self-exit watchdog, compose health check | | |
| 8 | **Memory pressure / OOM kill loop.** | RSS climbs. `docker inspect` shows `OOMKilled: true`. | Limits, `GOMEMLIMIT` | | |
| 9 | **Network namespace or VRF mismatch.** | Works from the host, fails from outside, or the reverse. | Host networking, runbook | | |

## Root cause

## What worked

## What did not

## Follow-ups

| Action | Owner | Due |
|---|---|---|
| | | |
