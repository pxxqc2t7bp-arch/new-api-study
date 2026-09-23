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

### Task 8: Require Structured Plan Quota Evidence

**Files:**
- Modify: `service/channel_quota_test.go`
- Modify: `service/channel.go`
- Modify: `controller/channel_test_internal_test.go`
- Modify: `controller/relay.go`

- [x] **Step 1: Add classifier RED coverage**

Cover HTTP 429 plus normalized `AccountQuotaExceeded` from
`NewAPIError.GetErrorCode()` or available OpenAI code/type fields and
recognized monthly, weekly, 5-hour, or remaining weighted-token evidence.
Include `WithClaudeError` coverage. Reject ordinary 429s, wrong status, wrong
code/type, and matching semantics without quota evidence.

Observed RED:

```text
undefined: ClassifyPlanQuotaError
```

- [x] **Step 2: Add the service classifier**

Keep `ParsePlanQuotaReset` as the message parser, but permit the Plan-domain
path only through `ClassifyPlanQuotaError`. Leave configured generic status and
keyword disables unchanged.

- [x] **Step 3: Use the classifier in controller precedence**

Replace text-only managed prioritization with the service classifier. Keep
managed unsupported-model isolation first.

### Task 9: Bind Disable to the Observed Request Identity

**Files:**
- Modify: `service/channel_quota_test.go`
- Modify: `service/channel.go`
- Modify: `controller/relay.go`

- [x] **Step 1: Add the credential-rotation RED regression**

Send the failing request with a Plan tag and credential A, rotate both source
tag and key to an ordinary/B identity, and assert that current Plan A peers are
isolated while the rotated source and B peers remain enabled.

Observed RED:

```text
undefined: DisableChannelForAPIError
```

- [x] **Step 2: Add the structured disable entry**

`DisableChannelForAPIError` carries the classified reset,
`ChannelError.UsingKey`, and selected channel snapshot tag into domain
selection. The request-time tag decides Plan eligibility; current rows are
matched exactly and written through the existing identity/status/metadata CAS.
Empty observed credentials remain source-only.

- [x] **Step 3: Make recognized Plan handling synchronous**

Call the structured entry directly from `processChannelError` before retry
selection. Keep generic disables asynchronous; the structured service entry
retains multi-key per-key handling.

### Task 10: Fence and Count Health-Check Recovery

**Files:**
- Modify: `service/channel_quota_test.go`
- Modify: `service/channel.go`
- Modify: `controller/channel_test_internal_test.go`
- Modify: `controller/channel-test.go`

- [x] **Step 1: Add peer-fence RED coverage**

After source CAS success, assert that peers with a different generation or a
future `disabled_until` remain auto-disabled with disabled abilities. Preserve
compatibility when both marked legacy rows omit generation.

Observed RED: mismatched-generation and not-yet-due peers were enabled.

- [x] **Step 2: Apply generation and due-time checks**

Require matching non-empty generation strings, or explicit both-missing legacy
compatibility, and require each peer to be due before its own CAS.

- [x] **Step 3: Add committed-count RED coverage**

Assert a source plus peer recovery returns two commits and a stale replay
returns zero. Exercise `testChannelForHealthCheck` against a real local HTTP
upstream and require the same summary values.

Observed RED: the service API had no return value; after adding it, controller
summaries reported one for both the two-row commit and stale no-op.

- [x] **Step 4: Return and aggregate committed enables**

Return the number of successful status CAS commits from
`EnableChannelForHealthCheck` and add that exact number to the controller
summary.

### Task 11: Preserve Plan Ownership During Managed Reconciliation

**Files:**
- Modify: `service/upstream_orchestration_test.go`
- Modify: `service/upstream_routing.go`

- [x] **Step 1: Add the two-path RED regression**

Run `ReconcileManagedUpstreams` with one shadow route becoming active and one
already-active route entering steady-state ranking. Assert both owned channels
remain disabled while route state, rank, priority, endpoint, and models update.

Observed RED: both channels and abilities were enabled, and the activation
path also appended generic status metadata.

- [x] **Step 2: Share an ownership guard**

Both the route-state activation writer and `rankManagedRoutes` consult
`PlanQuotaRecoveryDomainKey` before selecting enabled status. Valid ownership
preserves status, raw metadata, and disabled abilities; ordinary managed
behavior is unchanged.

### Task 12: Fail Passive Selection Closed

**Files:**
- Modify: `controller/channel_test_internal_test.go`
- Modify: `controller/channel-test.go`

- [x] **Step 1: Add a database-failure RED regression**

Run the passive system task without the managed-route table and provide an
otherwise probeable ordinary auto-disabled channel.

Observed RED: the query error was ignored, the task returned success, and the
upstream received a probe.

- [x] **Step 2: Propagate selection errors**

Return `([]*model.Channel, error)` from `selectChannelsForAutomaticTest`.
Propagate managed-route query failures from `runChannelTestTask` before worker
startup and update every selector call site.

### Task 13: Final Verification and Commit

**Files:**
- Modify only the registered handoff occupancy list.

- [x] **Step 1: Run formatting and focused/package tests**

```bash
gofmt -w service/channel.go service/channel_quota_test.go \
  service/upstream_routing.go service/upstream_orchestration_test.go \
  controller/relay.go controller/channel-test.go \
  controller/channel_test_internal_test.go
go test ./service ./controller -count=1
```

- [x] **Step 2: Run race, vet, and whitespace gates**

```bash
go test -race ./service -run \
  'PlanQuota|ClassifyPlanQuota|ReconcileManagedUpstreamsPreservesPlanQuotaOwnership' -count=1
go test -race ./controller -run \
  'Test(ChannelForHealthCheckCountsOnlyCommittedRecoveries|RunChannelTestTaskFailsClosedWhenManagedRouteQueryFails|SelectChannelsForAutomaticTest.*|ShouldPrioritizePlanQuotaDisableForManagedChannel)' -count=1
go vet ./service ./controller
git diff --check
```

- [x] **Step 3: Self-review and commit**

Review the cumulative diff for occupancy, secret disclosure, schema changes,
multi-key behavior, and stale-write protection, then create one focused commit.

### Task 14: Close Final Review Blockers

**Files:**

- Modify: `service/channel.go`
- Modify: `service/channel_quota_test.go`
- Modify: `service/upstream_routing.go`
- Modify: `service/upstream_orchestration_test.go`
- Modify: `model/channel.go`
- Modify: `model/channel_status_cas_test.go`
- Modify: `docs/superpowers/specs/2026-09-23-monthly-plan-quota-isolation-design.md`
- Modify: `docs/superpowers/plans/2026-09-23-final-plan-quota-recovery-races.md`

- [x] **Step 1: Add all four blocking RED regressions**

Cover empty observed key rotation by key and by tag, managed multi-key Plan
isolation below the managed threshold, a future source `disabled_until`, and
deterministic quota-disable interleavings in activation and steady-state rank.
Add a model contract test for stale and fresh managed channel snapshots.

Observed RED:

```text
TestDisableChannelForAPIErrorPreservesRotatedEmptyCredentialSource:
source status became 3 and its ability became disabled after both rotations.

TestDisableChannelManagedPlanMultiKeyImmediatelyIsolatesUsedKey:
used-key status remained 0 and the managed failure counter became 1.

TestEnableChannelForHealthCheckPreservesPlanQuotaDomainBeforeSourceDue:
returned 1, enabled the source ability, and cleared source quota metadata.

TestReconcileManagedUpstreamsPreservesConcurrentPlanQuotaDisable:
activation and steady-state rank both restored channel status 1 and enabled
the ability.

TestUpdateManagedChannelIfUnchangedPreservesConcurrentStatusOwner:
build failed because ManagedChannelUpdate and
UpdateManagedChannelIfUnchanged did not exist.
```

- [x] **Step 2: Bind empty observed identity and managed multi-key handling**

Require the freshly loaded empty-key source to remain non-multi-key with the
exact empty raw key and observed tag. Route structured multi-key Plan errors
directly to `UpdateChannelStatus` with `UsingKey` before managed failure
accounting.

- [x] **Step 3: Fence source recovery by due time**

Return zero from `EnableChannelForHealthCheck` before any write when the source
snapshot has `disabled_until > now`. Keep `EnableChannel` unchanged as the
manual override.

- [x] **Step 4: Make final managed reconciliation atomic**

Remove activation's pre-rank enable. Add a model-owned managed update CAS that
uses the existing status locks, `lockForUpdate`, and a conditional channel
update, then commits route rank/multiplier, channel priority/base/models/status,
and abilities in one transaction. Retry stale snapshots from current state.

- [x] **Step 5: Verify focused GREEN**

```bash
go test ./service -run \
  '^(TestDisableChannelForAPIErrorPreservesRotatedEmptyCredentialSource|TestDisableChannelManagedPlanMultiKeyImmediatelyIsolatesUsedKey|TestEnableChannelForHealthCheckPreservesPlanQuotaDomainBeforeSourceDue|TestReconcileManagedUpstreamsPreservesConcurrentPlanQuotaDisable)$' -count=1
go test ./model -run \
  '^(TestUpdateManagedChannelIfUnchangedPreservesConcurrentStatusOwner|TestUpdateSingleKeyChannelStatusIfUnchanged.*)$' -count=1
```

Observed: PASS.

- [x] **Step 6: Run final formatting, package, race, vet, and diff gates**

Run the full requested verification set, including the optional configured
MySQL/PostgreSQL CAS matrix when DSNs are available, then self-review and
commit.

Observed: `gofmt`, focused race suites for model/service/controller, full
`go test ./service ./controller ./model -count=1`, `go vet` for those packages,
and `git diff --check` all passed. The optional configured-database test passed
with its MySQL and PostgreSQL cases skipped because `TEST_MYSQL_DSN` and
`TEST_POSTGRES_DSN` were unset. Cumulative review found no P0-P2 defect.
