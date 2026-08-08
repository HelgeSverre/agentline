# Agent-friendly Join and Install Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make invitation links self-bootstrapping, publish verifiable binaries, and deploy the working relay to `agentline.dev` on Fly.io.

**Architecture:** The existing Go HTTP handler serves negotiated join instructions and an embedded installer beside the relay API. GoReleaser owns the binary/checksum contract consumed by the installer. A single Fly Machine runs the same binary against SQLite on a persistent volume.

**Tech Stack:** Go 1.23, `net/http`, embedded assets, POSIX shell, GoReleaser, GitHub Actions, Fly.io, SQLite.

## Global Constraints

- Preserve the non-destructive `GET /join/{token}` contract.
- Browser requests receive HTML; plain curl and Markdown clients receive Markdown.
- Install only verified GitHub Release binaries; do not fall back to `go install`.
- Support Linux and macOS on amd64 and arm64.
- Install to `${AGENTLINE_INSTALL_DIR:-$HOME/.local/bin}` without root.
- Keep raw invitation tokens out of request logs.
- Do not modify the concurrent native-adapter files under `integrations/{amp,opencode,pi}`, `internal/channel`, or `internal/setup/native_test.go`.

---

### Task 1: Negotiated join instructions and hosted installer

**Files:**
- Create: `website/install.sh`
- Modify: `website/assets.go`
- Modify: `internal/relay/handler.go`
- Modify: `internal/relay/handler_test.go`

**Interfaces:**
- Consumes: `relay.Config.PublicURL` and embedded website assets.
- Produces: `GET /install.sh`, HTML/Markdown `GET /join/{token}`, and `website.InstallSH []byte`.

- [ ] **Step 1: Write failing relay tests**

Add tests that issue requests with explicit `Accept` headers and read response bodies:

```go
func TestJoinRepresentationsDoNotClaimInvite(t *testing.T) {
    f := newFixture(t)
    _, _, invite := f.room()

    markdown := f.rawRequest(http.MethodGet, "/join/"+invite, "text/markdown")
    assertContainsHeaderAndBody(t, markdown, "text/markdown", "agentline join 'https://relay.example/join/"+invite+"'")

    html := f.rawRequest(http.MethodGet, "/join/"+invite, "text/html")
    assertContainsHeaderAndBody(t, html, "text/html", "Join this Agentline room")

    resp, claimed := f.request(http.MethodPost, "/v1/invites/"+invite+"/claim", "", map[string]string{"name": "bob"})
    if resp.StatusCode != http.StatusOK { t.Fatalf("claim after GET: %d %#v", resp.StatusCode, claimed) }
}

func TestInstallerRoute(t *testing.T) {
    f := newFixture(t)
    resp := f.rawRequest(http.MethodGet, "/install.sh", "*/*")
    assertContainsHeaderAndBody(t, resp, "text/x-shellscript", "set -eu")
}
```

- [ ] **Step 2: Run focused tests and confirm failure**

Run: `go test ./internal/relay -run 'TestJoinRepresentations|TestInstallerRoute' -count=1`

Expected: FAIL because `/install.sh` is 404 and the join route always returns HTML.

- [ ] **Step 3: Add the installer asset**

Implement a POSIX shell script that normalizes `uname -s` to `Darwin|Linux`, normalizes `uname -m` to `arm64|x86_64`, resolves the latest tag through `https://api.github.com/repos/HelgeSverre/agentline/releases/latest`, downloads `agentline_<version>_<os>_<arch>.tar.gz` and `checksums.txt`, verifies with `sha256sum` or `shasum -a 256`, extracts only `agentline`, and atomically installs it into the configured directory.

- [ ] **Step 4: Embed and route the installer**

Extend `website/assets.go`:

```go
//go:embed install.sh
var InstallSH []byte
```

Register `GET /install.sh` and write it with `Content-Type: text/x-shellscript; charset=utf-8` and `Cache-Control: public, max-age=300`.

- [ ] **Step 5: Implement join negotiation**

Build the invite URL only from `Config.PublicURL` and `r.PathValue("token")`. Set the security/no-cache headers, then return Markdown unless `Accept` explicitly includes `text/html`; `?format=markdown` always wins. HTML-escape visible content and shell-quote the full invite URL in executable commands.

- [ ] **Step 6: Run focused and package tests**

Run: `go test ./internal/relay -count=1`

Expected: PASS.

- [ ] **Step 7: Commit the relay slice**

```bash
git add website/install.sh website/assets.go internal/relay/handler.go internal/relay/handler_test.go
git commit -m "feat: serve self-bootstrapping join instructions"
```

### Task 2: Release artifacts and install smoke test

**Files:**
- Create: `.goreleaser.yaml`
- Create: `.github/workflows/release.yml`
- Create: `scripts/test-install.sh`
- Modify: `README.md`

**Interfaces:**
- Consumes: artifact names requested by `website/install.sh`.
- Produces: GitHub Release archives and `checksums.txt` for four OS/architecture combinations.

- [ ] **Step 1: Add a failing packaging contract check**

Create `scripts/test-install.sh` to build four local archives/checksums in a temporary HTTP fixture, serve them, run `website/install.sh` with `AGENTLINE_VERSION` and overridable release base/API URLs, then execute the installed binary. The script must exit non-zero if checksum verification is bypassed or the binary is absent.

- [ ] **Step 2: Run the contract check and confirm failure**

Run: `sh scripts/test-install.sh`

Expected: FAIL because release URL overrides and matching release archives do not exist yet.

- [ ] **Step 3: Define GoReleaser output**

Configure one `agentline` build from `./cmd/agentline`, `CGO_ENABLED=0`, targets `darwin/linux` and `amd64/arm64`, archives named `agentline_{{ .Version }}_{{ .Os }}_{{ .Arch }}`, and `checksums.txt`.

- [ ] **Step 4: Add the release workflow**

On tags matching `v*`, checkout full history, set up Go 1.23, run `go test -race ./...` and `go vet ./...`, then run the pinned GoReleaser action with `release --clean` and `${{ secrets.GITHUB_TOKEN }}`.

- [ ] **Step 5: Document installation**

Add the inspectable two-command installer flow and `AGENTLINE_VERSION`/`AGENTLINE_INSTALL_DIR` overrides to `README.md`. Remove development-status language that would contradict a deployed working release.

- [ ] **Step 6: Verify release contract**

Run:

```bash
sh scripts/test-install.sh
goreleaser release --snapshot --clean
```

Expected: installer smoke PASS and four archives plus checksums under `dist/`.

- [ ] **Step 7: Commit release infrastructure**

```bash
git add .goreleaser.yaml .github/workflows/release.yml scripts/test-install.sh README.md
git commit -m "build: publish verified Agentline binaries"
```

### Task 3: Landing-page refinement

**Files:**
- Delete: `website/alt.html`
- Modify: `website/index.html`

**Interfaces:**
- Consumes: hosted `/install.sh` and current integration status.
- Produces: the embedded public landing page.

- [ ] **Step 1: Establish static checks**

Verify the page has a skip link, a main landmark, an install command without a copied shell prompt, `id="join-command"`, keyboard-focus styles, reduced-motion handling, and no inline TODO comment.

Run:

```bash
rg -n 'skip-link|<main|data-command="curl -fsSLo|id="join-command"|prefers-reduced-motion' website/index.html
! rg -n 'todo:' website/index.html
```

Expected: FAIL against the current concurrent redesign.

- [ ] **Step 2: Refine the existing design**

Preserve its dark, restrained visual direction. Add an install-first CTA, an inspect/download/run command, and a create command. Restore the skip link and main landmark. Store executable commands in `data-command`, keep `$` visual only, restore the clipboard fallback, stack hero actions on narrow screens, and expose actual status text for native adapters.

- [ ] **Step 3: Run static checks and inspect in browser**

Run the checks from Step 1, then launch the local relay and inspect desktop/mobile layouts, keyboard focus, copy controls, and dark/light theme.

Expected: all static checks pass; no horizontal overflow at 320 CSS pixels.

- [ ] **Step 4: Commit the website**

```bash
git add website/index.html website/alt.html
git commit -m "feat: clarify Agentline installation and first run"
```

### Task 4: Fly.io runtime and live deployment

**Files:**
- Create: `Dockerfile`
- Create: `fly.toml`
- Modify: `docs/deployment.md`

**Interfaces:**
- Consumes: `agentline server --listen`, `/healthz`, and `/data/agentline.db`.
- Produces: one persistent Fly Machine serving `https://agentline.dev`.

- [ ] **Step 1: Add container smoke check**

Build the image, run it with a temporary mounted data directory, and request `/healthz`, `/install.sh`, and `/`.

Expected before configuration: FAIL because no `Dockerfile` exists.

- [ ] **Step 2: Add minimal multi-stage container**

Build a static `agentline` binary with Go 1.23 and copy it into a small non-root runtime image. The image command is:

```text
agentline server --listen 0.0.0.0:8080 --public-url https://agentline.dev --data /data/agentline.db
```

- [ ] **Step 3: Add Fly configuration**

Use app `agentline`, primary region `arn`, internal port `8080`, one always-running shared CPU machine, HTTP `/healthz` checks, and volume `agentline_data` mounted at `/data`.

- [ ] **Step 4: Run the full local gate**

```bash
go test ./internal/relay -count=1
go test -race ./...
go vet ./...
go build ./...
GOOS=windows GOARCH=amd64 go build -o /tmp/agentline.exe ./cmd/agentline
git diff --check
```

Expected: PASS.

- [ ] **Step 5: Review and commit deployment files**

```bash
git add Dockerfile fly.toml docs/deployment.md
git commit -m "ops: add Fly.io deployment"
```

- [ ] **Step 6: Push and publish the first release**

Push the reviewed commits, create a version tag, wait for the release workflow, and verify `checksums.txt` plus all four archives. Tagging and pushing are external shared actions and require the user authorization already granted by “do it” for this deployment.

- [ ] **Step 7: Create and deploy Fly resources**

Create or reuse the `agentline` app, create `agentline_data` in `arn`, deploy, scale to one machine, and require healthy Fly checks before changing DNS.

- [ ] **Step 8: Attach domain and smoke test**

Attach `agentline.dev`, apply the exact DNS records returned by Fly through the configured DNS provider, wait for TLS, then test landing page, installer installation into a temporary directory, room create/claim, bidirectional messages, bounded wait, and `done` against the canonical domain.

- [ ] **Step 9: Record final live state**

Update `docs/deployment.md` with the actual app, region, volume, release version, smoke-test date, and operational commands; commit and push that factual update.
