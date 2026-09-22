# Monthly Plan Quota Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automatically isolate every Plan channel sharing a credential when an upstream monthly quota is exhausted, then recover through the existing reset-time workflow.

**Architecture:** Extend the existing Plan quota classifier instead of treating all 429 responses as fatal. Add a model query for exact credential matches, filter the result to Plan channels in the service layer, and preserve tag-based passive recovery metadata.

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

### Task 2: Isolate the Shared Plan Credential Domain

**Files:**
- Modify: `model/channel.go`
- Modify: `service/channel.go`
- Modify: `service/channel_quota_test.go`

- [ ] **Step 1: Write the failing credential-domain test**

Create Plan channels that cover these cases:

```go
channels := []model.Channel{
	{Id: 11, Name: "messages", Key: "shared", Status: common.ChannelStatusEnabled, Tag: planTag, AutoBan: &autoBan},
	{Id: 12, Name: "responses", Key: "shared", Status: common.ChannelStatusEnabled, Tag: planTag, AutoBan: &autoBan},
	{Id: 13, Name: "native", Key: "shared", Status: common.ChannelStatusEnabled, Tag: nativeTag, AutoBan: &autoBan},
	{Id: 14, Name: "other-account", Key: "other", Status: common.ChannelStatusEnabled, Tag: planTag, AutoBan: &autoBan},
	{Id: 15, Name: "ordinary", Key: "shared", Status: common.ChannelStatusEnabled, Tag: ordinaryTag, AutoBan: &autoBan},
}
```

Call the credential-domain disable path using channel 11. Assert that channels
11, 12, and 13 are auto-disabled while 14 and 15 remain enabled. Assert the
disabled channels have disabled abilities and preserve their own tag in
`quota_domain`.

- [ ] **Step 2: Run the credential-domain test and verify RED**

Run:

```bash
go test ./service -run TestDisablePlanQuotaCredentialDomain -count=1
```

Expected: FAIL because the existing implementation selects only one tag.

- [ ] **Step 3: Add the exact-key model query**

Add:

```go
func GetChannelsByKey(key string, selectAll bool) ([]*Channel, error) {
	var channels []*Channel
	query := DB.Where(commonKeyCol+" = ?", key)
	if !selectAll {
		query = query.Omit("key")
	}
	err := query.Find(&channels).Error
	return channels, err
}
```

- [ ] **Step 4: Implement Plan-only credential filtering**

Pass the failing `*model.Channel` into the disable function, load exact-key
matches, retain only `plan:` channels, and store each selected channel's own
tag in `quota_domain`. Keep `disabled_until=resetAt+60`.

- [ ] **Step 5: Run service and model tests**

Run:

```bash
go test ./service ./model -count=1
```

Expected: PASS.

### Task 3: Regression and Quality Gates

**Files:**
- Verify: `service/channel.go`
- Verify: `service/channel_quota_test.go`
- Verify: `model/channel.go`

- [ ] **Step 1: Format and inspect the diff**

Run:

```bash
gofmt -w service/channel.go service/channel_quota_test.go model/channel.go
git diff --check
git diff -- service/channel.go service/channel_quota_test.go model/channel.go
```

Expected: no formatting or whitespace errors; diff contains only quota
classification and credential-domain isolation.

- [ ] **Step 2: Run the complete backend suite**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 3: Run three uncached focused regression rounds**

Run the following command three times:

```bash
go test ./service ./model -run 'PlanQuota|ChannelsByKey' -count=1
```

Expected: all three rounds PASS.

- [ ] **Step 4: Commit the implementation**

Run:

```bash
git add model/channel.go service/channel.go service/channel_quota_test.go
git commit -m "fix(channel): isolate monthly plan quota domains"
```

### Task 4: Production Audit Evidence

**Files:**
- Create: `reports/monthly-plan-quota-isolation-20260923/audit.md`
- Create: `reports/monthly-plan-quota-isolation-20260923/SHA256SUMS`

- [ ] **Step 1: Record the operational mutation**

Document the production revision, affected channel IDs, reset timestamps,
pre-change backup location, transaction preconditions, post-change statuses,
ability counts, and cache synchronization evidence. Do not include credentials,
cookies, session secrets, or database passwords.

- [ ] **Step 2: Hash local and remote evidence**

Hash the Compose snapshot, extension ZIP, database dump, pre/post channel
snapshots, audit report, and implementation commit diff.

- [ ] **Step 3: Verify production remains healthy**

Confirm:

```text
new-api health=healthy
restart count=0
channels 4,66,96 status=3
enabled abilities for channels 4,66,96 = 0
no post-mutation requests use channels 4,66,96
```
