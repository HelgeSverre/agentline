# Task 2 Report

## Outcome

- Made invite claims reusable with a fresh participant ID and token per successful claim.
- Kept capacity admission inside the existing SQLite immediate transaction.
- Added completed-room rejection with `ErrRoomClosed`.
- Added ordered participant listing after room accessibility validation.

## TDD and verification

- RED: `go test ./internal/store -run 'Test(ReusableInvite|UnlimitedRoom|ConcurrentClaims)' -count=1` failed because `Store.Participants` did not exist. Task 1 already contained reusable claim and transactional capacity logic, so the expected one-use failure was no longer present.
- GREEN: the same targeted command passed.
- Concurrency/race: `go test -race ./internal/store -run 'Test(ReusableInvite|UnlimitedRoom|ConcurrentClaims)' -count=20` passed all 20 iterations.
- Store regression: `go test ./internal/store -count=1` passed.
- Formatting/whitespace: `gofmt` applied and `git diff --check` passed.

## Commit

- `7c36490 feat: make room invites reusable`

## Self-review

- Claim boundary order is expiry, completion, then capacity, producing the requested errors.
- Immediate transactions serialize the participant count and insert, preventing capacity oversubscription.
- Participant listing calls `GetRoom`, orders by `joined_at, id`, and cannot expose token hashes.
- Changes are limited to the requested store interface, SQLite implementation, and store tests.

## Concerns

- As stated in the task brief, packages outside `internal/store` temporarily require planned pointer caller changes and were not changed or used as the acceptance gate.
- `ErrInviteClaimed` remains defined for compatibility but has no claim-state reads or writes.
