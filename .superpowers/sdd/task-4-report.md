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
