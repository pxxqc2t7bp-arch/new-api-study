# Atomic Channel Quota Ownership Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make single-key Plan quota disable and recovery preserve concurrent channel ownership by changing channel status, status metadata, and abilities in one row-locked compare-and-swap transaction.

**Architecture:** Add a focused model CAS that takes the expected and desired status plus raw `other_info`. It holds the existing process-local channel locks, locks the current database row with `lockForUpdate(tx)`, compares the snapshot exactly, and updates the channel and abilities in one `DB.Transaction`; cache status changes happen only after commit. Service disable and recovery build complete desired metadata from their selected snapshots and treat a stale CAS as a no-op.

**Tech Stack:** Go, GORM transactions and locking clauses, SQLite default tests, optional MySQL/PostgreSQL integration tests, testify

---

### Task 1: Specify the Model CAS Contract

**Files:**
- Create: `model/channel_status_cas_test.go`

- [ ] **Step 1: Write the successful CAS test**

Create a real single-key `Channel` and `Ability`, then call the wished-for API:

```go
changed, err := UpdateSingleKeyChannelStatusIfUnchanged(
	channel.Id,
	common.ChannelStatusEnabled,
	expectedOtherInfo,
	common.ChannelStatusAutoDisabled,
	desiredOtherInfo,
)
```

Assert `changed=true`, the persisted channel has the desired status and exact
raw `other_info`, and its ability is disabled.

- [ ] **Step 2: Write stale snapshot tests**

Use deterministic subtests for:

```go
expectedStatus != stored.Status
expectedOtherInfo != stored.OtherInfo
```

Assert `changed=false`, `err=nil`, and channel plus ability rows are unchanged.

- [ ] **Step 3: Write the rollback consistency test**

Register a temporary GORM update callback that rejects the selected ability
update. Call the CAS and assert an error, then verify the transaction left the
channel status, channel metadata, ability enabled state, and status cache
unchanged. Remove the callback in test cleanup.

- [ ] **Step 4: Run focused RED**

Run:

```bash
go test ./model -run '^TestUpdateSingleKeyChannelStatusIfUnchanged' -count=1
```

Expected: build failure because
`UpdateSingleKeyChannelStatusIfUnchanged` does not exist.

### Task 2: Implement the Transactional Model CAS

**Files:**
- Modify: `model/channel.go`

- [ ] **Step 1: Add the minimal model API**

Implement:

```go
func UpdateSingleKeyChannelStatusIfUnchanged(
	channelId int,
	expectedStatus int,
	expectedOtherInfo string,
	status int,
	otherInfo string,
) (bool, error)
```

Acquire `channelStatusLock` when memory cache is enabled and always acquire
`GetChannelPollingLock(channelId)`. Inside `DB.Transaction`, load the channel
with:

```go
lockForUpdate(tx).Where("id = ?", channelId).First(&current)
```

Return no change when the row is multi-key or either expected value is stale.
Otherwise update only `status` and `other_info`, then update matching
`Ability.Enabled` rows through the same `tx`. Set `changed=true` only after both
updates succeed. After `DB.Transaction` commits successfully, call
`CacheUpdateChannelStatus`; never mutate cache before commit.

- [ ] **Step 2: Run focused GREEN**

Run:

```bash
go test ./model -run '^TestUpdateSingleKeyChannelStatusIfUnchanged' -count=1
```

Expected: PASS.

### Task 3: Protect Plan Disable Call Sites

**Files:**
- Modify: `service/channel_quota_test.go`
- Modify: `service/channel.go`

- [ ] **Step 1: Write a stale-snapshot regression test**

Use the empty-credential fail-closed path so the service receives a known stale
channel snapshot without test hooks. Persist a concurrent manual-disable status
and owner metadata after taking the snapshot, invoke
`disablePlanQuotaDomain`, and assert status, metadata, and ability remain owned
by the concurrent update.

- [ ] **Step 2: Run service RED**

Run:

```bash
go test ./service -run '^TestDisablePlanQuotaDomainPreservesConcurrentOwnership$' -count=1
```

Expected: FAIL because the current status update plus metadata merge overwrites
the newer owner.

- [ ] **Step 3: Replace the split disable write**

For each eligible snapshot, retain `channel.Status` and `channel.OtherInfo` as
expected values. Build desired metadata from `channel.GetOtherInfo()`, adding
status reason/time only for an actual transition and adding or clearing quota
fields exactly as before. Call
`model.UpdateSingleKeyChannelStatusIfUnchanged`; log errors and treat
`changed=false` as an ownership no-op. Count notifications only when a
previously enabled row commits.

- [ ] **Step 4: Run service GREEN**

Run:

```bash
go test ./service -run '^(TestDisablePlanQuotaDomainPreservesConcurrentOwnership|TestDisableAndEnablePlanQuotaDomainLifecycle|TestDisablePlanQuotaCredentialDomain|TestDisablePlanQuotaCredentialDomainHandlesMissingCredential|TestDisablePlanQuotaDomainWithoutResetStillDisables)$' -count=1
```

Expected: PASS.

### Task 4: Protect Plan Recovery Call Sites

**Files:**
- Modify: `service/channel.go`

- [ ] **Step 1: Replace the split recovery write**

For each owned auto-disabled snapshot, build desired metadata by setting the
existing empty status reason/time and removing all quota ownership keys. Call
the same model CAS with the snapshot's exact status and raw `other_info`.
Increment recovery notifications only for committed changes; log transaction
errors and leave stale rows unchanged.

- [ ] **Step 2: Verify existing recovery behavior**

Run:

```bash
go test ./service -run '^(TestDisableAndEnablePlanQuotaDomainLifecycle|TestEnablePlanQuotaDomainScopesRecoveryByMarker|TestLegacyPlanQuotaDomainRecoveryByTag|TestEnablePlanQuotaDomainAfterCredentialRotation)$' -count=1
```

Expected: PASS.

### Task 5: Add the Optional Real-Dialect Matrix

**Files:**
- Modify: `model/channel_status_cas_test.go`

- [ ] **Step 1: Add configured database subtests**

Open `TEST_MYSQL_DSN` with `mysql.Open` and `TEST_POSTGRES_DSN` with
`postgres.New(... PreferSimpleProtocol: true)`. Skip an unset DSN. Give each
subtest a unique GORM table prefix, migrate temporary real `Channel` and
`Ability` tables, exercise the CAS, and drop only those prefixed tables in
cleanup. Temporarily point `model.DB` and the main database type at the
selected dialect, restoring both after each subtest.

- [ ] **Step 2: Run the default SQLite test**

Run:

```bash
go test ./model -run '^TestUpdateSingleKeyChannelStatusIfUnchanged' -count=1
```

Expected: SQLite tests PASS; configured-database subtests SKIP when their DSNs
are absent.

### Task 6: Documentation and Verification

**Files:**
- Modify: `docs/superpowers/specs/2026-09-23-monthly-plan-quota-isolation-design.md`
- Create: `docs/superpowers/plans/2026-09-23-atomic-channel-quota-ownership.md`

- [ ] **Step 1: Format and run focused checks**

```bash
gofmt -w model/channel.go model/channel_status_cas_test.go service/channel.go service/channel_quota_test.go
go test ./model -run '^TestUpdateSingleKeyChannelStatusIfUnchanged' -count=1
go test ./service -run 'PlanQuota|MonthlyPlanQuota' -count=1
```

- [ ] **Step 2: Run requested package and independent controller tests**

```bash
go test ./model ./service ./controller -count=1
go test ./controller -run '^TestSelectChannelsForAutomaticTestDeduplicatesDuePlanDomain$' -count=1
go vet ./model ./service ./controller
git diff --check
```

- [ ] **Step 3: Commit**

```bash
git add model/channel.go model/channel_status_cas_test.go \
  service/channel.go service/channel_quota_test.go \
  docs/superpowers/specs/2026-09-23-monthly-plan-quota-isolation-design.md \
  docs/superpowers/plans/2026-09-23-atomic-channel-quota-ownership.md
git commit -m "fix(channel): make quota ownership updates atomic"
```

### Final Review Addendum: Identity and Legacy Writer Fencing

The final review extends this plan without adding schema:

- `UpdateSingleKeyChannelStatusIfUnchanged` also receives the snapshot's exact
  key and tag and compares both under the row lock.
- Stale-key and stale-tag tests prove credential or routing identity rotation
  makes the CAS a no-op.
- Every Plan quota disable event stores a new string `quota_generation`, even
  for an already-owned auto-disabled channel, so a stale recovery snapshot
  cannot undo a fresh disable.
- The non-multi-key branch of `UpdateChannelStatus` uses a bounded
  snapshot/CAS retry over the same internal transaction. Channel status,
  `other_info`, and abilities commit together, and cache status changes only
  after commit.
- Multi-key status-list handling remains on its existing path.

Final focused commands:

```bash
go test ./model -run '^(TestUpdateChannelStatusSingleKey|TestUpdateSingleKeyChannelStatusIfUnchanged|TestUpdateChannelStatusPersistsMultiKeyState)' -count=1
go test -race ./model -run '^(TestUpdateChannelStatusSingleKey|TestUpdateSingleKeyChannelStatusIfUnchanged)' -count=1
TEST_MYSQL_DSN='...' TEST_POSTGRES_DSN='...' \
  go test ./model -run '^TestUpdateSingleKeyChannelStatusIfUnchangedConfiguredDatabases$' -count=1 -v
```
