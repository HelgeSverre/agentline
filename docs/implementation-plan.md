# Agentline MVP implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `subagent-driven-development` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build and verify a single Go binary that hosts or connects to temporary two-agent rooms, exposes CLI and MCP interfaces, installs supported harness integrations, and is ready for deployment at `agentline.dev`.

**Architecture:** The `agentline` binary owns a HTTP relay, SQLite persistence, local credentials, CLI commands, and a stdio MCP server. Every harness uses the same relay and room semantics; native adapters are optional local delivery improvements. The server embeds the website and serves it beside `/api` APIs.

**Tech stack:** Go, `net/http`, `database/sql`, `modernc.org/sqlite`, `github.com/modelcontextprotocol/go-sdk/mcp`, `github.com/spf13/cobra` for the CLI, embedded HTML/skill/plugin assets, and TypeScript only for harness adapters.

## Global constraints

- Module path: `github.com/HelgeSverre/agentline`.
- Prefer the Go standard library for server, storage, and client code; the CLI uses Cobra/pflag for command dispatch, help, and flag parsing. Do not add an HTTP framework, ORM, or dependency-injection framework.
- Hosted server default: `https://agentline.dev`.
- Room defaults: multiple participants, 24-hour TTL, seven-day maximum, and 1,000 events.
- Message body maximum: 64 KiB.
- Hosted rate defaults: 20 room creations per IP per hour and 120 sends per IP per minute.
- Tokens are random, shown once, hashed before database storage, and omitted from logs.
- MCP waits default to 60 seconds, honor cancellation, and return timeout as data.
- Peer content is untrusted collaborator input.
- End-to-end encryption, group rooms, attachments, accounts, and Codex App Server hosting are outside the MVP.
- Do not deploy, change DNS, push, or create shared infrastructure without explicit approval at that stage.

## Planned file ownership

```text
cmd/agentline/main.go               process entry point
internal/model/model.go             shared domain types
internal/securetoken/token.go       token generation and hashing
internal/localconfig/config.go      local credentials and preferences
internal/store/sqlite.go            SQLite persistence
internal/relay/                      HTTP handler and lifecycle
internal/client/client.go           typed HTTP client
internal/cli/                        command parser and implementations
internal/localserver/local.go       managed loopback relay
internal/mcpserver/server.go        portable stdio MCP server
internal/setup/                      setup planning and installation
internal/channel/server.go          Claude Channel adapter
integrations/                        skills and native adapters
web/index.html                      embedded website
README.md, LICENSE                  public project entry points
Dockerfile, fly.toml                deployment readiness
```

---

### Task 1: Website, README, and MIT license

**Parallel:** Run alongside Task 2. Do not edit Go files or `docs/`.

**Files:**
- Create: `web/index.html`
- Create: `README.md`
- Create: `LICENSE`

**Consumes:** `docs/design.md`, `docs/integrations.md`.

- [ ] Write a README with the problem, hosted flow, local flow, self-host command, harness matrix, plaintext-relay caveat, development status, and links to `docs/`.
- [ ] Add the standard MIT license with `Copyright (c) 2026 Helge Sverre`.
- [ ] Build a self-contained one-page site with no runtime dependencies. Explain “let coding agents talk,” show the three-command flow, list all five harnesses, and distinguish portable CLI/MCP from native adapters.
- [ ] Include this exact join placeholder so the server can render invite instructions without claiming the invite:

```html
<code id="join-command" hidden></code>
```

- [ ] Validate:

```bash
test -s README.md
test -s LICENSE
test -s web/index.html
grep -q 'id="join-command"' web/index.html
```

Expected: exit code 0.

---

### Task 2: Go foundation, model, tokens, and local configuration

**Parallel:** Run alongside Task 1. Do not edit website or root docs.

**Files:**
- Create: `go.mod`
- Create: `cmd/agentline/main.go`
- Create: `internal/model/model.go`
- Create: `internal/securetoken/token.go`
- Create: `internal/securetoken/token_test.go`
- Create: `internal/localconfig/config.go`
- Create: `internal/localconfig/config_test.go`

**Produces:**

```go
type RoomStatus string
type MessageKind string
type Room struct { ID, Name string; Status RoomStatus; MaxParticipants int; CreatedAt, ExpiresAt time.Time }
type Participant struct { ID, RoomID, Name string; JoinedAt time.Time }
type Message struct { ID, RoomID, SenderID, SenderName, Body, ReplyTo string; Sequence int64; Kind MessageKind; CreatedAt time.Time }
type RoomCredential struct { RoomID, RoomName, ServerURL, ParticipantID, Token string; Cursor int64 }

func securetoken.New(bytes int) (string, error)
func securetoken.Hash(raw string) [32]byte

type localconfig.Store struct { Root string }
func (s Store) Load() (Config, error)
func (s Store) Save(Config) error
func (s Store) SaveRoom(model.RoomCredential) error
func (s Store) LoadRoom(handle string) (model.RoomCredential, error)
func (s Store) ListRooms() ([]model.RoomCredential, error)
func (s Store) RemoveRoom(roomID string) error
```

- [ ] Write tests for token uniqueness, hash stability, default server, atomic saves, one-room resolution, and refusal to guess among multiple rooms.
- [ ] Run `go test ./internal/securetoken ./internal/localconfig`; expect failure before implementation.
- [ ] Implement with `crypto/rand`, `crypto/sha256`, raw URL-safe base64, `os.UserConfigDir`, temporary files, and atomic rename.
- [ ] Run the focused tests; expect PASS.

---

### Task 3: Transactional SQLite store

**Files:**
- Create: `internal/store/store.go`
- Create: `internal/store/sqlite.go`
- Create: `internal/store/sqlite_test.go`

**Produces:**

```go
type CreateRoomParams struct { Name, CreatorName string; TTL time.Duration; MaxParticipants int }
type CreatedRoom struct { Room model.Room; Creator model.Participant; CreatorToken, InviteToken string }
type ClaimResult struct { Room model.Room; Participant model.Participant; ParticipantToken string }
type AppendParams struct { RoomID, ParticipantID, MessageID, Body, ReplyTo string; Kind model.MessageKind }

type Store interface {
    CreateRoom(context.Context, CreateRoomParams) (CreatedRoom, error)
    ClaimInvite(context.Context, string, string) (ClaimResult, error)
    Authenticate(context.Context, string, string) (model.Participant, error)
    GetRoom(context.Context, string) (model.Room, error)
    Append(context.Context, AppendParams) (model.Message, error)
    MessagesAfter(context.Context, string, int64, int) ([]model.Message, error)
    CloseRoom(context.Context, string, string, string) (model.Message, error)
    DeleteExpired(context.Context, time.Time) (int64, error)
    Ping(context.Context) error
    Close() error
}

func OpenSQLite(path string, now func() time.Time) (Store, error)
```

- [ ] Write contract tests for creation, hashed credentials, concurrent reusable invite claims, capacity, fixed expiry, auth, ordering, idempotency, 1,000-event limit, `done`, history after `done`, and lazy expiry.
- [ ] Run `go test -race ./internal/store`; expect failure.
- [ ] Implement with `database/sql`, `modernc.org/sqlite`, schema migration, short transactions, and unique constraints. Use WAL, foreign keys, and a 5-second busy timeout.
- [ ] Run `go test -race ./internal/store`; expect PASS.

---

### Task 4: HTTP relay and embedded website

**Files:**
- Create: `internal/relay/handler.go`
- Create: `internal/relay/handler_test.go`
- Create: `internal/relay/ratelimit.go`
- Create: `internal/relay/ratelimit_test.go`
- Create: `internal/relay/server.go`
- Create: `web/assets.go`

**Produces:**

```go
type Config struct {
    PublicURL string
    MaxTTL, WaitMax time.Duration
    MessageBytes int64
    CreatePerHour, SendPerMinute int
}
func NewHandler(store.Store, Config, func() time.Time) http.Handler
func Serve(context.Context, net.Listener, http.Handler, store.Store) error
```

- [ ] Use `httptest.Server` to test every endpoint in `docs/protocol.md`, bearer auth, reusable invite claims, body and rate limits, stable errors, cursors, idempotency, `/healthz`, `/`, and non-destructive `GET /join/{token}`.
- [ ] Test wait wake-up, timeout, cancellation, and graceful server shutdown.
- [ ] Run `go test -race ./internal/relay`; expect failure.
- [ ] Implement with `http.ServeMux`, `http.MaxBytesReader`, strict JSON, route-pattern logging, and a small in-memory fixed-window limiter. Embed `web/index.html` from `web/assets.go`. Never log raw invite paths, authorization headers, tokens, or bodies.
- [ ] Run `go test -race ./internal/relay`; expect PASS.

---

### Task 5: Typed HTTP client and complete CLI

**Files:**
- Create: `internal/client/client.go`
- Create: `internal/client/client_test.go`
- Create: `internal/cli/cli.go`
- Create: `internal/cli/commands.go`
- Create: `internal/cli/cli_test.go`
- Modify: `cmd/agentline/main.go`

**Produces:**

```go
func (c Client) CreateRoom(context.Context, string, time.Duration) (CreateResult, error)
func (c Client) ClaimInvite(context.Context, string, string) (ClaimResult, error)
func (c Client) Room(context.Context, string) (model.Room, error)
func (c Client) Send(context.Context, string, string, string, string) (model.Message, error)
func (c Client) Read(context.Context, string, int64) ([]model.Message, error)
func (c Client) Wait(context.Context, string, int64, time.Duration) (WaitResult, error)
func (c Client) Done(context.Context, string, string) (model.Message, error)

func cli.Run(context.Context, []string, io.Reader, io.Writer, io.Writer, Dependencies) int
```

- [ ] Test error decoding, idempotent retry, invite parsing, credentials, create/join/send/read/wait/done/status/server commands, `--json`, room resolution, cursor persistence, and secret-safe output.
- [ ] Run `go test ./internal/client ./internal/cli`; expect failure.
- [ ] Implement command parsing with one `flag.FlagSet` per command and stable human/JSON output.
- [ ] Run focused tests and `go build ./cmd/agentline`; expect PASS.

---

### Task 6: Managed loopback relay

**Files:**
- Create: `internal/localserver/local.go`
- Create: `internal/localserver/local_test.go`
- Modify: `internal/cli/commands.go`

**Produces:**

```go
type Manager struct { Config localconfig.Store; Executable string }
func (m Manager) Ensure(context.Context) (string, error)
func (m Manager) Stop(context.Context) error
```

- [ ] Test free-port startup, healthy reuse, stale PID recovery, `create --local`, graceful stop, and protection against killing an unrelated reused PID.
- [ ] Run `go test ./internal/localserver ./internal/cli`; expect failure.
- [ ] Spawn the current executable with loopback listen, local SQLite, and a machine-readable readiness pipe; persist and verify PID plus URL.
- [ ] Run focused tests; expect PASS.

---

### Task 7: Portable stdio MCP server

**Files:**
- Create: `internal/mcpserver/server.go`
- Create: `internal/mcpserver/server_test.go`
- Modify: `internal/cli/commands.go`

**Tools:** `create_room`, `join_room`, `send_message`, `read_messages`, `wait_for_message`, `end_conversation`, `get_room_status`.

- [ ] Use the official Go MCP client to test initialize, exact tools, create/join, send/read, timeout as structured data, receive, cancellation, and `done`.
- [ ] Assert persisted participant tokens never appear in tool results after create/join.
- [ ] Run `go test ./internal/mcpserver`; expect failure.
- [ ] Implement with `mcp.NewServer`, typed tool input/output structs, `mcp.AddTool`, and `mcp.StdioTransport`. Default waits to 60 seconds.
- [ ] Run MCP tests and `go test ./...`; expect PASS.

---

### Task 8: Shared skill and stable setup installers

**Files:**
- Create: `integrations/skills/agentline/SKILL.md`
- Create: `internal/setup/assets.go`
- Create: `internal/setup/setup.go`
- Create: `internal/setup/setup_test.go`
- Modify: `internal/cli/commands.go`

**Produces:**

```go
type Change struct { Path, Description string; Before, After []byte }
type Plan struct { Target string; Changes []Change; Warnings []string }
func BuildPlan(target, home, executable string, remove bool) (Plan, error)
func Apply(Plan) error
func Doctor(context.Context, string, string, string) Report
```

- [ ] Test temporary-home setup for Claude, Codex, Amp, Pi, OpenCode, and generic MCP; cover idempotence, preview, unrelated-setting preservation, backups, removal, and absolute executable paths.
- [ ] Run `go test ./internal/setup`; expect failure.
- [ ] Write the Agent Skill with the trust boundary and repeated bounded-wait behavior.
- [ ] Implement narrow atomic config edits, preview plus confirmation, `--yes`, removal of only owned entries, and `doctor` checks.
- [ ] Run setup and full tests; expect PASS.

---

### Task 9: Native harness adapters

**Files:**
- Create: `integrations/pi/agentline.ts`
- Create: `integrations/amp/agentline.ts`
- Create: `integrations/opencode/agentline.ts`
- Create: `internal/channel/server.go`
- Create: `internal/channel/server_test.go`
- Create: `internal/setup/native_test.go`
- Modify: `internal/setup/assets.go`
- Modify: `internal/setup/setup.go`
- Modify: `internal/cli/commands.go`

- [x] Test complete embedded assets, documented install paths, removal, bounded listeners, message-ID deduplication, and experimental warnings.
- [x] Implement Pi with `session_start`, `session_shutdown`, `sendUserMessage`, and busy `followUp` delivery. Installed by `agentline setup pi --native`.
- [x] Implement Amp with explicit room-to-thread mapping, `appendUserMessage(..., {steer: true})`, and `onDispose`; label idle wake experimental. Installed by `agentline setup amp --native`.
- [x] Implement OpenCode event listening and context injection; keep automatic prompt triggering opt-in and experimental. Installed by `agentline setup opencode --native`.
- [x] Implement `agentline channel` with `experimental["claude/channel"]`, `notifications/claude/channel`, string metadata, and the regular reply tool. The Go MCP SDK exposes no generic notification sender, so the adapter speaks JSON-RPC over stdio directly rather than importing SDK internals.
- [x] Run `go test ./internal/setup ./internal/channel` and `go test ./...`; expect PASS.

---

### Task 10: Black-box reliability and security verification

**Files:**
- Create: `internal/e2e/e2e_test.go`
- Create: `docs/smoke-tests.md`
- Modify implementation only for demonstrated defects.

- [ ] Build one binary; start a temporary relay; use two isolated config roots; create, claim, exchange both ways, time out, reconnect by cursor, send `done`, restart, and read retained history.
- [ ] Test concurrent claims, duplicate sends, oversized body, third participant, expiry, writes after `done`, invalid bearer, wait cancellation, shutdown with waits, and secret-free logs.
- [ ] Run:

```bash
go test -race ./...
go vet ./...
go build ./cmd/agentline
```

Expected: all pass.

- [ ] Document exact normally launched smoke tests for Claude Code, Codex, Amp, Pi, and OpenCode, separating portable conformance from experimental wake checks.

---

### Task 11: Container and Fly.io readiness

**Files:**
- Create: `Dockerfile`
- Create: `.dockerignore`
- Create: `fly.toml`
- Modify: `README.md`
- Modify: `docs/deployment.md` only if implementation differs.

- [ ] Add a multi-stage static Go build, non-root runtime, port 8080, and `/data` persistence.
- [ ] Add the reviewed `arn` single-Machine Fly configuration: one volume, autostop off, `/healthz`, 120-second idle timeout, SIGTERM, and 30-second kill timeout.
- [ ] Build and smoke-test locally:

```bash
docker build -t agentline:local .
docker run --rm -d --name agentline-local -p 8080:8080 -v agentline-test:/data agentline:local
curl --fail http://127.0.0.1:8080/healthz
docker rm -f agentline-local
docker volume rm agentline-test
```

Expected: image builds and health check succeeds.

- [ ] Stop and request approval before Fly app creation, deploy, certificate creation, DNS changes, or live traffic tests.

## Execution waves

```text
Wave 1: Task 1 (web/docs) || Task 2 (Go foundation)
Wave 2: Task 3
Wave 3: Task 4, then Task 5
Wave 4: Task 6 || Task 7
Wave 5: Task 8
Wave 6: Task 9
Wave 7: Task 10
Wave 8: Task 11, then deployment approval gate
```

The orchestrator reviews each subagent diff before committing. Parallel workers own disjoint files and do not commit independently in the shared checkout.
