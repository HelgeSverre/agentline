# Task 3 Report

## Outcome

- Added `Message.To` and `AppendParams.To` for directed-message addressing.
- Persisted private recipients as `messages.recipient_id`, while broadcasts and `done` events store `NULL`.
- Validated non-empty recipients against membership in the message room before sequence allocation; unknown and cross-room recipients return `ErrInvalid` without consuming a message or sequence.
- Included the recipient in idempotency comparisons and loaded it in direct lookups and message history.
- Added direct store-contract coverage for ordered participant listing, addressing the Task 2 minor.

## TDD and verification

- RED: `go test ./internal/store -run 'TestAppend(Broadcast|Rejects|Idempotency)' -count=1` failed to compile because `Message.To` and `AppendParams.To` did not exist.
- GREEN: the same targeted command passed after the minimal model/store/SQLite changes.
- Store regression: `go test ./internal/store -count=1` passed.
- Focused race/repetition: `go test -race ./internal/store -run 'Test(AppendBroadcast|AppendRejects|AppendIdempotency|ParticipantsLists)' -count=10` passed.
- Repository suite: `go test ./... -count=1` remains blocked at the intentional clean cutoff by the pre-existing `internal/relay/handler.go:184` scalar `MaxParticipants: 2` caller; store tests pass. The relay was not changed because this task explicitly excludes relay work.
- Formatting/whitespace: `gofmt` applied and `git diff --check` passed.

## Commit

- `3a10e07 feat: add private message recipients`

## Self-review

- Recipient membership is checked with the room-scoped `(room_id, id)` pair before reading or incrementing `next_sequence`.
- Both rejected recipient classes expose the same safe `ErrInvalid` result.
- Broadcast and completion rows are verified as SQL `NULL`; private recipients round-trip through append, retry lookup, and history.
- Recipient changes conflict on message-ID retry, while exact directed retries remain idempotent.
- Review found no production-code correctness, security, or performance issues. A participant-listing test initially assumed creator-first ordering when timestamps tie; it was corrected to assert the implementation contract (`joined_at, id`) and passed repeated race runs.
- The diff is limited to the requested model/store surfaces and tests; relay/docs `ErrInviteClaimed` work remains untouched.

## Concerns

- Full-repository compilation is intentionally not green until the planned relay caller updates `MaxParticipants` to a pointer in a later task.
- Directed delivery/filtering is not part of this persistence task; `MessagesAfter` continues returning room history with recipient metadata for later relay/client enforcement.
