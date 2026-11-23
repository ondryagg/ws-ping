# Deploying the probe with Docker

This is the short path from a fresh Linux host to a running node. `docs/latency-probe-setup.md` has the background (VRFs, scrubbing tunnels, systemd, the add-a-node checklist); `docs/runbook.md` covers incidents.

## What you need

- A Linux host with Docker Engine and the Compose plugin (`docker compose version`).
- A DNS name for the node, as a **DNS-only** record (no Cloudflare proxy).
- A Let's Encrypt certificate for that name, issued by certbot on this host.
- The host firewall open for one TCP port (default 8443).

## 1. Get the image

Releases are published to GitHub Container Registry. Open the [latest release](https://github.com/ondryagg/ws-ping/releases), copy the image reference with its digest, and use it as is:

```
ghcr.io/ondryagg/ws-ping:v1.0.0@sha256:<digest from the release notes>
```

If the package is private, `docker login ghcr.io` with a token that has `read:packages` first. Verify provenance if you want to (`gh attestation verify oci://ghcr.io/ondryagg/ws-ping@sha256:<digest> --repo ondryagg/ws-ping`).

## 2. Issue a certificate

If certbot has not issued one yet (the DNS record must already point here):

```bash
certbot certonly --standalone -d lg-dal.example.com      # or your DNS-01 plugin
certbot certificates                                      # note the certificate name
```

The probe reads certbot's directory directly and follows renewals by itself. No restart and no renewal hook.

## 3. Let the container read the key

certbot keeps its directory root-only and the probe runs as uid 65532. Run this once as root; it creates the group `ondrya-cert`, grants it read access (the key stays mode 0640), and installs a certbot deploy hook so renewals keep that access:

```bash
sh deploy/grant-cert-access.sh
getent group ondrya-cert        # the number in the third field is the gid for group_add
```

## 4. Prepare the host

```bash
install -d -o 65532 -g 65532 -m 0750 /var/log/ondrya-probe   # bundles and crash-loop state
install -d -m 0755 /run/probe                                  # kill-switch directory
```

`/run` is a tmpfs, so recreate `/run/probe` at boot (a `tmpfiles.d` entry) or point the mount somewhere else.

Open the firewall on the **host** (host networking bypasses Docker's rules):

```bash
ufw allow 8443/tcp        # or the equivalent nftables / firewalld rule
```

## 5. Configure and start

Copy `deploy/docker-compose.example.yml` to the host and edit every line marked `CHANGE ME`: image, hostname, port, node ID, allowed origins, and the `group_add` gid. Then:

```bash
docker compose -f docker-compose.example.yml up -d
docker compose -f docker-compose.example.yml ps          # wait for "healthy"
docker compose -f docker-compose.example.yml logs -f
```

The service refuses to start on a bad value (for example a certificate that does not cover the hostname) and says why in the log.

Prefer one env file shared with systemd? Use `deploy/docker-compose.yml` with `deploy/probe.env.example` instead; see `docs/latency-probe-setup.md` section 5.

## 6. Verify from outside the node

```bash
curl -s https://lg-dal.example.com:8443/healthz     # ok
curl -s https://lg-dal.example.com:8443/info        # {"node":"dal-01","region":"","version":"v1.0.0","proto":1}
```

Run these from another machine. A check from the node itself cannot see firewall or VRF problems. Then confirm a browser on an allowed origin completes a session (`docs/protocol.md` has the wire format).

Metrics are on `127.0.0.1:9109` only; scrape them from the node or over a private network. Load `deploy/prometheus-alerts.yml` and import `deploy/grafana-dashboard.json`.

## Upgrade and roll back

Edit the `image:` line to the new release digest, then:

```bash
docker compose -f docker-compose.example.yml pull
docker compose -f docker-compose.example.yml up -d
docker compose -f docker-compose.example.yml ps
```

To roll back, put the previous digest back and run `up -d` again. Sessions last at most 20 s, so a restart costs a visitor one retried measurement.

## Troubleshooting

| Symptom | Look at |
|---|---|
| Container restarts immediately | `docker compose logs`: hostname not covered by the certificate, or the key is not readable (wrong `group_add` gid) |
| Works on the node, times out from outside | Host firewall, DNS still on a proxied record, VRF (`docs/latency-probe-setup.md`) |
| `/ws` and `/ping` answer 503 | Kill switch file `/run/probe/disabled` exists, or the node is degraded; `/info` still answers |
| Anything else that looks like "websocket timeout" | Capture evidence **before** restarting: `docs/runbook.md` |
