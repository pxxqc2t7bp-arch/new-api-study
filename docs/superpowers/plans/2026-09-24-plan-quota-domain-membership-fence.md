# Plan Quota Domain Membership Fence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent a channel created or rotated concurrently with a shared single-key Plan quota disable from escaping the disabled credential domain.

**Architecture:** Add a permanent, non-secret authority row keyed by the lowercase SHA-256 credential hash. Disable, recovery, create, and identity-changing writes lock the relevant authority rows before route, channel, and ability rows; the authority state is projected atomically into channel metadata and routing state. Startup backfill establishes authority for existing Plan channels before writes are served.

**Tech Stack:** Go, GORM, SQLite, MySQL 5.7+/8, PostgreSQL 9.6+/17, testify

---

## Tasks

### Task 1: Add the Cross-Process Domain Authority and Wire Every Writer

**Files:**

- Create: `model/plan_quota_domain.go`
- Create: `model/plan_quota_domain_test.go`
- Modify: `model/main.go`
- Modify: `model/channel.go`
- Modify: `model/channel_status_cas_test.go`
- Modify: `service/channel.go`
- Modify: `service/channel_quota_test.go`
- Modify: `service/codex_credential_refresh.go`
- Modify: `service/upstream_channel_pair.go`
- Modify: `controller/channel.go`
- Modify: `controller/codex_usage.go`
- Modify: `docs/superpowers/specs/2026-09-23-monthly-plan-quota-isolation-design.md`
- Modify: `docs/mvp/handoff.md`

- [ ] **Step 1: Add authority migration and backfill RED tests**

Add tests that require:

```go
type PlanQuotaDomain struct {
    CredentialHash string `gorm:"type:char(64);primaryKey"`
    Generation     int64  `gorm:"bigint;not null;default:0"`
    State          string `gorm:"type:varchar(16);not null"`
    DisabledUntil  int64  `gorm:"bigint;not null;default:0"`
}
```

Cover an active existing Plan credential, a quota-owned disabled credential,
conflicting generations/deadlines, an empty credential, a multi-key row, and a
random `schema.NamingStrategy{TablePrefix: ...}`. Assert only non-empty
single-key Plan credentials create authority rows, disabled ownership never
backfills as active, and no stored value contains the credential.

- [ ] **Step 2: Run migration RED**

Run:

```bash
go test ./model -run '^(TestInitializePlanQuotaDomains|TestPlanQuotaDomainHonorsNamingStrategy)$' -count=1
```

Expected: build failure because the authority type and initializer do not
exist.

- [ ] **Step 3: Implement schema, hashing, and fail-closed backfill**

Create `model/plan_quota_domain.go` with:

```go
const (
    PlanQuotaDomainStateActive   = "active"
    PlanQuotaDomainStateDisabled = "disabled"
)

func PlanQuotaDomainHash(credential string) (string, bool)
func PlanQuotaDomainMembership(channel *Channel) (string, bool)
func InitializePlanQuotaDomains() error
```

Hash exact credential bytes with SHA-256 and lowercase hex. Never log or store
raw credentials or hashes. Register `&PlanQuotaDomain{}` immediately after
`&Channel{}` in `migrateDB`, then run the initializer before startup succeeds.
Do not define `TableName`; all queries must use GORM models so `TablePrefix`
works.

Backfill in one transaction. For each existing non-empty single-key `plan:`
credential, create or validate one authority. A valid matching
`quota_domain_id`, or validated legacy `quota_type=plan` ownership, makes the
row disabled. Use the newest valid decimal generation and maximum known
deadline. Malformed or conflicting ownership must remain disabled or fail the
initializer, never become active. Return every database or commit error.

- [ ] **Step 4: Verify migration GREEN**

Run the Step 2 command. Expected: PASS.

- [ ] **Step 5: Add membership-writer RED tests**

Add deterministic tests for:

```text
TestPlanQuotaDomainDisableSerializesConcurrentCreate
TestPlanQuotaDomainDisableSerializesConcurrentRotationIntoDomain
TestPlanQuotaDomainRecoverySerializesConcurrentCreate
TestPlanQuotaDomainUpdateLocksOldAndNewHashesInOrder
TestPlanQuotaDomainMissingAuthorityFailsClosed
TestPlanQuotaDomainUnknownWriteIsNotRetried
```

Use a temporary SQLite file, `_txlock=immediate`, and multiple connections.
Use test-only barriers immediately after authority lock acquisition; do not
use timing sleeps. Exercise both legal commit orders. A create or rotation
that enters a disabled authority must commit auto-disabled with matching
`quota_domain_id`, decimal `quota_generation`, deadline, disabled abilities,
and no cache routing membership. A transaction/commit error must be returned
after one attempt.

- [ ] **Step 6: Run membership RED**

Run:

```bash
go test ./model -run '^TestPlanQuotaDomain(DisableSerializes|RecoverySerializes|UpdateLocks|MissingAuthority|UnknownWrite)' -count=1
```

Expected: FAIL because channel identity writers do not lock persistent domain
authority.

- [ ] **Step 7: Implement the authority lock protocol for all identity writers**

Add internal helpers that:

1. Sort and deduplicate credential hashes.
2. Safely insert authority for a genuinely new destination credential.
3. Lock authority rows in lexical order with `lockForUpdate`.
4. Lock managed source/group/route rows when applicable.
5. Lock channel rows in ascending ID order.
6. Update abilities in the same transaction.
7. Publish complete channel/cache snapshots only after commit.

Use the protocol from:

```text
BatchInsertChannels
Channel.Insert
Channel.Update
BatchSetChannelTag
EditChannelByTag
ApplyUpstreamEnrollmentResult
RefreshCodexChannelCredential
the Codex usage fallback refresh
```

Replace broad malformed-settings `Channel.Save` writes with field-specific
updates so they cannot replay stale identity. Add a snapshot-fenced key
rotation API for Codex callers. An existing Plan member with missing authority
must fail closed. An entering channel may retain a manual disable, but it may
never commit enabled while the destination authority is disabled.

- [ ] **Step 8: Verify writer GREEN**

Run the Step 6 command plus:

```bash
go test ./model ./service ./controller -run 'PlanQuotaDomain|CodexCredential|ChannelTag|BatchInsertChannels|ChannelUpdate' -count=1
```

Expected: PASS.

- [ ] **Step 9: Add atomic disable/recovery RED tests**

Extend service/model tests to require:

```text
TestPlanQuotaDomainDisableRollbackIncludesAuthorityAndAbilities
TestPlanQuotaDomainDisableIncludesMemberCommittedBeforeLock
TestPlanQuotaDomainCreateAfterDisableInheritsGeneration
TestPlanQuotaDomainRecoveryIsAtomic
TestPlanQuotaDomainRecoveryRejectsStaleAuthorityGeneration
TestPlanQuotaDomainRecoveryClearsQuotaOwnerButPreservesManagedRouteDisable
```

Assert domain state, generation, every eligible channel's full metadata,
abilities, and cache publish atomically. Inject channel, ability, and commit
errors separately. A recovery error must leave the authority disabled and
publish no partial source or peer enable.

- [ ] **Step 10: Run transition RED**

Run:

```bash
go test ./model ./service -run '^TestPlanQuotaDomain(Disable|CreateAfter|Recovery)' -count=1
```

Expected: FAIL because disable enumerates before its transaction and recovery
commits source and peers separately.

- [ ] **Step 11: Move shared-domain disable and recovery into authority transactions**

Expose focused model request/result types rather than leaking `*gorm.DB` into
the service package:

```go
type PlanQuotaDomainDisableRequest struct {
    FailingChannelID  int
    ObservedCredential string
    ObservedTag       string
    Reason            string
    ResetAt           int64
    Generation        int64
}

type PlanQuotaDomainTransitionResult struct {
    Channels       []*Channel
    NewlyDisabled  int
    NewlyEnabled   int
}
```

The disable transaction locks or establishes the observed authority, advances
the generation to `max(time.Now().UnixNano(), current.Generation+1)`, marks it
disabled, discovers exact current members while locked, validates complete
snapshots, and commits authority, channel metadata/status, and abilities
together.

The recovery transaction locks the disabled authority, requires the source
snapshot's exact marker/generation/deadline, locks applicable managed routes
before sorted channels, and advances the authority to active while clearing
matching quota ownership. Enable only members whose independent route state
allows routing; clear the expired quota owner without overriding another
disable owner. Manual recovery uses the same transaction without the
due-time gate. Keep empty credentials on the existing source-only snapshot
CAS and keep multi-key Plan handling unchanged.

Delete `planQuotaDisableMaxAttempts` and the per-member shared-domain retry.
Do not retry transaction or commit errors. Notify and publish only from the
committed result.

- [ ] **Step 12: Verify transition GREEN and regressions**

Run:

```bash
go test ./model ./service ./controller -count=1
go test -race ./model -run 'PlanQuotaDomain|ChannelStatus' -count=1
go test -race ./service -run 'PlanQuota|Managed' -count=1
```

Expected: PASS with no warnings or partial-state observations.

- [ ] **Step 13: Run real-dialect and static gates**

Run:

```bash
TEST_MYSQL_DSN='...' TEST_POSTGRES_DSN='...' \
  go test ./model -run '^TestPlanQuotaDomainConfiguredDatabases$' -count=1 -v
go vet ./model ./service ./controller
gofmt -d model/plan_quota_domain.go model/plan_quota_domain_test.go \
  model/main.go model/channel.go model/channel_status_cas_test.go \
  service/channel.go service/channel_quota_test.go \
  service/codex_credential_refresh.go service/upstream_channel_pair.go \
  controller/channel.go controller/codex_usage.go
git diff --check
```

MySQL and PostgreSQL cases use isolated prefixed tables and drop only those
tables. If DSNs are unavailable, record explicit skips. Expected: all
configured cases and static gates pass.

- [ ] **Step 14: Self-review and commit**

Check for raw credential disclosure, hard-coded table names, unsorted lock
sets, cache publication before commit, retries after unknown writes, direct
key/tag bypasses, and schema changes beyond the authority table. Mark the
handoff entry complete and commit:

```bash
git add model/plan_quota_domain.go model/plan_quota_domain_test.go \
  model/main.go model/channel.go model/channel_status_cas_test.go \
  service/channel.go service/channel_quota_test.go \
  service/codex_credential_refresh.go service/upstream_channel_pair.go \
  controller/channel.go controller/codex_usage.go \
  docs/superpowers/specs/2026-09-23-monthly-plan-quota-isolation-design.md \
  docs/superpowers/plans/2026-09-24-plan-quota-domain-membership-fence.md
git commit -m "fix(channel): fence plan quota domain membership"
```

`docs/mvp/handoff.md` remains local coordination state and is not added to the
implementation commit.
