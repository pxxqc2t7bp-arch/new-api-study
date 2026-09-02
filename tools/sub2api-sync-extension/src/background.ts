import {
  asArray,
  COMMAND_ALARM,
  DAILY_ALARM,
  finiteNumber,
  normalizeHealth,
  normalizeURL,
  randomId,
  sanitizeError,
  sha256,
  SNAPSHOT_SCHEMA_VERSION,
  SYNC_ALARM,
  unwrap,
} from './contracts.js'
import { ebondAdapter } from './site-adapters/ebond.js'
import { hualongAdapter } from './site-adapters/hualong.js'
import { leyiAdapter } from './site-adapters/leyi.js'

const sources = [leyiAdapter, hualongAdapter, ebondAdapter]
let syncPromise

chrome.runtime.onInstalled.addListener(async () => {
  await scheduleAlarms()
  chrome.runtime.openOptionsPage()
})

chrome.runtime.onStartup.addListener(async () => {
  await scheduleAlarms()
  const { lastSyncAt = 0 } = await chrome.storage.local.get('lastSyncAt')
  if (Date.now() - Number(lastSyncAt) > 4 * 60 * 60 * 1000) {
    await runSync()
  } else {
    await pollCommands()
  }
})

chrome.action.onClicked.addListener(() => chrome.runtime.openOptionsPage())

chrome.alarms.onAlarm.addListener(async (alarm) => {
  if (alarm.name === SYNC_ALARM || alarm.name === DAILY_ALARM) {
    await runSync()
    if (alarm.name === DAILY_ALARM) await scheduleDailyAlarm()
    return
  }
  if (alarm.name === COMMAND_ALARM) await pollCommands()
})

chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (message?.type !== 'upstream:sync-now') return false
  runSync()
    .then(() => sendResponse({ ok: true }))
    .catch((error) => sendResponse({ ok: false, error: sanitizeError(error) }))
  return true
})

async function scheduleAlarms() {
  await chrome.alarms.create(SYNC_ALARM, {
    delayInMinutes: 1,
    periodInMinutes: 240,
  })
  await chrome.alarms.create(COMMAND_ALARM, {
    delayInMinutes: 2,
    periodInMinutes: 1,
  })
  await scheduleDailyAlarm()
}

async function scheduleDailyAlarm() {
  await chrome.alarms.create(DAILY_ALARM, {
    when: nextShanghaiThreeAM(),
  })
}

function nextShanghaiThreeAM() {
  const now = new Date()
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: 'Asia/Shanghai',
    year: 'numeric',
    month: 'numeric',
    day: 'numeric',
  }).formatToParts(now)
  const values = Object.fromEntries(parts.map((part) => [part.type, part.value]))
  let candidate = Date.UTC(
    Number(values.year),
    Number(values.month) - 1,
    Number(values.day),
    3 - 8,
    0,
    0
  )
  if (candidate <= Date.now()) candidate += 24 * 60 * 60 * 1000
  return candidate
}

async function runSync() {
  if (syncPromise) return syncPromise
  syncPromise = doSync().finally(() => {
    syncPromise = undefined
  })
  return syncPromise
}

async function doSync() {
  const config = await requireDeviceConfig()
  const snapshots = []
  for (const source of sources) {
    try {
      snapshots.push(await collectSource(source))
    } catch (error) {
      snapshots.push({
        key: source.key,
        name: source.name,
        console_url: source.origin,
        adapter_version: 'sub2api-user-v1',
        status: 'error',
        error: sanitizeError(error),
      })
    }
  }
  const payload = {
    schema_version: SNAPSHOT_SCHEMA_VERSION,
    snapshot_id: randomId(),
    device_id: config.deviceId,
    captured_at: Math.floor(Date.now() / 1000),
    sources: snapshots,
  }
  await newAPIRequest(config, '/api/upstream-orchestration/device/snapshots', {
    method: 'POST',
    body: JSON.stringify(payload),
  })
  await chrome.storage.local.set({
    lastSyncAt: Date.now(),
    lastSyncError: '',
  })
  await pollCommands(config)
}

async function collectSource(source) {
  const authToken = await getSiteAuthToken(source)
  if (!authToken) throw new Error(`${source.name}: login required`)
  const request = (path, options) => siteRequest(source, authToken, path, options)
  const [me, settings, groupsResponse, ratesResponse, channelsResponse, monitorsResponse, keysResponse, subscriptionsResponse] =
    await Promise.all([
      request('/api/v1/auth/me'),
      request('/api/v1/settings/public'),
      request('/api/v1/groups/available'),
      request('/api/v1/groups/rates'),
      request('/api/v1/channels/available').catch(() => null),
      request('/api/v1/channel-monitors').catch(() => null),
      request('/api/v1/keys?page=1&page_size=100&sort_by=created_at&sort_order=desc'),
      request('/api/v1/subscriptions/active').catch(() => null),
    ])
  const groups = normalizeGroups(
    source,
    groupsResponse,
    ratesResponse,
    channelsResponse,
    monitorsResponse
  )
  const keys = await normalizeKeys(keysResponse)
  mergeSubscriptionUsage(keys, subscriptionsResponse)
  const probeKey = firstUsableRawKey(keysResponse)
  const endpoints = await measureEndpoints(extractEndpoints(settings), probeKey)
  return {
    key: source.key,
    name: source.name,
    console_url: source.origin,
    adapter_version: String(unwrap(settings)?.version || 'sub2api-user-v1'),
    status: 'operational',
    balance: finiteNumber(
      unwrap(me)?.balance ??
        unwrap(me)?.available_balance ??
        unwrap(me)?.wallet_balance
    ),
    api_base_url: endpoints[0]?.url || source.origin,
    endpoint_candidates: endpoints,
    groups,
    monitors: normalizeMonitors(monitorsResponse),
    keys,
  }
}

function firstUsableRawKey(keysResponse) {
  for (const key of asArray(unwrap(keysResponse))) {
    const rawKey = String(key.key ?? key.api_key ?? '')
    const status = String(key.status ?? '').toLowerCase()
    if (rawKey && !['disabled', 'expired', 'revoked'].includes(status)) return rawKey
  }
  return ''
}

async function measureEndpoints(endpoints, apiKey) {
  if (!apiKey) return endpoints
  return Promise.all(
    endpoints.map(async (endpoint) => {
      const controller = new AbortController()
      const timeout = setTimeout(() => controller.abort(), 5000)
      const startedAt = performance.now()
      try {
        const response = await fetch(`${endpoint.url}/v1/models`, {
          headers: { Authorization: `Bearer ${apiKey}` },
          signal: controller.signal,
        })
        return {
          ...endpoint,
          healthy: response.ok,
          latency_ms: Math.round(performance.now() - startedAt),
        }
      } catch {
        return {
          ...endpoint,
          healthy: false,
          latency_ms: Math.round(performance.now() - startedAt),
        }
      } finally {
        clearTimeout(timeout)
      }
    })
  )
}

async function getSiteAuthToken(source) {
  let tabs = await chrome.tabs.query({ url: `${source.origin}/*` })
  let createdTab
  if (tabs.length === 0) {
    createdTab = await chrome.tabs.create({
      url: `${source.origin}/dashboard`,
      active: false,
    })
    await waitForTab(createdTab.id)
    tabs = [createdTab]
  }
  try {
    const response = await chrome.tabs.sendMessage(tabs[0].id, {
      type: 'upstream:get-auth-token',
    })
    return String(response?.token || '')
  } finally {
    if (createdTab?.id) await chrome.tabs.remove(createdTab.id).catch(() => {})
  }
}

async function waitForTab(tabId) {
  for (let attempt = 0; attempt < 40; attempt += 1) {
    const tab = await chrome.tabs.get(tabId)
    if (tab.status === 'complete') return
    await new Promise((resolve) => setTimeout(resolve, 250))
  }
  throw new Error('site page did not finish loading')
}

async function siteRequest(source, authToken, path, options = {}) {
  const response = await fetch(`${source.origin}${path}`, {
    ...options,
    headers: {
      Accept: 'application/json',
      Authorization: `Bearer ${authToken}`,
      'Content-Type': 'application/json',
      ...(options.headers || {}),
    },
  })
  const payload = await response.json().catch(() => null)
  if (!response.ok) {
    throw new Error(
      `${source.name} ${path}: ${payload?.message || `HTTP ${response.status}`}`
    )
  }
  return payload
}

function normalizeGroups(source, groupsResponse, ratesResponse, channelsResponse, monitorsResponse) {
  const rates = new Map(
    asArray(unwrap(ratesResponse)).map((rate) => [
      String(rate.group_id ?? rate.id),
      rate,
    ])
  )
  const modelsByGroup = collectModelsByGroup(channelsResponse)
  const monitors = normalizeMonitors(monitorsResponse)
  return asArray(unwrap(groupsResponse))
    .map((group) => {
      const externalId = String(group.id ?? group.group_id ?? '')
      const rate = rates.get(externalId) || {}
      const name = String(group.name ?? group.group_name ?? '')
      const platform = normalizePlatform(source, group.platform ?? group.provider ?? name)
      const monitor = findMonitor(monitors, group, platform)
      const models = normalizeModelNames(
        group.supported_models ??
          group.models ??
          rate.supported_models ??
          modelsByGroup.get(externalId) ??
          []
      )
      return {
        external_id: externalId,
        name,
        platform,
        subscription_type: String(group.subscription_type || ''),
        rate_multiplier: finiteNumber(
          rate.rate_multiplier ?? group.rate_multiplier ?? group.multiplier
        ),
        user_rate_multiplier: finiteNumber(
          rate.user_rate_multiplier ?? group.user_rate_multiplier
        ),
        peak_rate_enabled: Boolean(rate.peak_rate_enabled ?? group.peak_rate_enabled),
        peak_start: String(rate.peak_start || group.peak_start || ''),
        peak_end: String(rate.peak_end || group.peak_end || ''),
        peak_rate_multiplier: finiteNumber(
          rate.peak_rate_multiplier ?? group.peak_rate_multiplier
        ),
        is_exclusive: Boolean(group.is_exclusive),
        monitor_external_id: monitor?.external_id || '',
        health_status: monitor?.status || 'unknown',
        availability: monitor?.availability,
        latency_ms: monitor?.latency_ms,
        endpoint_ping_ms: monitor?.endpoint_ping_ms,
        models: models.map((model) => ({ name: model, platform })),
      }
    })
    .filter(
      (group) =>
        group.external_id &&
        group.name &&
        group.platform &&
        group.rate_multiplier > 0 &&
        group.models.length > 0
    )
}

function collectModelsByGroup(channelsResponse) {
  const result = new Map()
  for (const channel of asArray(unwrap(channelsResponse))) {
    for (const group of asArray(channel.groups ?? channel.available_groups)) {
      const groupId = String(group.id ?? group.group_id ?? '')
      const models = normalizeModelNames(
        group.supported_models ?? channel.supported_models ?? channel.models ?? []
      )
      result.set(groupId, [...new Set([...(result.get(groupId) || []), ...models])])
    }
  }
  return result
}

function normalizeMonitors(monitorsResponse) {
  return asArray(unwrap(monitorsResponse)).map((monitor) => ({
    external_id: String(monitor.id ?? monitor.monitor_id ?? ''),
    name: String(monitor.name ?? monitor.channel_name ?? ''),
    provider: String(monitor.provider ?? monitor.platform ?? ''),
    model: String(monitor.model ?? monitor.test_model ?? ''),
    status: normalizeHealth(
      monitor.primary_status ?? monitor.status ?? monitor.state
    ),
    availability: finiteNumber(
      monitor.availability_7d ?? monitor.availability ?? monitor.success_rate
    ),
    latency_ms: finiteNumber(
      monitor.chat_latency_ms ?? monitor.latency_ms ?? monitor.first_token_ms
    ),
    endpoint_ping_ms: finiteNumber(
      monitor.endpoint_ping_ms ?? monitor.ping_ms
    ),
    observed_at: Math.floor(Date.now() / 1000),
  }))
}

function findMonitor(monitors, group, platform) {
  const groupId = String(group.id ?? group.group_id ?? '')
  const groupName = String(group.name ?? group.group_name ?? '').toLowerCase()
  return monitors.find((monitor) => {
    if (monitor.group_id && String(monitor.group_id) === groupId) return true
    const monitorName = monitor.name.toLowerCase()
    return (
      normalizePlatform({ platformAliases: {} }, monitor.provider) === platform &&
      (monitorName.includes(groupName) || groupName.includes(monitorName))
    )
  })
}

async function normalizeKeys(keysResponse) {
  const result = []
  for (const key of asArray(unwrap(keysResponse))) {
    const rawKey = String(key.key ?? key.api_key ?? '')
    if (!rawKey) continue
    result.push({
      external_id: String(key.id ?? key.key_id ?? ''),
      name: String(key.name ?? ''),
      group_external_id: String(key.group_id ?? key.group?.id ?? ''),
      status: String(key.status ?? ''),
      fingerprint: await sha256(rawKey),
      input_tokens: finiteNumber(key.input_tokens),
      output_tokens: finiteNumber(key.output_tokens),
      cached_tokens: finiteNumber(key.cached_tokens),
      total_tokens: finiteNumber(key.total_tokens),
      usage_5h: finiteNumber(key.usage_5h),
      limit_5h: finiteNumber(key.rate_limit_5h),
      reset_5h_at: finiteNumber(key.reset_5h_at),
      usage_1d: finiteNumber(key.usage_1d),
      limit_1d: finiteNumber(key.rate_limit_1d),
      reset_1d_at: finiteNumber(key.reset_1d_at),
      usage_7d: finiteNumber(key.usage_7d),
      limit_7d: finiteNumber(key.rate_limit_7d),
      reset_7d_at: finiteNumber(key.reset_7d_at),
      usage_30d: finiteNumber(key.usage_30d ?? key.cost_30d),
      data_quality: 'reported',
    })
  }
  return result
}

function mergeSubscriptionUsage(keys, subscriptionsResponse) {
  const subscription = unwrap(subscriptionsResponse)
  if (!subscription) return
  for (const key of keys) {
    if (key.limit_30d == null) {
      key.limit_30d = finiteNumber(
        subscription.monthly_limit ?? subscription.quota
      )
    }
    key.reset_30d_at = finiteNumber(
      subscription.current_period_end ?? subscription.expires_at
    )
  }
}

function normalizePlatform(source, value) {
  const normalized = String(value ?? '').trim().toLowerCase()
  for (const [alias, platform] of Object.entries(source.platformAliases || {})) {
    if (normalized.includes(alias)) return platform
  }
  if (normalized.includes('openai') || normalized.includes('gpt')) return 'openai'
  if (normalized.includes('anthropic') || normalized.includes('claude')) {
    return 'anthropic'
  }
  if (normalized.includes('grok') || normalized.includes('xai')) return 'grok'
  return ''
}

function normalizeModelNames(value) {
  const values = Array.isArray(value)
    ? value
    : typeof value === 'string'
      ? value.split(',')
      : []
  return [...new Set(values.map((item) => String(item?.name ?? item).trim()).filter(Boolean))]
}

function extractEndpoints(settingsResponse) {
  const settings = unwrap(settingsResponse) || {}
  const candidates = []
  const add = (name, value) => {
    const url = normalizeURL(value)
    if (url && !candidates.some((candidate) => candidate.url === url)) {
      candidates.push({ name, url })
    }
  }
  add('default', settings.api_base_url ?? settings.api_endpoint)
  for (const item of asArray(
    settings.api_endpoints ??
      settings.custom_endpoints ??
      settings.endpoint_options
  )) {
    add(String(item.name ?? item.label ?? 'endpoint'), item.url ?? item.value)
  }
  return candidates
}

async function pollCommands(existingConfig) {
  const config = existingConfig || (await requireDeviceConfig())
  const payload = await newAPIRequest(
    config,
    '/api/upstream-orchestration/device/commands'
  )
  for (const command of asArray(payload)) {
    try {
      if (command.type === 'sync') {
        if (!syncPromise) await runSync()
        await modelCompleteCommand(config, command, true, {})
        continue
      }
      if (command.type === 'enroll') {
        await executeEnrollment(config, command)
        continue
      }
      if (command.type === 'revoke') {
        await executeRevocation(config, command)
      }
    } catch (error) {
      if (command.type === 'enroll') {
        await completeEnrollment(config, command, {
          success: false,
          error: sanitizeError(error),
        }).catch(() => {})
      }
    }
  }
}

async function executeEnrollment(config, command) {
  const payload =
    typeof command.payload === 'string'
      ? JSON.parse(command.payload)
      : command.payload
  const source = sources.find((item) => item.key === payload.source_key)
  if (!source) throw new Error(`unknown source ${payload.source_key}`)
  const token = await getSiteAuthToken(source)
  if (!token) throw new Error(`${source.name}: login required`)
  const existingKeys = asArray(
    unwrap(
      await siteRequest(
        source,
        token,
        '/api/v1/keys?page=1&page_size=100&sort_by=created_at&sort_order=desc'
      )
    )
  )
  let response = existingKeys.find(
    (key) =>
      String(key.name ?? '') === payload.key_name &&
      String(key.group_id ?? key.group?.id ?? '') ===
        String(payload.external_group_id) &&
      String(key.key ?? key.api_key ?? '').startsWith('sk-') &&
      !String(key.key ?? key.api_key ?? '').includes('...')
  )
  if (!response) {
    response = unwrap(
      await siteRequest(source, token, '/api/v1/keys', {
        method: 'POST',
        body: JSON.stringify({
          name: payload.key_name,
          group_id: payload.external_group_id,
          ...(payload.ip_whitelist?.length
            ? { ip_whitelist: payload.ip_whitelist }
            : {}),
        }),
      })
    )
  }
  const rawKey = String(response?.key ?? response?.api_key ?? '')
  if (!rawKey) throw new Error('created key was not returned by Sub2API')
  await completeEnrollment(config, command, {
    success: true,
    source_key: payload.source_key,
    external_group_id: payload.external_group_id,
    external_key_id: String(response?.id ?? response?.key_id ?? ''),
    key_fingerprint: await sha256(rawKey),
    api_key: rawKey,
  })
}

async function executeRevocation(config, command) {
  const payload =
    typeof command.payload === 'string'
      ? JSON.parse(command.payload)
      : command.payload
  const source = sources.find((item) => item.key === command.source_key)
  if (!source) throw new Error(`unknown source ${command.source_key}`)
  const token = await getSiteAuthToken(source)
  await siteRequest(
    source,
    token,
    `/api/v1/keys/${encodeURIComponent(payload.external_key_id)}`,
    { method: 'DELETE' }
  )
  await modelCompleteCommand(config, command, true, {})
}

async function completeEnrollment(config, command, result) {
  return newAPIRequest(
    config,
    `/api/upstream-orchestration/device/enrollments/${encodeURIComponent(command.command_id)}/result`,
    {
      method: 'POST',
      body: JSON.stringify({
        command_id: command.command_id,
        source_key: command.source_key,
        ...result,
      }),
    }
  )
}

async function modelCompleteCommand(config, command, success, result) {
  if (command.type === 'enroll') {
    return completeEnrollment(config, command, { success, ...result })
  }
  return newAPIRequest(
    config,
    `/api/upstream-orchestration/device/commands/${encodeURIComponent(command.command_id)}/result`,
    {
      method: 'POST',
      body: JSON.stringify({
        success,
        result: JSON.stringify(result || {}),
      }),
    }
  )
}

async function requireDeviceConfig() {
  const config = await chrome.storage.local.get([
    'serverUrl',
    'deviceId',
    'deviceToken',
  ])
  if (!config.serverUrl || !config.deviceId || !config.deviceToken) {
    throw new Error('extension is not paired')
  }
  return config
}

async function newAPIRequest(config, path, options = {}) {
  const response = await fetch(`${config.serverUrl}${path}`, {
    ...options,
    headers: {
      Accept: 'application/json',
      Authorization: `Bearer ${config.deviceToken}`,
      'Content-Type': 'application/json',
      ...(options.headers || {}),
    },
  })
  const payload = await response.json().catch(() => null)
  if (!response.ok || payload?.success === false) {
    const message = payload?.message || `New API HTTP ${response.status}`
    await chrome.storage.local.set({ lastSyncError: sanitizeError(message) })
    throw new Error(message)
  }
  return payload?.data
}
