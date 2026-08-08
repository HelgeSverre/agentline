# Multi-participant Rooms and Session Identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the disposable two-party protocol with unlimited or optionally capped reusable-invite rooms, participant-addressed private messages, correct visibility/cursor semantics, and unambiguous per-session local identity across HTTP, CLI, and MCP.

**Architecture:** Make participant identity explicit at every boundary: SQLite owns membership, capacity, addressing, visibility, and room-global cursors; authenticated HTTP handlers pass the participant ID into store queries; local configuration stores one credential per room/participant; CLI selects explicitly while each MCP process caches a room-to-participant binding. This is a clean cutoff: replace the schema and protocol directly, delete old data, and add no migration or compatibility path.

**Tech Stack:** Go 1.23, `net/http`, `database/sql`, `modernc.org/sqlite`, `github.com/modelcontextprotocol/go-sdk/mcp`, file-backed JSON credentials guarded by `gofrs/flock`, embedded Markdown/HTML join instructions, Fly.io with one persistent SQLite volume.

## Global Constraints

- This is planning output only. Do not modify implementation files or commit while producing this document.
- Before execution, preserve and reconcile the worktree's unrelated/concurrent edits. In particular, never overwrite changes in `internal/relay/handler.go`, `internal/relay/handler_test.go`, `website/`, `integrations/`, `internal/channel/`, or `internal/setup/native_test.go`; inspect the current diff before each edit and stage only task-owned hunks.
- Use an isolated worktree via `superpowers:using-git-worktrees` if the concurrent edits have not landed. Rebase/cherry-pick onto their final form rather than resetting, stashing, or replacing them.
- Treat the new database and API as a hard pre-launch cutoff. Do not add `ALTER TABLE`, schema-version detection, old credential-file discovery, legacy JSON fields, fallback reads, or backward-compatibility branches.
- Delete local development databases before testing the new build. Existing SQLite files are intentionally unsupported because `CREATE TABLE IF NOT EXISTS` cannot transform the old schema.
- Rooms are unlimited by default. `max_participants` is nullable; an explicitly supplied value must be positive and has no application-wide upper bound.
- One reusable invite remains valid while its room is active, unexpired, and below optional capacity. Every successful claim creates a distinct participant; participant display names may repeat.
- Capacity check and membership insert must occur in the same SQLite immediate transaction. Concurrent claims must never exceed `max_participants`.
- `to == ""` is a broadcast. A non-empty `to` is a participant ID in the same room; unknown and cross-room IDs are rejected without leaking membership from another room.
- Broadcasts are visible to all participants. Private messages are visible only to sender and recipient. `done` is room-wide, visible to all, and wakes all waiters.
- Explicit history includes the authenticated participant's own visible events. Bounded wait skips ordinary self-authored messages but advances its room-global cursor across self-authored and invisible events.
- Store local credentials only at `rooms/<room-id>/<participant-id>.json`, with directories mode `0700` and files mode `0600`. Cursor locks are per room and participant.
- Each MCP server process owns a session-local room-ID-to-participant-ID binding. Create/join bind immediately; exactly one saved identity may auto-select; multiple identities require `participant_id` once and are remembered for the process lifetime.
- Every CLI room command accepts `--participant <id>`. Ambiguity errors list available participant IDs and names; never select by non-unique participant name.
- Do not add dependencies. Keep the existing 64 KiB message, 1,000-event, TTL, rate-limit, token secrecy, idempotency, cancellation, and log-redaction guarantees.
- Follow strict red-green-refactor TDD. Every behavior task starts with a focused failing test, runs that exact test to observe the intended failure, adds the smallest implementation, reruns the focused package, and commits only reviewed task files.
- The final release/protocol version is `v0.2.0` / HTTP `/v2`; do not retain `/v1` aliases. MCP server implementation version is `2.0.0`.
- Fly data destruction, deployment, tagging/pushing, and live traffic are explicit operator-approved production actions. Verify the app and volume names before the wipe and record the smoke evidence in `docs/deployment.md`.

---

## File and Interface Map

**Core model/store**

- Modify `internal/model/model.go`: nullable room capacity, participant membership in status, message recipient.
- Modify `internal/store/store.go`: optional capacity, recipient-aware append, participant-visible history with scan cursor, and membership listing.
- Modify `internal/store/sqlite.go`: clean-cutoff schema and all transactional/visibility logic.
- Modify `internal/store/sqlite_test.go`: schema, reusable/concurrent claims, addressing, privacy, cursor, completion, and expiry tests.

**HTTP/client**

- Modify `internal/relay/handler.go` and `internal/relay/handler_test.go`: `/v2`, request fields, authenticated visibility, wait scanning, membership status, reusable join copy, and safe error mapping. These files currently contain concurrent work; edit only after reconciling it.
- Modify `internal/client/client.go` and `internal/client/client_test.go`: new request shapes and cursor-bearing read/wait results.

**Identity adapters**

- Modify `internal/localconfig/config.go` and `internal/localconfig/config_test.go`: nested credential storage and participant-aware resolution/cursors.
- Modify `internal/cli/commands.go`, `internal/cli/flags.go`, `internal/cli/help.go`, and `internal/cli/cli_test.go`: `--max-participants`, `--to`, and universal `--participant` selection.
- Modify `internal/mcpserver/server.go` and `internal/mcpserver/server_test.go`: session binding, `participant_id`, `max_participants`, `to`, identity-bearing outputs, and updated descriptions.

**Prompts/docs/release/operations**

- Modify `integrations/skills/agentline/SKILL.md`: creator/joiner asymmetry and stopping discipline. Reconcile concurrent integration changes first.
- Modify `README.md`, `docs/design.md`, `docs/protocol.md`, `docs/integrations.md`, and `docs/deployment.md`: group-room contract, local identities, `/v2`, clean cutoff, and production runbook.
- Modify `internal/relay/handler.go` join-page copy rather than `website/index.html`; the join response is currently generated by `handler.join`.
- Modify `internal/mcpserver/server.go` MCP implementation version and release-facing documentation to `v0.2.0`; no new version source is required.

## Canonical Interfaces

Use these names consistently in every task:

```go
// internal/model/model.go
type Room struct {
	ID              string        `json:"id"`
	Name            string        `json:"name"`
	Status          RoomStatus    `json:"status"`
	MaxParticipants *int          `json:"max_participants"`
	Participants    []Participant `json:"participants,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	ExpiresAt       time.Time     `json:"expires_at"`
}

type Message struct {
	ID, RoomID, SenderID, SenderName string
	To                               string `json:"to,omitempty"`
	Body, ReplyTo                    string
	Sequence                         int64
	Kind                             MessageKind
	CreatedAt                        time.Time
}
```

Keep the existing explicit JSON tags on all compacted fields above. Store boundaries become:

```go
type CreateRoomParams struct {
	Name, CreatorName string
	TTL               time.Duration
	MaxParticipants   *int
}

type AppendParams struct {
	RoomID, ParticipantID, MessageID, To, Body, ReplyTo string
	Kind                                               model.MessageKind
}

type VisibleMessages struct {
	Messages []model.Message
	Cursor   int64 // highest room-global sequence scanned, visible or not
}

type Store interface {
	CreateRoom(context.Context, CreateRoomParams) (CreatedRoom, error)
	ClaimInvite(context.Context, string, string) (ClaimResult, error)
	Authenticate(context.Context, string, string) (model.Participant, error)
	GetRoom(context.Context, string) (model.Room, error)
	Participants(context.Context, string) ([]model.Participant, error)
	Append(context.Context, AppendParams) (model.Message, error)
	MessagesAfter(context.Context, string, string, int64, int, bool) (VisibleMessages, error)
	CloseRoom(context.Context, string, string, string) (model.Message, error)
	DeleteExpired(context.Context, time.Time) (int64, error)
	Ping(context.Context) error
	Close() error
}
```

`MessagesAfter(ctx, roomID, participantID, after, limit, skipSelf)` scans room-global rows after `after`, returns at most `limit` visible rows, and returns the highest sequence examined as `Cursor`. `skipSelf=false` is explicit history; `skipSelf=true` is bounded wait. A `done` row is never skipped.

---

### Task 1: Replace the SQLite Schema and Nullable Capacity Model

**Files:**
- Modify: `internal/model/model.go`
- Modify: `internal/store/store.go`
- Modify: `internal/store/sqlite.go:40-75,86-145,218-225`
- Test: `internal/store/sqlite_test.go`

**Interfaces:**
- Produces: `Room.MaxParticipants *int`, `CreateRoomParams.MaxParticipants *int`, and the canonical clean schema consumed by all later tasks.
- Preserves: `CreateRoom`, token hashing, fixed expiry, and `waiting_for_peer` creation status.

- [ ] **Step 1: Replace the two-person schema tests with clean-cutoff capacity tests**

Add table-driven cases that create an unlimited room (`nil`), capped room (`ptr(3)`), and reject `ptr(0)`/`ptr(-1)`. Open the raw database and assert `max_participants IS NULL` for unlimited, `3` for capped, and that direct SQL accepts a large positive value but rejects zero:

```go
func intPtr(n int) *int { return &n }

func TestCreateRoomSupportsUnlimitedAndPositiveCapacity(t *testing.T) {
	// nil => SQL NULL and JSON null; ptr(3) => 3; 0/-1 => ErrInvalid.
}

func TestSchemaIsCleanMultiParticipantCutoff(t *testing.T) {
	// PRAGMA table_info/messages and invites: no claimed_at/claimed_by,
	// messages has nullable recipient_id, rooms.max_participants is nullable.
}
```

- [ ] **Step 2: Run the focused tests and confirm the old contract fails**

Run: `go test ./internal/store -run 'Test(CreateRoomSupportsUnlimitedAndPositiveCapacity|SchemaIsCleanMultiParticipantCutoff)' -count=1`

Expected: FAIL because capacity is an `int`, `nil` is unsupported, the schema caps values at two, invites retain claim columns, and messages lack `recipient_id`.

- [ ] **Step 3: Replace the schema directly and update nullable scanning**

Use this schema exactly; do not add migration SQL:

```sql
CREATE TABLE IF NOT EXISTS rooms (
 id TEXT PRIMARY KEY, public_name TEXT NOT NULL,
 max_participants INTEGER CHECK(max_participants > 0),
 status TEXT NOT NULL, next_sequence INTEGER NOT NULL DEFAULT 1,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
 ended_at INTEGER, ended_by TEXT
);
CREATE TABLE IF NOT EXISTS participants (
 id TEXT PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
 name TEXT NOT NULL, token_hash BLOB NOT NULL UNIQUE, joined_at INTEGER NOT NULL,
 UNIQUE(room_id, id)
);
CREATE TABLE IF NOT EXISTS invites (
 id TEXT PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
 token_hash BLOB NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS messages (
 id TEXT PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
 sequence INTEGER NOT NULL, sender_id TEXT NOT NULL, recipient_id TEXT,
 kind TEXT NOT NULL, body TEXT NOT NULL, reply_to TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, UNIQUE(room_id, sequence),
 FOREIGN KEY(room_id, sender_id) REFERENCES participants(room_id, id),
 FOREIGN KEY(room_id, recipient_id) REFERENCES participants(room_id, id)
);
CREATE INDEX IF NOT EXISTS messages_after ON messages(room_id, sequence);
CREATE INDEX IF NOT EXISTS messages_recipient_after ON messages(room_id, recipient_id, sequence);
CREATE INDEX IF NOT EXISTS participants_room ON participants(room_id, joined_at, id);
CREATE INDEX IF NOT EXISTS rooms_expiry ON rooms(expires_at);
```

Validate only `TTL > 0` and `max == nil || *max > 0`; pass `nil` directly into the insert. Scan through `sql.NullInt64`, assigning `nil` or a newly allocated `int` in `getRoom`.

- [ ] **Step 4: Run the store package**

Run: `go test ./internal/store -count=1`

Expected: PASS after updating existing fixtures from `MaxParticipants: 2` to `MaxParticipants: intPtr(2)` and deleting assertions that enforce the obsolete two-person limit.

- [ ] **Step 5: Commit the clean cutoff**

```bash
git add internal/model/model.go internal/store/store.go internal/store/sqlite.go internal/store/sqlite_test.go
git commit -m "feat: replace room schema for multiple participants"
```

---

### Task 2: Make Invites Reusable and Capacity Claims Atomic

**Files:**
- Modify: `internal/store/sqlite.go:147-216`
- Test: `internal/store/sqlite_test.go`

**Interfaces:**
- Consumes: nullable `Room.MaxParticipants` and claim transaction from Task 1.
- Produces: reusable `ClaimInvite`; distinct participant/token per success; `ErrRoomFull`, `ErrRoomExpired`, and `ErrRoomClosed` at valid boundaries.

- [ ] **Step 1: Write reusable and concurrent claim tests**

Add tests with duplicate display names and distinct IDs/tokens; five successful claims in an unlimited room; a capacity-three room where eight goroutines race for two remaining slots; and reuse after full, expiry, and completion:

```go
func TestReusableInviteCreatesDistinctParticipants(t *testing.T) {}
func TestUnlimitedRoomAcceptsSeveralParticipants(t *testing.T) {}
func TestConcurrentClaimsStopExactlyAtCapacity(t *testing.T) {}
func TestReusableInviteRejectsFullExpiredAndCompletedRooms(t *testing.T) {}
```

For the race test, count exactly two successes plus six `ErrRoomFull`, then assert `Participants` has exactly three members. For completion, close with the creator and expect `ErrRoomClosed` rather than a consumed-invite error.

- [ ] **Step 2: Verify failure against one-use claims**

Run: `go test ./internal/store -run 'Test(ReusableInvite|UnlimitedRoom|ConcurrentClaims)' -count=1`

Expected: FAIL because `claimed_at` logic still consumes the invite and capacity/status are not evaluated for every reuse.

- [ ] **Step 3: Implement transactional reusable claims and participant listing**

Within the existing immediate transaction: select invite room by hash; load room; check expiry and `status == "done"`; count participants; if capacity is non-nil and count is at/above it, return `ErrRoomFull`; generate a fresh participant ID/token; insert membership; set status to `active`; commit. Delete every read/write of claim state.

Add:

```go
func (s *sqliteStore) Participants(ctx context.Context, roomID string) ([]model.Participant, error)
```

Order by `joined_at, id`, return no tokens, and call `GetRoom` first so expiry remains inaccessible.

- [ ] **Step 4: Run concurrency repeatedly and with the race detector**

Run: `go test -race ./internal/store -run 'Test(ReusableInvite|UnlimitedRoom|ConcurrentClaims)' -count=20`

Expected: PASS on every iteration; capacity-three membership count is always exactly three.

- [ ] **Step 5: Commit reusable membership**

```bash
git add internal/store/store.go internal/store/sqlite.go internal/store/sqlite_test.go
git commit -m "feat: make room invites reusable"
```

---

### Task 3: Persist Directed Messages and Enforce Recipient Membership

**Files:**
- Modify: `internal/model/model.go`
- Modify: `internal/store/store.go`
- Modify: `internal/store/sqlite.go:243-326`
- Test: `internal/store/sqlite_test.go`

**Interfaces:**
- Consumes: `messages.recipient_id` and membership foreign key from Task 1.
- Produces: `Message.To`, `AppendParams.To`, recipient validation, and recipient-sensitive idempotency.

- [ ] **Step 1: Add append/addressing tests**

Create two rooms and at least three participants in the first. Assert broadcast `To == ""`; private append returns the recipient ID; unknown and second-room IDs return `ErrInvalid`; no message or sequence is consumed on rejection; retry with same ID but changed `To` returns `ErrConflict`.

```go
func TestAppendBroadcastAndPrivateRecipient(t *testing.T) {}
func TestAppendRejectsUnknownAndCrossRoomRecipient(t *testing.T) {}
func TestAppendIdempotencyIncludesRecipient(t *testing.T) {}
```

- [ ] **Step 2: Run and observe missing recipient behavior**

Run: `go test ./internal/store -run 'TestAppend(Broadcast|Rejects|Idempotency)' -count=1`

Expected: FAIL because append types and SQL do not accept or validate a recipient.

- [ ] **Step 3: Add recipient validation and storage**

Before allocating a sequence, when `p.To != ""`, query:

```sql
SELECT 1 FROM participants WHERE room_id=? AND id=?
```

Map no row to `ErrInvalid` (one stable safe error for unknown and cross-room). Insert nullable recipient with `nullIfEmpty(p.To)`. Select it with `COALESCE(m.recipient_id,'')` in `findMessage`; include `existing.To == retry.To` in `sameMessage`. A `done` event always stores `recipient_id = NULL`.

- [ ] **Step 4: Run store tests**

Run: `go test ./internal/store -count=1`

Expected: PASS, including existing sender foreign-key and idempotency tests.

- [ ] **Step 5: Commit directed persistence**

```bash
git add internal/model/model.go internal/store/store.go internal/store/sqlite.go internal/store/sqlite_test.go
git commit -m "feat: add private message recipients"
```

---

### Task 4: Implement Participant-visible History and Wait Cursor Scanning

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/sqlite.go:327-354`
- Test: `internal/store/sqlite_test.go`

**Interfaces:**
- Produces: `VisibleMessages` and `MessagesAfter(ctx, roomID, participantID, after, limit, skipSelf)` from the canonical interface.
- Guarantees: visible history includes self; wait excludes ordinary self messages; both advance through hidden/self rows; done is always visible.

- [ ] **Step 1: Write a sequence-gap visibility matrix**

Append in order: Alice broadcast (1), Alice→Bob private (2), Bob→Alice private (3), Carol→Bob private (4), Carol broadcast (5), Alice broadcast (6), done by Bob (7). Assert:

```go
func TestMessagesAfterReturnsOnlyParticipantVisibleHistory(t *testing.T) {
	// Alice sees 1,2,3,5,6,7; Bob sees all; Carol sees 1,5,6,7.
}

func TestWaitScanSkipsSelfAndInvisibleEventsWhileAdvancingCursor(t *testing.T) {
	// Alice skipSelf=true from 0 returns Bob's sequence 3, Cursor >= 3.
	// Carol from 0 returns Alice's broadcast 1, then from 1 skips 2,3,4
	// and returns 5 with Cursor 5. A scan containing only hidden/self rows
	// returns no message and Cursor at the highest examined sequence.
}

func TestDoneIsVisibleAndNeverSelfFiltered(t *testing.T) {}
```

- [ ] **Step 2: Verify old room-wide reads fail privacy assertions**

Run: `go test ./internal/store -run 'Test(MessagesAfter|WaitScan|DoneIsVisible)' -count=1`

Expected: FAIL because the old query neither authenticates visibility nor exposes scan progress.

- [ ] **Step 3: Implement bounded scan semantics**

Read rows in ascending sequence after `after`. Visibility predicate:

```sql
m.kind = 'done'
OR m.recipient_id IS NULL
OR m.sender_id = ?
OR m.recipient_id = ?
```

Apply `skipSelf` in Go only to ordinary `kind == "message" && sender_id == participantID`; still update `VisibleMessages.Cursor` before skipping. Continue scanning until `limit` visible results, no more rows, or sequence 1000. Do not use SQL `LIMIT limit` before visibility/self filtering; fetch in bounded pages (for example 100 rows) so hidden rows cannot falsely terminate a wait scan.

- [ ] **Step 4: Run focused and full store verification**

Run: `go test -race ./internal/store -count=1`

Expected: PASS with no private payload returned to a third participant and cursor gaps preserved.

- [ ] **Step 5: Commit visibility semantics**

```bash
git add internal/store/store.go internal/store/sqlite.go internal/store/sqlite_test.go
git commit -m "feat: enforce participant message visibility"
```

---

### Task 5: Publish the `/v2` HTTP Contract and Correct Long Polling

**Files:**
- Modify after reconciling concurrent work: `internal/relay/handler.go`
- Modify after reconciling concurrent work: `internal/relay/handler_test.go`

**Interfaces:**
- Consumes: store capacity, participants, recipient, and `VisibleMessages` APIs.
- Produces: `/v2` request/response behavior; create `max_participants`; send `to`; status membership; read `{messages,cursor}`; wait scan cursor.

- [ ] **Step 1: Add HTTP contract tests before touching handlers**

Add reviewer-sized subtests covering:

```go
func TestV2CreateSupportsUnlimitedAndOptionalCapacity(t *testing.T) {}
func TestReusableInviteAndRoomMembershipStatus(t *testing.T) {}
func TestDirectedMessageValidationAndVisibility(t *testing.T) {}
func TestWaitSkipsSelfAndHiddenEventsAndAdvancesCursor(t *testing.T) {}
func TestDoneWakesEveryParticipantWaiter(t *testing.T) {}
func TestV1IsUnsupportedAfterCleanCutoff(t *testing.T) {}
```

Decode status and assert participant IDs/names but no token fields. Read as each of Alice/Bob/Carol. For wait, have Alice send two messages while waiting herself; response must not echo them and must expose `sequence` equal to the highest scanned sequence on timeout. Start three waiters before done and assert all return `status:"done"`.

- [ ] **Step 2: Run focused relay tests**

Run: `go test ./internal/relay -run 'Test(V2|ReusableInvite|DirectedMessage|WaitSkips|DoneWakes)' -count=1`

Expected: FAIL on missing `/v2`, missing fields/membership, and old room-wide wait behavior.

- [ ] **Step 3: Switch routes and request shapes**

Register only `/v2/...`; change unsupported-version detection accordingly. Decode create with `MaxParticipants *int`; reject non-positive values before store call. Decode send with `To string`; pass authenticated participant and `To` to `Append`. Return safe `invalid_recipient`/`"The recipient is not a participant in this room."` for `ErrInvalid` from addressed append without revealing whether the ID exists elsewhere.

- [ ] **Step 4: Return membership and authenticated history**

After authorization, call `GetRoom` plus `Participants`, assign `room.Participants`, and return it. For history call `MessagesAfter(..., p.ID, after, 1000, false)` and respond:

```json
{"messages": [], "cursor": 17}
```

Never include hidden message metadata or bodies.

- [ ] **Step 5: Rework wait around scan progress without busy loops**

Track a local `scanAfter`, initialized from the request. Call `MessagesAfter(..., p.ID, scanAfter, 1, true)`. If visible message/done exists, return it. If only skipped rows were scanned, set `scanAfter = result.Cursor`, subscribe, recheck, then block. Timeout response must include `sequence: scanAfter`; notification remains room-wide so each waiter wakes and independently applies visibility. Keep the subscribe-before-recheck race protection and cancellation cleanup.

- [ ] **Step 6: Run relay tests with race detection**

Run: `go test -race ./internal/relay -count=1`

Expected: PASS, including existing limits, shutdown, logging, strict JSON, and wait registry tests updated to `/v2`.

- [ ] **Step 7: Commit the HTTP cutoff without staging concurrent files wholesale**

```bash
git diff -- internal/relay/handler.go internal/relay/handler_test.go
git add -p internal/relay/handler.go internal/relay/handler_test.go
git commit -m "feat: publish multi-participant relay v2"
```

---

### Task 6: Update the Go HTTP Client and Cursor-bearing Results

**Files:**
- Modify: `internal/client/client.go`
- Modify: `internal/client/client_test.go`

**Interfaces:**
- Produces: `CreateRoom(..., maxParticipants *int)`, `Send(..., to string)`, `ReadResult`, and existing `WaitResult.Sequence` as scan cursor.

- [ ] **Step 1: Pin client request and response tests**

```go
type ReadResult struct {
	Messages []model.Message `json:"messages"`
	Cursor   int64           `json:"cursor"`
}

func TestCreateSendsNullableMaximumToV2(t *testing.T) {}
func TestSendIncludesOptionalRecipientToV2(t *testing.T) {}
func TestReadAndWaitPreserveServerScanCursor(t *testing.T) {}
```

Assert omitted capacity serializes as no `max_participants` field, explicit 4 as `4`, broadcast `to` as omitted/empty consistently, and every endpoint path starts `/v2`.

- [ ] **Step 2: Run failing client tests**

Run: `go test ./internal/client -run 'Test(CreateSends|SendIncludes|ReadAndWait)' -count=1`

Expected: FAIL on signatures, `/v1`, and absent read cursor.

- [ ] **Step 3: Implement the exact client contract**

Use:

```go
func (c Client) CreateRoom(ctx context.Context, name, creator string, ttl time.Duration, maxParticipants *int) (CreateResult, error)
func (c Client) Send(ctx context.Context, room, id, body, replyTo, to string) (model.Message, error)
func (c Client) Read(ctx context.Context, room string, after int64) (ReadResult, error)
```

Build create input as `map[string]any` and add `max_participants` only when non-nil. Build send input similarly and add `to` only when non-empty. Replace every `/v1` path with `/v2`.

- [ ] **Step 4: Run client tests**

Run: `go test -race ./internal/client -count=1`

Expected: PASS, retaining stable generated message IDs and bounded retries.

- [ ] **Step 5: Commit client v2**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "feat: add multi-participant relay client"
```

---

### Task 7: Store Credentials Per Room and Participant

**Files:**
- Modify: `internal/localconfig/config.go`
- Modify: `internal/localconfig/config_test.go`

**Interfaces:**
- Produces:

```go
func (s Store) LoadRoom(handle, participantID string) (model.RoomCredential, error)
func (s Store) ListParticipants(roomID string) ([]model.RoomCredential, error)
func (s Store) AdvanceCursor(roomID, participantID string, sequence int64) error
func (s Store) RemoveRoom(roomID, participantID string) error
```

- [ ] **Step 1: Write nested-layout and selection tests**

Assert two credentials for one room coexist at `rooms/room-1/p1.json` and `p2.json`; same participant save preserves its maximum cursor; locks are `rooms/room-1/p1.lock`; exact participant selection works; empty participant selects only when one identity exists; ambiguity returns `ErrRoomAmbiguous`; errors can be formatted with sorted `ID (Name)` choices; traversal is rejected for both IDs.

```go
func TestRoomCredentialsAreStoredPerParticipant(t *testing.T) {}
func TestLoadRoomRequiresParticipantWhenIdentityIsAmbiguous(t *testing.T) {}
func TestParticipantCursorAdvancesIndependently(t *testing.T) {}
func TestRoomAndParticipantTraversalAreRejected(t *testing.T) {}
```

- [ ] **Step 2: Verify the flat storage fails**

Run: `go test ./internal/localconfig -run 'Test(RoomCredentials|LoadRoomRequires|ParticipantCursor|RoomAndParticipant)' -count=1`

Expected: FAIL because saving the second participant overwrites the first flat file.

- [ ] **Step 3: Implement nested paths and participant-aware locks**

Use `rooms/<roomID>/<participantID>.json`; validate both path components with one `validID`; sort `ListRooms` by room ID then participant ID; make `ListParticipants(roomID)` read only that directory. `LoadRoom(handle, participantID)` first resolves room ID/name, then exact participant ID, then the exactly-one rule. Never resolve by participant name.

- [ ] **Step 4: Return actionable ambiguity details**

Add:

```go
type AmbiguousIdentityError struct {
	RoomID     string
	Identities []model.RoomCredential
}
func (e *AmbiguousIdentityError) Error() string
func (e *AmbiguousIdentityError) Is(target error) bool { return target == ErrRoomAmbiguous }
```

The error string lists sorted `participant_id (participant name)` values; add `ParticipantName string` to `RoomCredential` so selection output does not require a network call.

- [ ] **Step 5: Run local configuration tests**

Run: `go test -race ./internal/localconfig -count=1`

Expected: PASS with mode `0700` on root/rooms/room and `0600` on each credential.

- [ ] **Step 6: Commit local identity storage**

```bash
git add internal/model/model.go internal/localconfig/config.go internal/localconfig/config_test.go
git commit -m "feat: store credentials per participant"
```

---

### Task 8: Add CLI Capacity, Addressing, and Participant Selection

**Files:**
- Modify: `internal/cli/commands.go`
- Modify: `internal/cli/flags.go`
- Modify: `internal/cli/help.go`
- Modify: `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: participant-aware local config and client APIs.
- Produces: create `--max-participants`, send `--to`, and `--participant` on send/read/wait/done/status.

- [ ] **Step 1: Add CLI parsing and lifecycle tests**

```go
func TestCreateMaxParticipantsAndSendToAreForwarded(t *testing.T) {}
func TestEveryRoomCommandAcceptsParticipantBeforeOrAfterArguments(t *testing.T) {}
func TestCLIRefusesAmbiguousIdentityAndListsParticipants(t *testing.T) {}
func TestParticipantCursorsRemainIndependent(t *testing.T) {}
```

Exercise `--participant p1`, `--participant=p1`, flags before/after room/message, duplicate display names, JSON errors, and human errors containing both IDs/names but no tokens.

- [ ] **Step 2: Run the new CLI tests**

Run: `go test ./internal/cli -run 'Test(CreateMax|EveryRoom|CLIRefuses|ParticipantCursors)' -count=1`

Expected: FAIL because flags and nested resolution do not exist.

- [ ] **Step 3: Thread selection through one room resolver**

Refactor `room` to accept parsed participant selection:

```go
func (r runner) room(args []string, participantID string, minimum, maximum int) (model.RoomCredential, []string, error)
```

Register `--participant` on send/read/wait/done/status and call `LoadRoom(handle, participantID)`. Pass both room and participant ID to `AdvanceCursor`. On ambiguity, print the typed error's sorted identities. Do not duplicate resolver logic per command.

- [ ] **Step 4: Add capacity/addressing flags and output identity**

Create uses a custom optional positive integer value so omission remains `nil` and explicit zero is rejected; call client `CreateRoom(..., max)`. Send adds `--to PARTICIPANT_ID`. Human create/join output includes `Participant: <name> (<id>)`; JSON already includes participant. Status prints membership IDs and names.

- [ ] **Step 5: Run CLI and dependent tests**

Run: `go test -race ./internal/cli ./internal/localconfig ./internal/client -count=1`

Expected: PASS; no credential token appears in ordinary or ambiguity output.

- [ ] **Step 6: Commit CLI selection**

```bash
git add internal/cli/commands.go internal/cli/flags.go internal/cli/help.go internal/cli/cli_test.go
git commit -m "feat: select room participants in CLI"
```

---

### Task 9: Bind Participant Identity to Each MCP Process

**Files:**
- Modify: `internal/mcpserver/server.go`
- Modify: `internal/mcpserver/server_test.go`

**Interfaces:**
- Produces process-local `bindings map[string]string`, guarded by `sync.RWMutex`; optional `participant_id` on all existing room inputs; selected identity in useful outputs.

- [ ] **Step 1: Add two-process and restart tests**

Use one `localconfig.Store` root and separate calls to `New(deps)`:

```go
func TestTwoMCPProcessesShareConfigWithoutBorrowingIdentity(t *testing.T) {}
func TestMCPCreateAndJoinBindTheirOwnParticipant(t *testing.T) {}
func TestMCPRestartAutoSelectsExactlyOneIdentity(t *testing.T) {}
func TestMCPRestartRequiresParticipantOnceThenRemembersIt(t *testing.T) {}
func TestMCPParticipantIDIsValidatedForTheSelectedRoom(t *testing.T) {}
```

Have process A create, process B join the same room/config root, then send private messages in both directions. Restart process C: a call without `participant_id` must return a tool error listing identities; a call with Bob's ID binds Bob; the next call without it must remain Bob.

- [ ] **Step 2: Run identity tests and confirm stateless failure**

Run: `go test ./internal/mcpserver -run 'Test(TwoMCP|MCPCreate|MCPRestart|MCPParticipant)' -count=1`

Expected: FAIL because one flat credential is loaded for every process and no session map exists.

- [ ] **Step 3: Add a concurrency-safe service binding resolver**

Construct one pointer service per server:

```go
type service struct {
	deps Dependencies
	mu sync.RWMutex
	bindings map[string]string // room ID -> participant ID
}

func (s *service) credential(handle, participantID string) (model.RoomCredential, error)
func (s *service) bind(roomID, participantID string)
```

Resolve the room first; explicit `participant_id` must load an exact local credential and replace the binding; otherwise use an existing binding; otherwise auto-select exactly one identity. Create/join save `ParticipantName`, then bind immediately. Do not persist session bindings.

- [ ] **Step 4: Add tool fields and identity-bearing outputs**

Add `ParticipantID string 'json:"participant_id,omitempty"'` to room/send/read/wait/done inputs, `MaxParticipants *int` to create, and `To string` to send. Include `participant_id` in read/wait/status wrappers and preserve participant object in create/join. Update every cursor call with participant ID.

- [ ] **Step 5: Update MCP schema and descriptions**

Set MCP implementation version `2.0.0`. Describe reusable invites, unlimited/default capacity, private `to`, status membership discovery, one-time participant disambiguation, creator sends the concrete task, joiner waits before speaking, no generic greeting, bounded waits only while a response is expected, and no acknowledgement loops.

- [ ] **Step 6: Run MCP tests with race detection**

Run: `go test -race ./internal/mcpserver -count=1`

Expected: PASS, including schema type pinning, cancellation propagation, token secrecy, idempotent send/done, and the shared-root two-process scenario.

- [ ] **Step 7: Commit session identity**

```bash
git add internal/mcpserver/server.go internal/mcpserver/server_test.go
git commit -m "feat: bind participant identity per MCP session"
```

---

### Task 10: Establish Creator-first and Joiner-waits-first Agent Instructions

**Files:**
- Modify after reconciling concurrent work: `internal/relay/handler.go` (`join` copy only)
- Modify after reconciling concurrent work: `internal/relay/handler_test.go`
- Modify after reconciling concurrent work: `integrations/skills/agentline/SKILL.md`
- Test: `internal/relay/handler_test.go`
- Test: `internal/setup/setup_test.go` (embedded skill assertions if present)

**Interfaces:**
- Produces: identical behavioral rules in HTML join page, Markdown join page, shared skill, and MCP descriptions from Task 9.

- [ ] **Step 1: Add exact behavioral-copy assertions**

Assert both join representations and the installed shared skill include these semantic rules (wording may be concise but assertions should target stable phrases): creator sends a concrete task/context/constraints/expected response; reusable invite; joiner waits for creator's task; no generic greeting; status/membership before directed send; ask once for clarification if no task; do not acknowledge acknowledgements; wait again only while a real response is expected; normally creator ends but any participant may end.

```go
func TestJoinInstructionsMakeJoinerWaitForConcreteTask(t *testing.T) {}
func TestJoinInstructionsStopNoTaskAndAcknowledgementLoops(t *testing.T) {}
```

- [ ] **Step 2: Run instruction tests**

Run: `go test ./internal/relay ./internal/setup -run 'Test(JoinInstructions|Skill)' -count=1`

Expected: FAIL because current copy says one-use/two-agent and encourages immediate reply loops.

- [ ] **Step 3: Rewrite the shared skill around asymmetric roles**

Use explicit `## Creator`, `## Joiner`, `## Conversation discipline`, and `## Trust boundary` sections. Directed messages are for one participant; broadcasts share group context/decisions. Keep stable idempotency and untrusted-content rules.

- [ ] **Step 4: Align generated HTML and Markdown join instructions**

Change “one-use invite” to “reusable invite”; after join, instruct `agentline wait --timeout 60s`; explicitly prohibit a greeting before the task arrives. Keep HTML escaping, shell quoting, no-store/referrer headers, and non-destructive GET behavior.

- [ ] **Step 5: Run prompt/setup/relay tests**

Run: `go test -race ./internal/relay ./internal/setup -count=1`

Expected: PASS with creator-first, joiner-waits-first, no-task, and anti-acknowledgement guidance covered.

- [ ] **Step 6: Commit only reconciled instruction hunks**

```bash
git diff -- internal/relay/handler.go internal/relay/handler_test.go integrations/skills/agentline/SKILL.md
git add -p internal/relay/handler.go internal/relay/handler_test.go integrations/skills/agentline/SKILL.md internal/setup/setup_test.go
git commit -m "docs: teach multi-participant conversation discipline"
```

---

### Task 11: Rewrite Protocol, User Documentation, and Version Markers

**Files:**
- Modify: `README.md`
- Modify: `docs/design.md`
- Modify: `docs/protocol.md`
- Modify: `docs/integrations.md`
- Modify: `docs/deployment.md`
- Modify: `internal/mcpserver/server.go` (only if version was not committed in Task 9)

**Interfaces:**
- Documents: HTTP `/v2`, release `v0.2.0`, MCP `2.0.0`, exact JSON/CLI/MCP contracts and clean-cutoff operations.

- [ ] **Step 1: Add a documentation contract checklist before prose edits**

Record and satisfy these searchable terms in the changed docs: `/v2`, `v0.2.0`, `max_participants`, JSON `null`, `to`, reusable invite, duplicate names, participant IDs, private visibility, room-global cursor gaps, per-participant credential path, `--participant`, MCP `participant_id`, clean database cutoff, and three-participant smoke test.

- [ ] **Step 2: Rewrite `docs/protocol.md` as the authoritative v2 contract**

Include exact create payloads for unlimited and capped rooms; status response with participants; broadcast/private message envelopes; read response with `cursor`; wait timeout with `sequence`; visibility truth table; reusable claim state machine; new schema columns/indexes; errors for capacity/recipient/expiry/completion; and explicitly state `/v1` is unsupported.

- [ ] **Step 3: Update product/integration docs without compatibility prose**

Replace “two coding agents”, “one peer”, “one-use”, and “max two” throughout README/design/integrations. Explain participant selection independently for CLI and MCP and provide commands:

```console
agentline create --max-participants 3 --name creator
agentline status ROOM --participant CREATOR_ID
agentline send ROOM --participant CREATOR_ID --to JOINER_ID "Private task"
agentline wait ROOM --participant JOINER_ID --timeout 60s
```

- [ ] **Step 4: Update release/deployment status**

Set the planned release to `v0.2.0`, protocol to `/v2`, MCP implementation to `2.0.0`, and document that the old Fly SQLite file must be deleted—not migrated—before startup. Do not claim the deployment or smoke passed yet; leave evidence fields explicitly marked “pending execution” rather than fabricating results.

- [ ] **Step 5: Scan for stale contract language**

Run:

```bash
rg -n 'one-use|two-agent|two coding agents|both MVP participants|set to two|/v1|v0\.1\.1' README.md docs integrations/skills/agentline internal/mcpserver/server.go internal/relay/handler.go
```

Expected: no stale contract claims; `/v1` may appear only in an explicit “unsupported after cutoff” statement/test.

- [ ] **Step 6: Commit documentation/version updates**

```bash
git add README.md docs/design.md docs/protocol.md docs/integrations.md docs/deployment.md internal/mcpserver/server.go
git commit -m "docs: publish multi-participant protocol v2"
```

---

### Task 12: Run the Complete Local Cutoff Verification

**Files:**
- Modify only if failures expose a defect: the smallest owning implementation/test file from Tasks 1–11.
- Do not modify: unrelated concurrent website, integration adapter, channel, or setup native-test work.

**Interfaces:**
- Verifies all automated acceptance criteria before destructive deployment.

- [ ] **Step 1: Delete only known local development databases**

First list candidates and confirm they are inside this worktree:

```bash
find . -maxdepth 3 -type f \( -name 'agentline.db' -o -name 'relay.db' -o -name '*.db-wal' -o -name '*.db-shm' \) -print
```

Expected: only disposable development artifacts. Remove those exact reviewed paths; never glob outside the worktree and never remove test fixtures or the Fly volume.

- [ ] **Step 2: Format and inspect scope**

Run:

```bash
gofmt -w internal/model/model.go internal/store/store.go internal/store/sqlite.go internal/store/sqlite_test.go internal/client/client.go internal/client/client_test.go internal/localconfig/config.go internal/localconfig/config_test.go internal/cli/commands.go internal/cli/flags.go internal/cli/help.go internal/cli/cli_test.go internal/mcpserver/server.go internal/mcpserver/server_test.go
git status --short
git diff --check
```

Expected: no whitespace errors and no accidental edits/deletions to concurrent files.

- [ ] **Step 3: Run targeted acceptance tests**

Run:

```bash
go test -race ./internal/store -run 'Test(CreateRoomSupports|ReusableInvite|UnlimitedRoom|ConcurrentClaims|AppendBroadcast|AppendRejects|MessagesAfter|WaitScan|DoneIsVisible)' -count=10
go test -race ./internal/relay -run 'Test(V2|ReusableInvite|DirectedMessage|WaitSkips|DoneWakes)' -count=10
go test -race ./internal/localconfig -run 'Test(RoomCredentials|LoadRoomRequires|ParticipantCursor)' -count=10
go test -race ./internal/mcpserver -run 'Test(TwoMCP|MCPCreate|MCPRestart|MCPParticipant)' -count=10
go test -race ./internal/cli -run 'Test(CreateMax|EveryRoom|CLIRefuses|ParticipantCursors)' -count=10
```

Expected: PASS on all repeated runs; no capacity race or identity borrowing.

- [ ] **Step 4: Run the repository gate**

Run:

```bash
go vet ./...
test -z "$(gofmt -l .)"
go test -race ./...
go build ./cmd/agentline
```

Expected: all commands exit 0. If the concurrent untracked `internal/channel/server_test.go` still references unavailable implementation, report that pre-existing external blocker separately; do not delete, skip, or rewrite the test to make the gate green.

- [ ] **Step 5: Perform a local three-process smoke**

Start `go run ./cmd/agentline server --listen 127.0.0.1:18080 --public-url http://127.0.0.1:18080 --data "$TMPDIR/agentline-v2-smoke.db"`, use three isolated config roots/processes, and verify: reusable join; status lists three IDs; broadcast reaches both peers but sender wait skips itself; Alice→Bob private is absent from Carol read/wait; cursor sequence advances across Carol's hidden event; done wakes all waits; post-done send fails; history remains readable.

Expected: each assertion succeeds against real HTTP, not only in-process store tests. Remove the temporary database and stop the server afterward.

- [ ] **Step 6: Commit any test-driven corrections, one owner at a time**

```bash
git add -p internal/store/sqlite.go internal/store/sqlite_test.go
git commit -m "fix: advance cursors across hidden messages"
```

Expected: substitute the exact owning file pair and precise behavior message when a different test exposes a defect; no omnibus cleanup commit and no unrelated staged hunks.

---

### Task 13: Wipe Fly Data, Deploy, and Run the Live Three-participant Smoke

**Files:**
- Modify after successful execution: `docs/deployment.md` (actual date, release/image, machine, wipe, and smoke evidence)

**Interfaces:**
- Consumes: locally green reviewed `v0.2.0` build and explicit operator approval for destructive/shared actions.
- Produces: empty production v2 database and evidence that `agentline.dev` satisfies the new contract.

- [ ] **Step 1: Confirm authorization and identify exact Fly resources**

Run only after explicit operator approval:

```bash
fly status --app agentline
fly machines list --app agentline
fly volumes list --app agentline
```

Expected: one intended Machine and the `agentline_data` volume in `arn`. Record exact IDs. Abort if names/region/topology differ.

- [ ] **Step 2: Stop the service before deleting SQLite**

```bash
MACHINE_ID="$(fly machines list --app agentline --json | jq -r '.[0].id')"
test -n "$MACHINE_ID" && test "$MACHINE_ID" != "null"
fly machine stop "$MACHINE_ID" --app agentline
fly status --app agentline
```

Expected: target Machine is stopped; no process has the SQLite WAL open.

- [ ] **Step 3: Wipe only Agentline SQLite files from the mounted volume**

Start/attach a maintenance Machine using the same volume and image or use `fly ssh console` after a controlled start, inspect `/data`, then remove exactly:

```sh
rm -f /data/agentline.db /data/agentline.db-wal /data/agentline.db-shm
```

Expected: those files are absent and unrelated volume content is untouched. Do not use `rm -rf /data`.

- [ ] **Step 4: Deploy the reviewed cutoff build**

```bash
fly deploy --app agentline
fly scale count 1 --app agentline
fly status --app agentline
fly checks list --app agentline
curl -fsS https://agentline.dev/healthz
```

Expected: deploy succeeds, one healthy Machine runs, health returns `{"status":"ok"}`, and startup created the clean schema.

- [ ] **Step 5: Verify the hard protocol cutoff**

```bash
curl -sS -o /tmp/agentline-v1.out -w '%{http_code}\n' -X POST https://agentline.dev/v1/rooms
curl -sS -o /tmp/agentline-v2.out -w '%{http_code}\n' -H 'Content-Type: application/json' -d '{"name":"live-smoke","creator_name":"alice","ttl_seconds":3600}' https://agentline.dev/v2/rooms
```

Expected: `/v1` returns unsupported/not found; `/v2` returns 201 with `max_participants:null`. Treat returned invite and participant tokens as secrets; do not paste them into docs/logs.

- [ ] **Step 6: Run a live three-independent-participant scenario**

Use three separate temporary `AGENTLINE_CONFIG` roots or three MCP processes. Alice creates unlimited room; Bob and Carol claim the same invite; status lists all three distinct IDs. Verify Alice broadcast reaches Bob and Carol while Alice's bounded wait does not echo it. Send Alice→Bob private and assert Bob receives it, Carol's read and wait do not, and Carol's persisted cursor advances past its sequence. Send Carol broadcast and assert both peers receive it. Start bounded waits for all three, end from Alice, and assert each returns `done`; assert later writes fail and authenticated history remains readable until expiry.

Expected: every behavior passes against `https://agentline.dev` with no private payload visible to Carol.

- [ ] **Step 7: Inspect production logs for secrecy and errors**

```bash
fly logs --app agentline
```

Expected: routes/status are observable; invite tokens, bearer tokens, full invite URLs, message bodies, and private recipient contents are absent; no SQLite lock/capacity errors occurred.

- [ ] **Step 8: Record factual deployment evidence and commit**

Update `docs/deployment.md` with UTC timestamp, release `v0.2.0`, Machine/volume identifiers as appropriate, exact wipe/deploy commands, and pass/fail results for reusable join, broadcast, private isolation, cursor advancement, bounded waits, and completion. Never record tokens or message bodies.

```bash
git add docs/deployment.md
git commit -m "docs: record multi-participant Fly deployment"
```

- [ ] **Step 9: Tag/push only after review and authorization**

```bash
git status --short
git log --oneline --decorate -15
git tag v0.2.0
BRANCH="$(git branch --show-current)"
test -n "$BRANCH"
git push origin "$BRANCH"
git push origin v0.2.0
```

Expected: clean intended tree, reviewed commits only, release workflow passes `go test -race ./...`, `go vet ./...`, and GoReleaser publishes verified `v0.2.0` artifacts. If the deployment happens before tagging by repository convention, retain the same evidence and tag only after the final docs commit is reviewed.

---

## Final Acceptance Checklist

- [ ] Unlimited room admits at least creator plus four reusable claims.
- [ ] Optional positive capacity is enforced exactly under concurrent claims; omission persists SQL `NULL`; zero/negative fails.
- [ ] Invite reuse fails only for full, expired, completed, invalid rooms—not because another participant claimed first.
- [ ] Status returns participant IDs and duplicate-capable names without credentials.
- [ ] Broadcast/private recipient validation and visibility are correct for three participants and across rooms.
- [ ] Explicit history includes own visible messages; waits skip ordinary self messages; done remains room-wide.
- [ ] Read/wait cursors advance across private gaps and self-authored events without rescans.
- [ ] Credentials coexist at `rooms/<room>/<participant>.json`; cursor and lock scope are participant-specific.
- [ ] Two MCP processes sharing one root retain separate bindings; restart ambiguity requires `participant_id` once.
- [ ] Every CLI room operation supports `--participant`; ambiguity lists IDs/names and never tokens.
- [ ] Creator/joiner prompts enforce concrete-task-first behavior, bounded waits, one clarification, and no acknowledgement loops.
- [ ] `/v2`, MCP `2.0.0`, and release `v0.2.0` are documented; `/v1` and old databases have no compatibility path.
- [ ] `go vet`, formatting, `go test -race ./...`, build, and local three-process smoke pass.
- [ ] Fly data is intentionally wiped before deployment, health is green, and live three-participant privacy/completion smoke evidence is recorded.
