# 渠道级高可用验证

本文用于在预发布环境验证渠道选择、故障切换、自动熔断和恢复。它不验证应用实例、数据库、Redis 或负载均衡器的高可用。

## 通过标准

一次测试只有同时满足以下条件才算通过：

- 客户端得到预期响应；
- Mock 观察到的上游调用顺序与渠道优先级一致；
- 渠道和 ability 状态正确持久化；
- 管理员日志中的 `attempted_channels` 和 `priority_path` 与实际调用一致；
- 成功请求只结算一次；
- 非重试错误不会访问备用渠道。

仅看到最终 HTTP 200 不算通过。

## 准备

设置唯一测试标识，所有临时资源均使用此前缀：

```bash
export HA_RUN_ID="ha-smoke-$(date +%Y%m%d-%H%M%S)"
export NEW_API_BASE="https://new-api-staging.example.com"
export NEW_API_TOKEN="<test-user-token>"
export TEST_MODEL="gpt-3.5-turbo"
```

准备专用测试用户、令牌和分组。不要使用生产用户或正式渠道。测试用户应有足够额度，但不要设置无限额度，以便核对结算变化。

记录并在结束后恢复以下系统设置：

- 自动重试状态码包含 `401`、`429`、`500`；
- 自动禁用状态码包含 `401`、`429`；
- 自动禁用渠道：开启；
- 自动启用渠道：开启；
- 定时渠道测试：开启；
- 渠道测试模式：`passive_recovery`；
- 测试间隔：临时设为 1 分钟。

Mock 服务至少提供三个独立 Base URL，并记录时间、路径和测试请求 ID：

| 名称 | 初始行为 | 响应标识 |
| --- | --- | --- |
| `primary` | 可切换为 200、400、429、500 或断开连接 | `primary` |
| `backup` | 200 | `backup` |
| `last-resort` | 200 | `last-resort` |

成功响应必须是合法的 OpenAI chat completion，并带 usage：

```json
{
  "id": "chatcmpl-ha-smoke",
  "object": "chat.completion",
  "created": 1,
  "model": "gpt-3.5-turbo",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "backup"},
      "finish_reason": "stop"
    }
  ],
  "usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}
}
```

失败响应使用：

```json
{
  "error": {
    "message": "ha smoke injected failure",
    "type": "upstream_error",
    "code": "ha_smoke"
  }
}
```

在测试分组中创建三个开启自动禁用的 OpenAI 渠道：

| 渠道 | 优先级 | Base URL |
| --- | ---: | --- |
| `${HA_RUN_ID}-primary` | 30 | Mock `primary` |
| `${HA_RUN_ID}-backup` | 20 | Mock `backup` |
| `${HA_RUN_ID}-last-resort` | 10 | Mock `last-resort` |

三个渠道只配置 `${TEST_MODEL}`，不要与正式分组或模型能力混用。

## 请求模板

每个场景使用新的请求 ID：

```bash
export REQUEST_ID="${HA_RUN_ID}-baseline"
curl --fail-with-body -sS \
  -D "/tmp/${REQUEST_ID}.headers" \
  -o "/tmp/${REQUEST_ID}.json" \
  -H "Authorization: Bearer ${NEW_API_TOKEN}" \
  -H "Content-Type: application/json" \
  -H "X-Request-ID: ${REQUEST_ID}" \
  "${NEW_API_BASE}/v1/chat/completions" \
  -d "{\"model\":\"${TEST_MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}"
cat "/tmp/${REQUEST_ID}.json"
```

如果入口网关不透传 `X-Request-ID`，使用响应中的 request ID 查询日志。

## 场景

### 1. 健康基线

将三个 Mock 均设为 200，发送一次请求。

预期：

- 响应为 200，内容来自 `primary`；
- Mock 调用顺序仅为 `primary`；
- 消费日志 `attempt_count=1`、`priority_path=[30]`、`final_tier=30`；
- 记录本次用户额度变化，作为后续切换场景的计费基线。

### 2. 500 降级

将 `primary` 设为 500，`backup` 保持 200，发送一次请求。

预期：

- 同一请求最终为 200，内容来自 `backup`；
- Mock 调用顺序严格为 `primary -> backup`，各一次；
- 日志为 `attempt_count=2`、`priority_path=[30,20]`、`final_tier=20`；
- `fallback_reason` 以 `status_500:` 开头；
- 用户额度变化等于健康基线，不是两倍。

### 3. 429 熔断

将 `primary` 设为 429，`backup` 保持 200，发送一次请求。等待异步状态更新完成后再发送第二次请求。

预期：

- 第一次请求调用 `primary -> backup` 并返回 200；
- `primary` 变为自动禁用，其 ability 的 `enabled=false`；
- 第二次请求只调用 `backup`；
- 第二次日志为 `attempt_count=1`、`priority_path=[20]`。

### 4. 非重试错误

先重新启用 `primary`，再将其设为 400，发送一次请求。

预期：

- 客户端收到 400；
- 只调用 `primary`，不调用 `backup`；
- 预扣额度最终完整退回。

### 5. 连接中断

将 `primary` 配置为接受请求后立即断开连接，`backup` 保持 200。

预期：

- 同一请求最终为 200；
- Mock 调用顺序为 `primary -> backup`；
- `fallback_reason` 包含 `do_request_failed`。

## Plan 配额域

创建两个使用同一上游账号、不同协议入口的测试渠道，并给它们相同标签：

```text
plan:${HA_RUN_ID}:shared-account
```

另建一个不同 `plan:` 标签渠道和一个普通标签渠道作为隔离对照。让域内一个 Mock 返回 429，错误消息必须包含以下格式，时间使用未来时间：

```text
You have exceeded the weekly usage quota. It will reset at <YYYY-MM-DD HH:MM:SS +0800 CST>.
```

预期：

- 同一 `plan:` 标签的渠道全部自动禁用；
- 其他 Plan 域和普通渠道保持启用；
- 域内渠道均写入相同的 `quota_domain`、`quota_type=plan`、`quota_reset_at`；
- `disabled_until=quota_reset_at+60`；
- 到达 `disabled_until` 前的 passive recovery 不调用该域上游。

恢复验证时，将代表渠道 Mock 改为 200。可等待到期，也可仅对本次测试标签将 `disabled_until` 调整为当前时间之前，再等待下一次 passive recovery 周期。

预期：

- 同一域在一个周期内只收到一次探测；
- 探测成功后域内全部渠道及 abilities 恢复；
- `disabled_until`、`quota_reset_at`、`quota_domain` 和 `quota_type` 被清除。

## 真实失效 Key

最后增加一个仅属于测试分组的高优先级真实供应商渠道：

- 名称：`${HA_RUN_ID}-real-invalid-key`；
- 优先级：30；
- Key：专用且确认无效的测试 Key；
- 自动禁用：开启；
- 优先级 20 保留 Mock 成功渠道。

发送一次非流式请求。预期真实上游返回 401，系统在同一请求中切到 Mock 备用渠道并返回 200，随后将真实失效 Key 渠道自动禁用。

不要在截图、命令历史、日志备注或仓库文件中记录真实 Key。此步骤只验证真实供应商错误能够进入已由 Mock 证明的切换链路，不用于测试真实配额耗尽。

## 证据检查

用管理员 usage log 或只读 SQL 按请求 ID 核对：

```sql
SELECT request_id, type, channel_id, quota, other
FROM logs
WHERE request_id = '<request-id>'
ORDER BY id;
```

核对渠道和 ability：

```sql
SELECT id, name, status, priority, tag, other_info
FROM channels
WHERE name LIKE 'ha-smoke-%'
ORDER BY priority DESC;

SELECT channel_id, model, enabled, priority
FROM abilities
WHERE channel_id IN (<test-channel-ids>)
ORDER BY priority DESC;
```

每个场景记录：

| 场景 | 时间 | Request ID | HTTP | 实际上游顺序 | Priority path | 渠道状态 | 额度变化 | 结论 |
| --- | --- | --- | ---: | --- | --- | --- | ---: | --- |
| 健康基线 |  |  |  |  |  |  |  |  |
| 500 降级 |  |  |  |  |  |  |  |  |
| 429 熔断 |  |  |  |  |  |  |  |  |
| 400 不重试 |  |  |  |  |  |  |  |  |
| 连接中断 |  |  |  |  |  |  |  |  |
| Plan 恢复 |  |  |  |  |  |  |  |  |
| 真实 401 |  |  |  |  |  |  |  |  |

## 清理

1. 恢复测试前记录的重试、自动禁用、自动启用、渠道测试模式和周期。
2. 删除名称或标签以 `${HA_RUN_ID}` 开头的渠道及其 abilities。
3. 删除测试令牌和测试用户。
4. 删除 Mock 行为和调用记录。
5. 确认正式分组的渠道数量、状态和能力表未发生变化。
