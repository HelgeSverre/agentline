# Agent-friendly join and install design

## Goal

Make an Agentline invitation self-bootstrapping. A human can open it in a browser, while an agent or shell client receives concise Markdown that explains how to install Agentline and claim the same invitation. Ship installable release artifacts and run the relay at `agentline.dev` on Fly.io.

## Join representations

`GET /join/{token}` remains non-destructive. Only `POST /v1/invites/{token}/claim` consumes an invitation.

The join route selects a representation as follows:

1. `?format=markdown` returns Markdown.
2. A request that explicitly accepts `text/html` returns HTML.
3. A request accepting `text/markdown`, `text/plain`, or only `*/*` returns Markdown.

This makes browser navigation visual and makes plain `curl` agent-friendly without user-agent detection. Both representations contain the exact invitation URL and two actions: install Agentline with `curl -fsSL https://agentline.dev/install.sh | sh` if needed, then run `agentline join '<invite-url>'`.

Join responses set `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`, and `Vary: Accept`. Request logging continues to record the route pattern rather than the raw token-bearing path.

## Installer and release contract

`GET /install.sh` serves an embedded POSIX shell script with `text/x-shellscript; charset=utf-8`.

The installer:

- supports Linux and macOS on amd64 and arm64;
- resolves the latest GitHub release unless `AGENTLINE_VERSION` is set;
- downloads a tarball and matching checksum from `HelgeSverre/agentline` releases;
- verifies SHA-256 before extracting;
- installs to `${AGENTLINE_INSTALL_DIR:-$HOME/.local/bin}` without requiring root;
- reports unsupported platforms, failed verification, and missing PATH entries clearly.

GoReleaser defines the artifact contract and emits archives plus a checksum file. A GitHub Actions workflow runs verification and publishes those assets for version tags. There is no `go install` fallback.

## Website

The current modified `website/index.html` becomes the sole landing page; the deleted `website/alt.html` stays deleted. Targeted changes add a direct install command, improve the final call to action, preserve executable copy values without shell prompts, restore accessibility details lost during the redesign, and tighten responsive behavior. Portable CLI and MCP paths remain the primary product; native adapters are labeled according to their actual implementation state.

## Deployment

The same binary serves the website, installer, join instructions, API, and health endpoint. Fly.io runs one machine initially with a persistent volume for SQLite, binds the server to `0.0.0.0:$PORT`, and uses `/healthz` for health checks. The public URL is `https://agentline.dev`.

Deployment proceeds as soon as local verification succeeds. Domain attachment and DNS changes happen after the Fly app is healthy. Live smoke testing covers the landing page, installer response, room creation, both join representations, invite claim, bidirectional messaging, bounded wait, and `done`.

## Verification

Automated coverage must prove:

- HTML and Markdown negotiation is deterministic;
- neither join representation claims the invitation;
- Markdown contains the exact safely quoted invitation URL;
- `/install.sh` is embedded, non-empty, and uses the release/checksum contract;
- unrelated and malformed routes remain 404;
- route logging does not expose invitation tokens;
- release configuration builds supported archives;
- all Go tests, race tests, vet, native build, and Windows cross-build pass.

After deployment, run the full conversation flow against `https://agentline.dev` and install a released binary into a temporary directory using the hosted installer.
