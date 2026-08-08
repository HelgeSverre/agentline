# Task 1 Report

## Outcome

Implemented the clean multi-participant SQLite cutoff and nullable room-capacity model. Room creation now accepts unlimited capacity (`nil`) or any positive integer, persists unlimited capacity as SQL `NULL`, returns JSON `null`, and rejects non-positive values. The schema is replaced directly without migration or compatibility SQL.

## Files changed

- `internal/model/model.go` — changed `Room.MaxParticipants` to `*int`.
- `internal/store/store.go` — changed `CreateRoomParams.MaxParticipants` to `*int`.
- `internal/store/sqlite.go` — installed the canonical clean schema, nullable capacity validation/insertion/scanning, and removed invite claim-column reads/writes required by the new schema.
- `internal/store/sqlite_test.go` — replaced the obsolete two-person schema contract with nullable/positive capacity and clean-cutoff schema coverage; updated capped fixtures to pointers.

## TDD evidence

1. Added the new contract tests first.
2. Ran the focused command and observed the expected RED build failure because the production capacity fields were still `int` and could not accept `*int`/`nil`.
3. Implemented the nullable model, exact schema, validation, insertion, and `sql.NullInt64` scan.
4. Ran focused and package tests to GREEN.

## Tests run

- `go test ./internal/store -run 'Test(CreateRoomSupportsUnlimitedAndPositiveCapacity|SchemaIsCleanMultiParticipantCutoff)' -count=1` — PASS.
- `go test ./internal/store -count=1` — PASS.
- `git diff --check` — PASS.
- `go test ./... -count=1` — FAIL at compile time because `internal/relay/handler.go` still supplies the pre-cutoff integer literal `MaxParticipants: 2`; changing relay/API callers belongs to later tasks and was intentionally left outside Task 1's four-file scope. Store tests passed during this run.

## Commit

- Implementation commit: `982c885` (`feat: replace room schema for multiple participants`)

## Self-review

- Reviewed the complete diff for security, correctness, performance, maintainability, and unintended scope.
- Confirmed SQL remains static/parameterized and adds the exact requested constraints and indexes.
- Confirmed `nil` is passed directly to SQLite, capped values round-trip through `sql.NullInt64`, and non-positive capacities are rejected both by Go and the database.
- Confirmed token hashing, fixed expiry, and `waiting_for_peer` creation status remain covered and passing.
- No critical, high, medium, or low findings in the Task 1 diff.

## Concerns

- The repository-wide suite cannot compile until a later relay/API task adapts the existing hard-coded `MaxParticipants: 2` caller to the new pointer contract. Task 1's required store package is green.
- Removing the obsolete invite claim columns necessarily required removing their reads/writes. The resulting reusable-invite foundation is schema-compatible, while comprehensive reusable-invite behavior and concurrency coverage remain assigned to Task 2.
