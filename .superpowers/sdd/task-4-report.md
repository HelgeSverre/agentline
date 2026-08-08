# Task 4 Report

## Status

Implemented and committed participant-scoped history and bounded wait scanning.

## Changes

- Added `VisibleMessages`, carrying both returned messages and the highest examined sequence cursor.
- Changed `Store.MessagesAfter` to require a room participant, support `skipSelf`, and return scan progress.
- Enforced participant membership and private-message visibility.
- Scanned raw rows in bounded 100-row pages so hidden and self-authored rows advance the cursor without prematurely ending a wait scan.
- Kept `done` events visible, including when authored by the waiting participant.
- Updated relay history/wait callers to pass the authenticated participant and preserve scan cursor progress.
- Added the sequence-gap matrix, wait self/hidden-row scans, empty scan cursor coverage, and done visibility coverage.
- Fixed the relay's existing `MaxParticipants` pointer call site so the changed branch compiles.

## Commit

- `bd2d4ec feat: enforce participant message visibility`

## TDD and Verification

- RED: `go test ./internal/store -run 'Test(MessagesAfter|WaitScan|DoneIsVisible)' -count=1` failed because the old API returned room-wide `[]model.Message` and had no participant, self-skip, or cursor support.
- GREEN: the same focused command passed.
- `go test -race ./internal/store -count=1` passed.
- `go vet ./internal/store` passed.
- `git diff --check` passed before the implementation commit.
- `go test ./... -count=1` was also run. All packages except `internal/relay` passed. Existing relay lifecycle expectations conflict with earlier multi-participant changes (`invite_already_claimed` versus `room_full`), and an old wait test expects a participant to receive its own ordinary message; Task 4 intentionally excludes ordinary self messages from wait.

## Concerns / Decisions

The brief's concrete matrix contradicts its canonical guarantees in two places: it says visible history includes self but omits Carol's own private sequence 4, and it says wait excludes ordinary self messages but expects Carol's own broadcast sequence 5. The implementation follows the explicit visibility predicate and guarantees: Carol sees sequence 4 in history, while Carol's wait skips sequences 4 and 5 and returns Alice's sequence 6. The focused store suite verifies these semantics.

## Review Fixes (2026-08-08)

### Outcome

Resolved the Task 4 review findings and brought the relay package onto the participant-scoped wait semantics.

### Fixes

- Fixed the subscribe/recheck race in `internal/relay/handler.go`: a visible event found by the post-subscribe recheck is now rendered immediately before returning. Its cursor can no longer advance past an event that was never returned.
- Updated the relay lifecycle expectation to the current authoritative capacity result (`room_full`) without implementing Task 5 reusable-invite endpoint behavior.
- Updated the old wait wake test to use a peer-authored event rather than an ordinary self-authored event.
- Added focused relay coverage for ordinary self-event skipping and cursor progress, peer wake, hidden private event non-disclosure/cursor progress, self-authored `done`, and the subscribe/recheck race.
- Added store coverage requiring `MessagesAfter` to reject both unknown participant IDs and participants from another room with `ErrUnauthorized`.

### TDD Evidence

- RED: `go test ./internal/relay -run 'TestWaitReturnsMessageFoundBySubscribeRecheck' -count=1` failed with a timeout response whose cursor had advanced to sequence 1, proving the rechecked message was skipped.
- GREEN: after rendering the rechecked event immediately, the focused regression and full package suites passed.
- The focused store authorization test passed immediately because the implementation already enforced the required behavior; this adds the missing regression coverage rather than changing store production code.

### Verification

- `go test ./internal/store ./internal/relay -count=1` — PASS (`internal/store` 0.752s, `internal/relay` 0.335s).
- `go test -race ./internal/store ./internal/relay -count=1` — PASS (`internal/store` 6.357s, `internal/relay` 1.851s).
- `git diff --check` — PASS.

### Concerns / Scope

- Reusable-invite endpoint contract behavior remains intentionally deferred to Task 5. This fix only aligns the existing full-room relay expectation with the store's current authoritative result.
- The peer wake lifecycle test retains its pre-existing short scheduling delay; the subscribe/recheck boundary itself has a deterministic injected-event regression test.
