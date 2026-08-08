# Task 5 Report

## Status

Implemented and committed the clean `/v2` relay HTTP contract while preserving the Task 4 participant-scoped scan and subscribe-before-recheck behavior.

## Changes

- Registered only `/v2` API routes and made `/v1` and other versioned paths return `unsupported_api_version`.
- Added optional `max_participants` room creation, including pre-store validation; omission creates an unlimited room.
- Kept invite claims reusable and returned safe room membership from authenticated status responses.
- Added addressed sends through `to`, with non-disclosing `invalid_recipient` errors.
- Returned participant-visible history as `{messages,cursor}`.
- Kept room-wide waiter notification while independently applying participant visibility, preserving skipped-row cursor progress and returning timeout `sequence`.
- Verified that a done event wakes Alice, Bob, and Carol waiters.
- Removed obsolete relay mapping and tests for `ErrInviteClaimed`.
- Added the missing `Room.Participants` response field from the canonical model contract.

## Commit

- `c90d94b feat: publish multi-participant relay v2`

## TDD and Verification

- RED: the focused new contract suite failed against the old handler with `unsupported_api_version` for `/v2`.
- GREEN: `go test ./internal/relay -run 'Test(V2|ReusableInvite|DirectedMessage|WaitSkips|DoneWakes)' -count=1` passed.
- `go test -race ./internal/relay -count=1` passed.
- `git diff --check` passed.
- `go test ./... -count=1` was run. Relay, client unit, store, localconfig, securetoken, and setup packages passed. CLI, localserver, and MCP integration tests still use the old client `/v1` contract and fail with `unsupported_api_version`; their migration is assigned to later client/adapter tasks.

## Concerns

- The clean cutoff intentionally makes current higher-level integrations unusable until their scheduled `/v2` migrations land.
- Relay fixtures now use per-test temporary SQLite files instead of `:memory:` so the three concurrent waiter test exercises stable multi-connection behavior under the race detector.
