# Protocol: `ondrya-latency.v1`

The probe does one thing: echo a timestamped message so a browser can measure round-trip time to this node. Nothing else is carried.

## Transport

- **WSS only**, on the configured hostname and port: `wss://{hostname}:{port}/ws`.
- Subprotocol `ondrya-latency.v1` must be offered. Plain HTTP and any other subprotocol are refused.
- HTTP/1.1 only. `permessage-deflate` is off.
- The `Origin` header must exactly match one of the configured allowed origins.

## Session

1. The client opens the WebSocket.
2. After the pre-upgrade checks pass, the server sends one text frame:

   ```json
   {"t":"hello","proto":1,"node":"dal-01","maxPings":30,"serverTimeMs":1700000000123}
   ```

3. The client sends `{"t":"ping","seq":<uint32>}`.
4. The server replies at once with `{"t":"pong","seq":<uint32>}`: same `seq`, nothing else added.
5. The client may send up to `maxPings` pings.

The server never initiates anything except `hello` and `pong`. There are no server-side periodic pings, no broadcast, and no shared state between connections.

## Frames

- Text frames only, at most **64 bytes**.
- `seq` is an unsigned 32-bit integer (0 to 4294967295). A negative, fractional, quoted or out-of-range `seq`, an unknown field, a missing field or trailing data is malformed.

## Closing

| Condition | Close status |
|---|---|
| `maxPings` pings answered | **1000** after the last pong |
| 20 s since the session began | **1000** |
| No frame from the client for 5 s | **1001** |
| Malformed message | **1008** |
| Binary frame, or text frame over 64 bytes | **1009** |
| Server shutting down | **1001** |

The close handshake is given 2 s, then the socket is closed hard.

## Refusals before the upgrade

Checked in this order, cheapest first. Nothing is allocated for a refused request.

| Order | Check | Status |
|---|---|---|
| 0 | Maintenance flag, kill-switch file, or degraded mode | 503 |
| 1 | Global capacity | 503 |
| 1 | Per-client-IP capacity | 429 |
| 1 | New-connection rate for that client IP | 429 |
| 2 | `Host` is not the configured hostname (port ignored, case-insensitive) | 421 |
| 3 | Method is not `GET` | 405 |
| 4 | `Origin` missing, duplicated, or not an exact match | 403 |
| 5 | `ondrya-latency.v1` not offered | 400 |

The TLS handshake itself is refused when the SNI is not the configured hostname.

Error bodies are the plain status text and nothing else.

## Other endpoints

| Path | Behaviour |
|---|---|
| `GET /ping` | `204`, `Cache-Control: no-store`. If the request carries an allowed `Origin`, `Access-Control-Allow-Origin` and `Timing-Allow-Origin` are set to it. An unknown `Origin` gets 403. Rate limited per client IP. |
| `GET /info` | `{"node":"...","region":"...","version":"...","proto":1}` and nothing else. Stays available in maintenance and degraded mode (only `/ws` and `/ping` answer 503). |
| `GET /healthz` | `200 ok` only if the internal self-test passed within the last 30 s and the node is not degraded; otherwise `503`. Never cached. Keeps answering in maintenance mode. |

`/metrics` and `/debug/pprof/` exist only on the loopback listener (`127.0.0.1:9109` by default), never on the public one.

## Limits (defaults)

| Limit | Default |
|---|---|
| Concurrent sessions, all clients | 2000 |
| Concurrent sessions per client IP (IPv6: per /64) | 4 |
| New sessions per client IP | 10 per minute |
| `/ping` and `/info` per client IP | burst 20, 5 per second sustained |
| Frame size | 64 bytes |
| Pings per session | 30 |
| Session lifetime | 20 s |

The client IP is the TCP peer address; IPv6 clients are limited per /64 (by design, so one subscriber cannot rotate addresses). A single actor with a larger allocation (/56, /48) therefore gets one quota per /64; the global caps are the bound on aggregate load. `X-Forwarded-For` is read only if the peer is inside `--trusted-proxies` (empty by default; `0.0.0.0/0`, `::/0` and loopback ranges are refused at startup).
