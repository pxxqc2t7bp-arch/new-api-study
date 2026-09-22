# Monthly Plan Quota Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automatically isolate single-key Plan channels sharing an exact credential when an upstream monthly quota is exhausted, then recover only that credential domain.

**Architecture:** Extend the existing Plan quota classifier instead of treating all 429 responses as fatal. Load channels with `model.GetAllChannels(..., selectAll=true)`, compare `Channel.GetKeys()` values case-sensitively in Go, and persist a SHA-256 credential-domain marker for scoped recovery. Empty credentials use a non-secret `channel:<id>` marker and fail closed on the known channel; multi-key Plan channels retain the generic per-key status flow. Disable sweeps preserve manual and unrelated auto-disable ownership. A reusable service classifier validates legacy metadata and supplies domain keys for recovery and passive-test dedup. Credential rotation recovers only the tested channel.

**Tech Stack:** Go, GORM, SQLite-backed unit tests, testify

---

### Task 1: Recognize Monthly Quota Exhaustion

**Files:**
- Modify: `service/channel_quota_test.go`
- Modify: `service/channel.go`

- [ ] **Step 1: Write the failing parser and disable-decision tests**

Add tests using the production error text:

```go
func TestParsePlanQuotaResetMonthly(t *testing.T) {
	resetAt, matched := ParsePlanQuotaReset(
		"status_code=429, You have exceeded the monthly usage quota. " +
			"It will reset at 2026-09-30 23:59:59 +0800 CST.",
	)

	assert.True(t, matched)
	assert.Equal(t, time.Date(2026, 9, 30, 23, 59, 59, 0, time.FixedZone("CST", 8*60*60)).Unix(), resetAt)
}

func TestShouldDisableChannelRecognizesMonthlyPlanQuota(t *testing.T) {
	originalEnabled := common.AutomaticDisableChannelEnabled
	originalKeywords := operation_setting.AutomaticDisableKeywords
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableKeywords = []string{"unrelated"}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = originalEnabled
		operation_setting.AutomaticDisableKeywords = originalKeywords
	})

	err := types.NewOpenAIError(
		errors.New("You have exceeded the monthly usage quota. It will reset at 2026-09-30 23:59:59 +0800 CST."),
		types.ErrorCode("AccountQuotaExceeded"),
		http.StatusTooManyRequests,
	)

	assert.True(t, ShouldDisableChannel(err))
}
```

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
go test ./service -run 'TestParsePlanQuotaResetMonthly|TestShouldDisableChannelRecognizesMonthlyPlanQuota' -count=1
```

Expected: both tests fail because monthly quota is not recognized.

- [ ] **Step 3: Implement the minimal classifier**

Extend `ParsePlanQuotaReset` with the monthly phrase and call it from
`ShouldDisableChannel` before generic status-code and keyword matching:

```go
if _, quotaLimited := ParsePlanQuotaReset(err.Error()); quotaLimited {
	return true
}
```

- [ ] **Step 4: Run the focused tests and verify GREEN**

Run:

```bash
go test ./service -run 'TestParsePlanQuotaResetMonthly|TestShouldDisableChannelRecognizesMonthlyPlanQuota|TestParsePlanQuotaResetRejectsOrdinaryRateLimit' -count=1
```

Expected: PASS.

### Task 2: Isolate Exact Single-Key Plan Credential Domains

**Files:**

- Modify: `service/channel.go`
- Modify: `service/channel_quota_test.go`

- [ ] **Step 1: Extend the credential-domain test and verify RED**

Cover exact `Channel.GetKeys()` matching, case sensitivity, and exclusions:

```go
channels := []model.Channel{
    {Id: 11, Name: "messages", Key: "shared", Status: common.ChannelStatusEnabled, Tag: planTag, AutoBan: &autoBan},
    {Id: 12, Name: "responses", Key: "shared\n", Status: common.ChannelStatusEnabled, Tag: planTag, AutoBan: &autoBan},
    {Id: 13, Name: "native", Key: "shared", Status: common.ChannelStatusEnabled, Tag: nativeTag, AutoBan: &autoBan},
    {Id: 14, Name: "other-account", Key: "other", Status: common.ChannelStatusEnabled, Tag: planTag, AutoBan: &autoBan},
    {Id: 15, Name: "case-different", Key: "SHARED", Status: common.ChannelStatusEnabled, Tag: planTag, AutoBan: &autoBan},
    {Id: 16, Name: "ordinary", Key: "shared", Status: common.ChannelStatusEnabled, Tag: ordinaryTag, AutoBan: &autoBan},
    {Id: 17, Name: "multi-key", Key: "shared", Status: common.ChannelStatusEnabled, Tag: planTag, AutoBan: &autoBan, ChannelInfo: model.ChannelInfo{IsMultiKey: true}},
}
```

Run:

```bash
go test ./service -run TestDisablePlanQuotaCredentialDomain -count=1
```

Expected: FAIL because the database equality query does not apply
`Channel.GetKeys()` semantics and does not exclude a one-entry multi-key
channel.

- [ ] **Step 2: Implement in-memory exact matching and verify GREEN**

Remove `model.GetChannelsByKey`. Load all credentials and select only matching
single-key Plan channels. An eligible match must be currently enabled or
already auto-disabled with the same `quota_domain_id`; manually disabled rows
and auto-disabled rows carrying another marker must retain their status,
metadata, and abilities:

```go
channels, err := model.GetAllChannels(0, 0, true, false)
matchedChannels := make([]*model.Channel, 0)
for _, channel := range channels {
    keys := channel.GetKeys()
    if !isNonMultiKeyPlanChannel(channel) || len(keys) != 1 || keys[0] != credential {
        continue
    }
    matchedChannels = append(matchedChannels, channel)
}
```

Run:

```bash
go test ./service -run TestDisablePlanQuotaCredentialDomain -count=1
```

Expected: PASS.

### Task 3: Scope Recovery to the Persisted Domain Marker

**Files:**

- Modify: `service/channel.go`
- Modify: `service/channel_quota_test.go`

- [ ] **Step 1: Write marker-scoped recovery tests and verify RED**

Create two auto-disabled credential domains that share a tag, a
shared-credential channel with another tag, and a manually disabled channel in
the shared tag. Recover one channel through `EnableChannel` and assert:

```go
assert.Equal(t, common.ChannelStatusEnabled, recovered.Status)
assert.Equal(t, common.ChannelStatusAutoDisabled, otherDomain.Status)
assert.Equal(t, common.ChannelStatusManuallyDisabled, manuallyDisabled.Status)
assert.NotContains(t, recovered.GetOtherInfo(), "quota_domain_id")
assert.Contains(t, otherDomain.GetOtherInfo(), "quota_domain_id")
```

Add a legacy fixture whose auto-disabled rows have `quota_domain` but no
`quota_domain_id`; assert same-tag auto-disabled rows recover while manually
disabled rows do not.

Run:

```bash
go test ./service -run 'TestEnablePlanQuotaDomain|TestLegacyPlanQuotaDomain' -count=1
```

Expected: FAIL because recovery currently selects every row by tag.

- [ ] **Step 2: Persist and recover by a non-secret marker**

Compute the marker as follows:

```go
sum := sha256.Sum256([]byte(credential))
domainID := hex.EncodeToString(sum[:])
```

Store `domainID` as `quota_domain_id`; use `channel:<id>` when the credential
is empty. Change
`enablePlanQuotaDomain` to accept the recovering channel, load fresh channel
rows, and update only auto-disabled rows with the same marker. When the
recovering row has no marker, recover only auto-disabled rows whose metadata
explicitly contains `quota_type="plan"` and `quota_domain` equal to the
recovering tag. Generic markerless failures remain disabled. Clear quota
metadata only after a selected row is recovered.

- [ ] **Step 3: Run recovery tests and verify GREEN**

Run:

```bash
go test ./service -run 'TestEnablePlanQuotaDomain|TestLegacyPlanQuotaDomain' -count=1
```

Expected: PASS.

### Task 4: Fail Closed for Missing Credentials

**Files:**

- Modify: `service/channel_quota_test.go`
- Modify: `service/channel.go`

- [ ] **Step 1: Update the missing-credential test and verify RED**

Keep the nil-channel case unchanged. For an empty credential, assert the known
channel becomes auto-disabled, its ability is disabled, reset metadata is
stored, and `quota_domain_id` equals `channel:<id>`.

Run:

```bash
go test ./service -run TestDisablePlanQuotaCredentialDomainHandlesMissingCredential -count=1
```

Expected: FAIL because the current function returns without disabling the
known channel.

- [ ] **Step 2: Implement the empty-credential fallback and verify GREEN**

Select only the known failing channel when `GetKeys()` returns no credential,
then use the normal status and metadata update path.

Run the same focused command. Expected: PASS.

### Task 5: Preserve Multi-Key Per-Key Disable Behavior

**Files:**

- Modify: `service/channel_quota_test.go`
- Modify: `service/channel.go`

- [ ] **Step 1: Write the public-path regression test and verify RED**

Create a two-key Plan channel, call `DisableChannel` with
`UsingKey: "key-a"` and a recognized quota reason, then assert:

```go
assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
assert.True(t, ability.Enabled)
```

Run:

```bash
go test ./service -run TestDisableChannelPreservesPlanMultiKeyIsolation -count=1
```

Expected: FAIL because the shared-domain path currently disables the whole
channel.

- [ ] **Step 2: Gate shared-domain isolation and verify GREEN**

Enter `disablePlanQuotaDomain` only for non-multi-key Plan channels. Let
multi-key Plan errors reach the existing `model.UpdateChannelStatus` call with
`channelError.UsingKey`.

Run the same focused command. Expected: PASS.

### Task 6: Preserve Passive Recovery Domains and Credential Rotation

**Files:**

- Modify: `service/channel.go`
- Modify: `service/channel_quota_test.go`
- Modify: `controller/channel-test.go`
- Modify: `controller/channel_test_internal_test.go`

- [ ] **Step 1: Write failing ownership and rotation tests**

Extend the shared-credential fixture with a manually disabled row and an
auto-disabled row carrying another `quota_domain_id`. Assert that status,
metadata, and abilities are unchanged. Add a recovery test that disables two
`credential-a` peers, rotates the recovering row to `credential-b`, and
asserts only the rotated row is enabled and cleared.

Run:

```bash
go test ./service -run '^(TestDisablePlanQuotaCredentialDomain|TestEnablePlanQuotaDomainAfterCredentialRotation)$' -count=1
```

Expected: FAIL because the sweep overwrites existing ownership and recovery
trusts the stale persisted marker.

- [ ] **Step 2: Write the failing passive-dedup test with isolated DB setup**

Initialize `model.DB` explicitly in the controller test fixture. Cover two
rows sharing a `quota_domain_id` across tags, a distinct marked domain in the
same tag, two validated legacy rows, and two generic markerless failures.

Run:

```bash
go test ./controller -run '^TestSelectChannelsForAutomaticTestDeduplicatesDuePlanDomain$' -count=1
```

Expected: FAIL because selection deduplicates all Plan rows by tag.

- [ ] **Step 3: Add the reusable recovery-domain classifier**

Export a service helper that returns a stable dedup key for:

- auto-disabled rows with a non-empty string `quota_domain_id`;
- markerless auto-disabled rows with `quota_type="plan"` and
  `quota_domain == channel.GetTag()`.

Return no shared key for all other rows so controller selection treats them as
unique channels. Reuse the same classifier in service recovery.

- [ ] **Step 4: Enforce ownership and rotation behavior**

Restrict the disable sweep to enabled rows and same-marker auto-disabled rows.
During marked recovery, compare the persisted marker with the marker computed
from the recovering row's current single credential. On mismatch, enable and
clear only the recovering row.

- [ ] **Step 5: Verify focused GREEN**

Run:

```bash
go test ./service -run '^(TestDisablePlanQuotaCredentialDomain|TestLegacyPlanQuotaDomainRecoveryByTag|TestEnablePlanQuotaDomainAfterCredentialRotation)$' -count=1
go test ./controller -run '^TestSelectChannelsForAutomaticTestDeduplicatesDuePlanDomain$' -count=1
```

Expected: PASS.

### Task 7: Documentation and Quality Gates

**Files:**

- Modify: `docs/superpowers/specs/2026-09-23-monthly-plan-quota-isolation-design.md`
- Modify: `docs/superpowers/plans/2026-09-23-monthly-plan-quota-isolation.md`
- Verify: `model/channel.go`
- Verify: `service/channel.go`
- Verify: `service/channel_quota_test.go`

- [ ] **Step 1: Correct design and plan documentation**

Document that shared-domain isolation applies only to single-key Plan channels,
matching is case-sensitive in Go, disable ownership is preserved, recovery
uses `quota_domain_id`, legacy fallback requires explicit Plan quota metadata,
passive tests deduplicate by recovery domain, and credential rotation cannot
recover peers under the old marker.

- [ ] **Step 2: Format and inspect the diff**

Run:

```bash
gofmt -w service/channel.go service/channel_quota_test.go
git diff --check
```

Expected: no formatting or whitespace errors.

- [ ] **Step 3: Run required tests**

Run:

```bash
go test ./service ./controller ./model -count=1
go test ./controller -run '^TestSelectChannelsForAutomaticTestDeduplicatesDuePlanDomain$' -count=1
```

Expected: PASS.

- [ ] **Step 4: Commit the correction**

Run:

```bash
git add model/channel.go service/channel.go service/channel_quota_test.go \
  docs/superpowers/specs/2026-09-23-monthly-plan-quota-isolation-design.md \
  docs/superpowers/plans/2026-09-23-monthly-plan-quota-isolation.md
git commit -m "fix(channel): scope plan quota recovery domains"
```
