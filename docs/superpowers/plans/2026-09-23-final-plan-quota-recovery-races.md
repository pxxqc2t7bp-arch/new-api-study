# Final Plan Quota Recovery Races Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the remaining credential-rotation, stale-recovery, legacy-writer, passive-fairness, managed-route, and pre-probe snapshot races in Plan quota isolation without a schema change.

**Architecture:** Extend the existing row-locked single-key CAS to compare the snapshot's credential and tag in addition to status and raw metadata. Reuse an internal lock-aware transaction primitive from quota ownership writes, legacy single-key status updates, and health-check recovery; stamp every Plan disable event with a string generation; select the oldest passive recovery candidate per shared domain; and admit validated managed Plan quota owners to passive recovery without changing ordinary managed handling.

**Tech Stack:** Go, GORM transactions and row locking, SQLite deterministic regression tests, optional MySQL/PostgreSQL integration tests, testify

---

### Task 1: Fence Credential and Tag Rotation

**Files:**
- Modify: `model/channel_status_cas_test.go`
- Modify: `model/channel.go`
- Modify: `service/channel.go`

- [x] **Step 1: Add stale-key and stale-tag tests**

Call the wished-for CAS with the snapshot's `Key` and `GetTag()` and mutate each
identity field separately before the call. Assert `changed=false` and that
channel status, raw metadata, and ability state remain unchanged.

- [x] **Step 2: Run focused RED**

Run:

```bash
go test ./model -run '^TestUpdateSingleKeyChannelStatusIfUnchanged' -count=1
```

Expected: build failure because the CAS does not yet accept expected key and
tag values.

- [x] **Step 3: Extend the locked CAS**

Change the API to accept:

```go
expectedKey string
expectedTag string
```

Compare `current.Key` and `current.GetTag()` while the row is locked, alongside
the existing single-key, status, and raw `other_info` checks. Pass exact
snapshot values from every service and test call site. Do not write either
identity value or expose the credential in metadata or logs.

- [x] **Step 4: Run focused GREEN**

Run the focused model test and the Plan quota service tests. Expected: PASS.

### Task 2: Fence Stale Recovery with a Disable Generation

**Files:**
- Modify: `service/channel_quota_test.go`
- Modify: `service/channel.go`

- [x] **Step 1: Add a deterministic stale-recovery interleaving test**

Disable a channel once, retain the resulting recovery snapshot, issue a second
disable event against the already-owned auto-disabled channel with the same
reason and reset time, then attempt the stale recovery CAS. Assert the second
event changed a string `quota_generation`, the stale CAS returns no change,
and status plus ability remain disabled.

- [x] **Step 2: Run focused RED**

Run:

```bash
go test ./service -run '^TestFreshPlanQuotaDisableFencesStaleRecoverySnapshot$' -count=1
```

Expected: FAIL because repeated disables currently reproduce identical
metadata and the stale recovery succeeds.

- [x] **Step 3: Stamp every disable event**

Generate one decimal `UnixNano` string per `disablePlanQuotaDomain` call and
write it as `quota_generation` on every eligible channel, including channels
already auto-disabled by the same domain. Remove it with the other quota
ownership fields during recovery.

- [x] **Step 4: Run focused GREEN**

Run the new regression and all Plan quota service tests. Expected: PASS.

### Task 3: Make Legacy Single-Key Status Writes Transactional

**Files:**
- Modify: `model/channel_status_test.go`
- Modify: `model/channel.go`

- [x] **Step 1: Add rollback and interleaving regressions**

Force the ability update to fail and assert `UpdateChannelStatus` returns
false while database channel status, metadata, ability state, and cache remain
unchanged. Add a deterministic simulated competing transactional writer
between the legacy channel write and ability write; assert the final channel
status and ability state cannot diverge.

- [x] **Step 2: Run focused RED**

Run:

```bash
go test ./model -run '^TestUpdateChannelStatusSingleKey' -count=1
```

Expected: FAIL because the channel write commits before the deferred ability
write and the cache is changed before persistence.

- [x] **Step 3: Factor lock and transaction internals**

Create a shared internal lock wrapper and a lock-assuming single-key CAS
transaction. Keep `UpdateSingleKeyChannelStatusIfUnchanged` as the public
snapshot API. Route non-multi-key `UpdateChannelStatus` through a bounded
snapshot/retry loop that builds reason/time metadata and calls the same
transaction. Update cache status only after commit. Keep the existing
multi-key mutation and persistence behavior.

- [x] **Step 4: Run focused GREEN and race coverage**

Run:

```bash
go test ./model -run '^(TestUpdateChannelStatusSingleKey|TestUpdateSingleKeyChannelStatusIfUnchanged)' -count=1
go test -race ./model -run '^(TestUpdateChannelStatusSingleKey|TestUpdateSingleKeyChannelStatusIfUnchanged)' -count=1
```

Expected: PASS.

### Task 4: Select the Oldest Passive Recovery Candidate

**Files:**
- Modify: `controller/channel_test_internal_test.go`
- Modify: `controller/channel-test.go`

- [x] **Step 1: Add the fairness regression**

Provide two due channels in the same marked domain with the most recently
tested channel first. Give the peer an older `TestTime` and assert the peer is
selected. Cover equal `TestTime` values with the lower ID as the deterministic
winner while unrelated failures remain individually selected.

- [x] **Step 2: Run focused RED**

Run:

```bash
go test ./controller -run '^TestSelectChannelsForAutomaticTest.*Recovery' -count=1
```

Expected: FAIL because the current implementation keeps the first domain
representative.

- [x] **Step 3: Choose by age per recovery key**

For passive recovery only, collect one candidate per shared recovery key and
replace it when a candidate has an older `TestTime`, or the same `TestTime`
and a lower ID. Continue assigning every non-owned auto-disabled row a unique
channel key.

- [x] **Step 4: Run focused GREEN**

Run the focused controller selection tests. Expected: PASS.

### Task 5: Prioritize Managed Plan Domain Isolation

**Files:**
- Modify: `service/channel_quota_test.go`
- Modify: `service/channel.go`
- Modify: `controller/relay.go`

- [x] **Step 1: Add a managed-route fixture regression**

Enable upstream orchestration with a failure threshold above one, create a
managed route for a single-key Plan channel and a peer sharing its credential,
then call the public disable path with a recognized Plan quota error. Assert
both channels are auto-disabled with the same non-secret domain marker and
generation, both abilities are disabled, and managed threshold handling did
not suppress domain isolation.

- [x] **Step 2: Run focused RED**

Run:

```bash
go test ./service -run '^TestDisableChannelManagedPlanQuotaIsolatesCredentialDomain$' -count=1
```

Expected: FAIL because managed failure recording returns before Plan quota
classification.

- [x] **Step 3: Reorder service and controller decisions**

In `DisableChannel`, load/classify a recognized non-multi-key Plan quota domain
before calling `RecordManagedChannelFailure`. In `processChannelError`, send a
recognized non-multi-key Plan quota error to `DisableChannel` before the
managed failure branch. Leave managed unsupported-model isolation first and
leave ordinary managed errors on their existing threshold path.

- [x] **Step 4: Run focused GREEN**

Run the managed Plan regression plus existing managed unsupported-model and
failure-threshold tests. Expected: PASS.

### Task 6: Documentation and Final Verification

**Files:**
- Modify: `docs/superpowers/specs/2026-09-23-monthly-plan-quota-isolation-design.md`
- Modify: `docs/superpowers/plans/2026-09-23-monthly-plan-quota-isolation.md`
- Modify: `docs/superpowers/plans/2026-09-23-atomic-channel-quota-ownership.md`
- Modify: `docs/superpowers/plans/2026-09-23-final-plan-quota-recovery-races.md`

- [x] **Step 1: Update the design and plans**

Document identity-fenced CAS, exact string disable generations, transactional
legacy single-key updates, oldest-first passive recovery, managed Plan
precedence, no schema change, and the rule that raw credentials never enter
quota metadata or logs.

- [x] **Step 2: Format and run all requested checks**

```bash
gofmt -w model/channel.go model/channel_status_cas_test.go model/channel_status_test.go service/channel.go service/channel_quota_test.go controller/channel-test.go controller/channel_test_internal_test.go controller/relay.go
go test ./model ./service ./controller -count=1
go test -race ./model -run '^(TestUpdateChannelStatusSingleKey|TestUpdateSingleKeyChannelStatusIfUnchanged)' -count=1
go test -race ./service -run 'PlanQuota' -count=1
go test -race ./controller -run '^TestSelectChannelsForAutomaticTest.*Recovery' -count=1
go vet ./model ./service ./controller
git diff --check
```

- [x] **Step 3: Run optional real-dialect CAS tests when configured**

```bash
TEST_MYSQL_DSN='...' TEST_POSTGRES_DSN='...' \
  go test ./model -run '^TestUpdateSingleKeyChannelStatusIfUnchangedConfiguredDatabases$' -count=1 -v
```

Record skips when the DSNs are unavailable.

- [x] **Step 4: Review and commit**

Review the cumulative diff for scope, credential disclosure, and schema
changes, then commit all final-review changes:

```bash
git commit -m "fix(channel): close plan quota recovery races"
```

### Task 7: Bind Recovery to the Pre-Probe Snapshot

**Files:**
- Modify: `service/channel_quota_test.go`
- Modify: `service/channel.go`
- Modify: `controller/channel-test.go`
- Modify: `controller/channel_test_internal_test.go`
- Modify: `docs/superpowers/specs/2026-09-23-monthly-plan-quota-isolation-design.md`
- Modify: `docs/superpowers/plans/2026-09-23-final-plan-quota-recovery-races.md`

- [x] **Step 1: Add production service-path stale-probe regressions**

Call a wished-for exported health-check recovery API with a Plan quota source
snapshot captured before a second disable generation and before a concurrent
manual disable. Assert source and peers remain disabled, their ability rows
remain disabled, and the newer owner metadata is unchanged.

- [x] **Step 2: Verify focused RED**

Run:

```bash
go test ./service -run '^TestEnableChannelForHealthCheckRejects(StalePlanQuotaGeneration|ConcurrentManualDisable)$' -count=1
```

Observed: build failure because `EnableChannelForHealthCheck` did not exist.

- [x] **Step 3: Add snapshot-aware recovery and wire the controller**

Add `EnableChannelForHealthCheck(snapshot, usingKey)`. For a non-multi-key Plan
quota owner, derive the domain from the pre-probe snapshot and CAS the source
using its exact key, tag, status, and raw `other_info`. Abort without generic
enablement when the source CAS does not commit. After it commits, load current
same-domain peers and recover each through its own CAS. If the source
snapshot's credential does not match its marker, recover only the source.
Keep `EnableChannel` unchanged for manual and internal callers. Pass the
original channel snapshot from `testChannelForHealthCheck`.

- [x] **Step 4: Verify snapshot recovery GREEN**

Run the focused stale-probe tests and the broader `PlanQuota|MonthlyPlanQuota`
service subset. Expected and observed: PASS.

- [x] **Step 5: Add managed passive-selection RED**

Create managed-route fixtures for a due marked Plan quota row, a due validated
legacy Plan quota row, and a due ordinary auto-disabled row. Assert the two
validated Plan rows are selected and the ordinary managed row is excluded.

Run:

```bash
go test ./controller -run '^TestSelectChannelsForAutomaticTestPassiveRecoveryIncludesManagedPlanQuota$' -count=1
```

Observed: FAIL with an empty selected set.

- [x] **Step 6: Admit only validated managed Plan owners**

In passive selection, classify quota ownership before applying the managed
exclusion. Keep excluding managed rows without a valid shared Plan recovery
key, while allowing marked or validated legacy Plan quota rows after
`disabled_until`. Preserve existing domain deduplication and oldest-first
selection.

- [x] **Step 7: Verify managed-selection GREEN**

Run the passive recovery selection tests plus managed unsupported-model,
ordinary failure-classification, and managed Plan isolation tests. Expected
and observed: PASS.

- [x] **Step 8: Run final gates and commit**

```bash
gofmt -w service/channel.go service/channel_quota_test.go \
  controller/channel-test.go controller/channel_test_internal_test.go
go test ./service ./controller ./model -count=1
go test -race ./service -run '^(TestEnableChannelForHealthCheck|TestEnablePlanQuotaDomainAfterCredentialRotation|TestDisableChannelManagedPlanQuota)' -count=1
go test -race ./controller -run '^TestSelectChannelsForAutomaticTest.*Recovery' -count=1
go vet ./service ./controller ./model
git diff --check
git commit -m "fix(channel): bind quota recovery to probe snapshot"
```
