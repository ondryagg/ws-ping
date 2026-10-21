# Setting up a latency probe node

The probe is a small Go service that terminates TLS itself and echoes timestamped WebSocket messages. See `docs/protocol.md` for the wire format and `docs/runbook.md` for incidents.

## Decisions to confirm per node

These are owner decisions. They live in one file, `/etc/ondrya-probe/probe.env` (template: `deploy/probe.env.example`), which both the systemd unit and `docker compose` read, so the binary and the container behave the same. Both refuse to start without the required values; nothing in the repository guesses them.

| Decision | Where it goes | Notes |
|---|---|---|
| Probe hostname | `PROBE_HOSTNAME` | One DNS name per node, for example `lg-dal.example.com` |
| Port | `PROBE_LISTEN` (for example `:8443`) | Above 1024, so no capability is needed. Must be reachable with no short idle timeout on the path (see scrubbing tunnel) |
| Node ID | `PROBE_NODE_ID` | Sent in `hello` and `/info`. Letters, digits, `.`, `_`, `-` |
| Allowed origin(s) | `PROBE_ALLOWED_ORIGINS` | Exact, comma separated, for example `https://ondrya.com` |
| Image | `PROBE_IMAGE` | Pinned by digest in production, never a moving tag |
| TLS source | `PROBE_TLS_LETSENCRYPT_DIR` (default `/etc/letsencrypt`), optional `PROBE_TLS_CERT_NAME` | The certificate certbot already issued; see TLS below. Built-in ACME is **not implemented** |

## 1. DNS

- Create an `A` (and `AAAA` if the node has IPv6) record for the probe hostname pointing at the node.
- In Cloudflare the record must be **DNS only (grey cloud)**. A proxied record puts Cloudflare's WebSocket and idle handling between the visitor and the node and measures the wrong path.

## 2. TLS

The probe serves the certificate Let's Encrypt (certbot) already issued. You give it the hostname and the certbot directory; it works out the rest:

```
PROBE_HOSTNAME=lg-dal.example.com
PROBE_TLS_LETSENCRYPT_DIR=/etc/letsencrypt
# PROBE_TLS_CERT_NAME=lg-dal.example.com-0001   only if `certbot certificates` shows a different name
```

That serves `/etc/letsencrypt/live/<hostname>/fullchain.pem` and `privkey.pem`. It **follows renewals by itself**: certbot repoints the `live/` symlinks, the probe notices within 10 s and starts using the new certificate, and open sessions are not disturbed. No restart and no renewal hook are needed. A failed reload (for example a half-written renewal) keeps the previous certificate in service and increments `probe_cert_reload_failures_total`.

- The certificate must cover the probe hostname. The service refuses to start otherwise.
- Minimum TLS 1.2; TLS 1.3 is negotiated when the client supports it. HTTP/1.1 only.
- Issue it however suits the node. With Cloudflare DNS-01, `certbot` with the `dns-cloudflare` plugin works for a service that is not on port 443.
- Not using certbot? Set `PROBE_TLS_CERT` and `PROBE_TLS_KEY` to a chain and key file instead. Setting both a certbot directory and explicit files is refused.
- Alert: `ProbeCertificateExpiring` fires under 14 days.

**Letting the probe read the key.** certbot keeps `/etc/letsencrypt` root-only, and the probe does not run as root. Read `deploy/grant-cert-access.sh`, then run it once as root:

```bash
sh deploy/grant-cert-access.sh          # or pass another certbot directory
```

It creates the group `ondrya-cert`, makes the certbot tree readable by that group (the private key stays mode 0640, never world-readable), and installs a certbot deploy hook so renewed or new certificates keep that access. The systemd unit joins that group by name. For docker, put the printed numeric gid in `PROBE_CERT_GID`; compose adds the container to that group. Docker mounts the whole certbot directory read-only at the same path, because `live/` is symlinks into `archive/`.

Option B in the spec (built-in ACME with `certmagic` and a Cloudflare DNS-01 token) was not built: it adds a large dependency tree and a long-lived API credential inside an internet-facing container. If you want it, it is a separate change.

## 3. Firewall

Host networking bypasses Docker's iptables rules, so the **host firewall must explicitly allow the port**.

- Open only the probe port, TCP. Nothing else needs to be reachable: metrics and pprof listen on `127.0.0.1:9109` only.
- With a default-drop host firewall and `DOCKER-USER` rules, host-network containers are not affected by `DOCKER-USER`; use the host's own rules.

## 4. Host preparation

```bash
install -d -o 65532 -g 65532 -m 0750 /var/log/ondrya-probe   # bundles and crash-loop state
install -d -m 0755 /run/probe                                   # kill switch directory (read-only mount)
```

`/run` is a tmpfs on most hosts, so create `/run/probe` at boot (a `tmpfiles.d` entry) or point `PROBE_RUN_DIR` elsewhere.

## 5. Deploy

Write the configuration once, for either way of running:

```bash
install -d -m 0755 /etc/ondrya-probe
install -m 0640 deploy/probe.env.example /etc/ondrya-probe/probe.env   # then fill in every value
```

**Docker**

```bash
cd deploy
docker compose --env-file /etc/ondrya-probe/probe.env up -d
docker compose --env-file /etc/ondrya-probe/probe.env ps    # wait for "healthy"
```

`docker compose` fails loudly if a required value is missing. The container is `network_mode: host`, `read_only`, `cap_drop: [ALL]`, runs as uid 65532, with `nofile` 65536, 256 MB memory and `GOMEMLIMIT=200MiB`.

**systemd, straight from the binary**

```bash
install -m 0755 probe /usr/local/bin/ondrya-probe          # from `make build`
useradd --system --no-create-home --shell /usr/sbin/nologin --groups ondrya-cert ondrya-probe
install -m 0644 deploy/ondrya-probe.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now ondrya-probe
journalctl -u ondrya-probe -f
```

The unit runs as an unprivileged user with no capabilities, a read-only filesystem except `/var/log/ondrya-probe`, the same limits as the container, and `Restart=always` so exit 70 (self-test failed) brings it back.

## VRF nodes `[[ owner to confirm per node ]]`

If the node uses VRFs (the earlier gn12-nyc node needed a VRF fix), a listener in the wrong VRF looks exactly like a timeout: it works from the host and not from outside, or the reverse.

- Either run the container in the VRF where visitor traffic arrives, or set `net.ipv4.tcp_l3mdev_accept=1` so a listener in the default VRF accepts connections arriving through a VRF.
- Check from outside the node and from the node itself (`docs/runbook.md` 9.1). The self-test only proves the loopback path, so it cannot see a VRF problem. An external check can.

## Scrubbing tunnel `[[ owner to confirm per node ]]`

If a DDoS scrubbing tunnel fronts the node:

- Confirm the probe hostname and port take **the same path as game traffic**. Otherwise the latency shown will not match what players see.
- Confirm the scrubber does not apply a short WebSocket or idle timeout to this port. Sessions last 20 s at most, so a 30 s timeout is harmless, but a 10 s one is not.
- Do not put a reverse proxy (nginx, Traefik, Cloudflare proxy) in the path.

## If host networking is not possible

Document why, then do all of this:

- `"userland-proxy": false` in `/etc/docker/daemon.json`.
- Raise `net.netfilter.nf_conntrack_max`, and alert on `nf_conntrack_count / nf_conntrack_max`.
- Per-IP limits only work if the real client address reaches the container. Behind NAT or `docker-proxy` they see the proxy's address, so either set `--trusted-proxies` correctly or accept that one source address is shared by everyone.

## Add-a-node checklist

- [ ] DNS record created, **grey cloud**
- [ ] certbot certificate issued for the hostname; `deploy/grant-cert-access.sh` run once
- [ ] Host firewall allows the probe port (TCP)
- [ ] VRF / namespace checked
- [ ] Scrubbing path and idle timeout checked
- [ ] `/var/log/ondrya-probe` created, owned by 65532
- [ ] `/etc/ondrya-probe/probe.env` filled in (image pinned by digest if using docker)
- [ ] `docker compose up -d`, container reports `healthy`
- [ ] `curl https://<host>:<port>/healthz` returns `ok` **from outside the node**
- [ ] External smoke test: `websocat` or the site's test page completes a session
- [ ] Prometheus scrapes `127.0.0.1:9109` (through your usual path), alert rules loaded, dashboard imported
- [ ] Entry added to `latency-nodes.json`

## Monitoring

- Scrape the metrics port with Prometheus from the node itself or over a private network. It is bound to loopback on purpose.
- Load `deploy/prometheus-alerts.yml` and import `deploy/grafana-dashboard.json`.

## Upgrade and roll back

```bash
# upgrade: set PROBE_IMAGE to the new digest in probe.env, then
export E=--env-file=/etc/ondrya-probe/probe.env
docker compose $E pull && docker compose $E up -d
docker compose $E ps     # healthy?

# roll back: put the previous digest back in probe.env, then
docker compose $E up -d
```

Sessions last 20 s, so a restart costs visitors at most one retried measurement. Shutdown is graceful: `SIGTERM` stops accepting, waits up to 10 s, then closes what is left and exits 0.

Both base images in `probe/Dockerfile` are pinned by digest. Refresh them on purpose, rebuild, and run the soak tests (`make soak`) before shipping.

## Configuration reference

Every setting is a flag or a `PROBE_*` environment variable (flags win). `docker run --rm <image> --help` lists them all. The service validates everything at startup and refuses to start on a bad value.

## Listening on one address instead of all

`PROBE_LISTEN=":8443"` (all addresses) is what the compose file uses. If you bind to one IP (`PROBE_LISTEN="203.0.113.5:8443"`), the self-test and health check dial that same IP. The service treats a connection from its own address as local, so the self-test keeps its exemption from the caps and the kill switch. Binding to one address is supported, but it was not exercised in the soak tests; run `make smoke` with that setting before relying on it.
