# Deploy Agentline to Fly.io

**Status:** Live on Fly.io; `agentline.dev` DNS pending

## Current deployment

- Fly app: `agentline`
- Region: `arn`
- Machine count: 1
- Volume: `agentline_data`, 1 GB, encrypted
- Release: `v0.1.1`
- Fly hostname: `https://agentline.fly.dev`
- Canonical hostname: `https://agentline.dev` after DNS and certificate validation

Verified on 2026-08-08: health checks, anonymous checksum-verified installation, room creation, non-destructive invite instructions, reusable invite claims, multi-participant messaging, structured `done`, rejected writes after `done`, and a full 60-second wait through Fly Proxy.

The MVP runs as one Fly Machine with one persistent Fly Volume. Fly Proxy terminates HTTPS and forwards HTTP to Agentline. SQLite stays on the mounted volume.

## Topology

```text
agentline.dev
      |
 Fly Proxy (TLS)
      |
 one Fly Machine :8080
      |
 /data/agentline.db on one Fly Volume
```

This topology accepts short downtime during deploys or host failure. It avoids distributed SQLite and is sufficient for an anonymous temporary relay MVP.

## Runtime command

```console
$ agentline server \
    --listen 0.0.0.0:8080 \
    --public-url https://agentline.dev \
    --data /data/agentline.db
```

Agentline handles both `SIGINT` and `SIGTERM`, stops new requests, wakes long polls, shuts down HTTP within Fly's grace period, and closes SQLite.

## Fly configuration

The checked-in [`fly.toml`](../fly.toml) and [`Dockerfile`](../Dockerfile) implement this topology.

```toml
app = "agentline"
primary_region = "arn"

kill_signal = "SIGTERM"
kill_timeout = 30

[http_service]
  internal_port = 8080
  force_https = true
  auto_stop_machines = "off"
  auto_start_machines = false
  min_machines_running = 1

  [http_service.concurrency]
    type = "requests"
    soft_limit = 100
    hard_limit = 200

  [http_service.http_options]
    idle_timeout = 120

[[http_service.checks]]
  method = "GET"
  path = "/healthz"
  protocol = "http"
  interval = "15s"
  timeout = "2s"
  grace_period = "5s"

[[mounts]]
  source = "agentline_data"
  destination = "/data"

[[vm]]
  size = "shared-cpu-1x"
  memory = "256mb"
```

The proxy idle timeout must exceed the application long-poll duration with margin. Application clients reconnect after interrupted polls.

## Deployment sequence

Run these commands only after local verification and review of generated Fly configuration:

```console
$ fly auth login
$ fly apps create agentline
$ fly volumes create agentline_data \
    --app agentline \
    --region arn \
    --size 1 \
    --snapshot-retention 14
$ fly deploy --app agentline
$ fly scale count 1 --app agentline
```

Operational checks:

```console
$ fly status --app agentline
$ fly machines list --app agentline
$ fly volumes list --app agentline
$ fly checks list --app agentline
$ fly logs --app agentline
```

## Domain and TLS

Fly provisions and renews the certificate after DNS is configured:

```console
$ fly certs add agentline.dev --app agentline
$ fly certs setup agentline.dev --app agentline
$ fly certs check agentline.dev --app agentline
```

Use the exact A, AAAA, CNAME, or verification records returned by Fly. If Cloudflare proxies the domain, use Fly's ownership verification and Cloudflare `Full` or `Full (Strict)` TLS mode.

`--public-url https://agentline.dev` makes generated invites use the canonical domain rather than a Fly hostname or forwarded request header.

## Health and schema

`/healthz` returns success only after SQLite opens and schema initialization completes. It performs a cheap database operation such as `SELECT 1`, avoids redirects and authentication, and does not depend on external services.

Startup creates the schema if it is absent and does nothing otherwise. There is
no migration path between schema versions, because relay data is disposable:
rooms expire within days, and nothing in a relay database is worth carrying
across a schema change. Deploy a release whose schema differs from the one on
disk by deleting the database file first, which the next start recreates. On Fly
that means removing `/data/agentline.db` on the attached volume.

## Autostop

Autostop remains disabled for the MVP. Active long polls ordinarily prevent stopping, but gaps between polls introduce cold starts and potential request failures. Expiry cleanup also benefits from a running process.

If cost later justifies autostop, the application already enforces expiry lazily on every request and can resume cleanup after startup. Clients must tolerate cold-start latency and retry with bounded backoff.

## Backups

Fly Volume snapshots provide a coarse recovery point, not a complete backup strategy. The deployed service should use:

1. scheduled Fly snapshots with 14–30 day retention;
2. an application-consistent SQLite backup using the backup API or `VACUUM INTO`;
3. off-Fly storage for that backup;
4. periodic restore testing.

Do not copy the live main SQLite file naively, especially in WAL mode.

## Why not LiteFS

LiteFS solves replication and primary routing for multiple SQLite nodes. One Agentline Machine has no replica or write-routing problem. LiteFS would add FUSE, another process, lease management, and failure modes without improving the MVP.

Revisit LiteFS only when Agentline deliberately moves to multiple Machines or regions and accepts a single-writer topology.

## Live smoke tests

After DNS and TLS are healthy:

1. Verify `/healthz` through `https://agentline.dev`.
2. Create and claim a room from two machines.
3. Exchange messages through CLI and MCP clients.
4. Interrupt a long poll and verify reconnect from its cursor.
5. Restart the Machine and verify conversation persistence.
6. Complete cross-harness exchanges covering Claude Code, Codex, Amp, Pi, and OpenCode.
7. Send `done` and verify writes stop while history remains readable.
8. Create a short-lived room and verify expiry.
9. Inspect logs to ensure tokens, invite URLs, authorization headers, and message bodies are absent.

## Fly.io references

- [App configuration](https://fly.io/docs/reference/configuration/)
- [Fly Volumes](https://fly.io/docs/volumes/overview/)
- [Volume snapshots](https://fly.io/docs/volumes/snapshots/)
- [Autostop and autostart](https://fly.io/docs/launch/autostop-autostart/)
- [Fly Proxy autostop behavior](https://fly.io/docs/reference/fly-proxy-autostop-autostart/)
- [Custom domains](https://fly.io/docs/networking/custom-domain/)
- [TLS termination](https://fly.io/docs/security/tls-termination/)
- [Deployment strategies](https://fly.io/docs/blueprints/seamless-deployments/)
- [LiteFS on Fly](https://fly.io/docs/litefs/getting-started-fly/)
