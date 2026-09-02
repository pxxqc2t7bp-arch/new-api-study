export const SNAPSHOT_SCHEMA_VERSION = 1
export const SYNC_ALARM = 'sub2api-sync-four-hour'
export const DAILY_ALARM = 'sub2api-sync-daily'
export const COMMAND_ALARM = 'sub2api-sync-commands'

export function unwrap(response) {
  if (response == null) return null
  if (Object.hasOwn(response, 'data')) return response.data
  return response
}

export function asArray(value) {
  if (Array.isArray(value)) return value
  if (Array.isArray(value?.items)) return value.items
  if (Array.isArray(value?.data)) return value.data
  if (Array.isArray(value?.list)) return value.list
  if (Array.isArray(value?.records)) return value.records
  return []
}

export function collectGroupRates(response) {
  const value = unwrap(response)
  if (Array.isArray(value)) {
    return new Map(
      value.map((rate) => [
        String(rate.group_id ?? rate.id),
        rate,
      ])
    )
  }
  if (value && typeof value === 'object') {
    return new Map(
      Object.entries(value).map(([id, rate]) => [
        String(id),
        { user_rate_multiplier: rate },
      ])
    )
  }
  return new Map()
}

export function collectModelsByGroup(response) {
  const result = new Map()
  for (const channel of asArray(unwrap(response))) {
    const sections = asArray(channel.platforms)
    if (sections.length > 0) {
      for (const section of sections) {
        const models = normalizeModelNames(section.supported_models ?? [])
        for (const group of asArray(section.groups)) {
          addModels(result, String(group.id ?? group.group_id ?? ''), models)
        }
      }
      continue
    }
    const models = normalizeModelNames(channel.supported_models ?? channel.models ?? [])
    for (const group of asArray(channel.groups ?? channel.available_groups)) {
      addModels(result, String(group.id ?? group.group_id ?? ''), models)
    }
  }
  return result
}

export function extractModelNames(response) {
  const value = unwrap(response)
  const models = Array.isArray(value)
    ? value
    : Array.isArray(value?.models)
      ? value.models
      : []
  return normalizeModelNames(models.map((model) => model?.id ?? model?.name ?? model))
}

export function mergeModelsByGroup(primary, fallback) {
  const result = new Map(primary)
  for (const [groupId, models] of fallback) {
    addModels(result, groupId, models)
  }
  return result
}

function addModels(result, groupId, models) {
  if (!groupId) return
  result.set(groupId, [...new Set([...(result.get(groupId) || []), ...models])])
}

export function normalizeModelNames(value) {
  const values = Array.isArray(value)
    ? value
    : typeof value === 'string'
      ? value.split(',')
      : []
  return [...new Set(values.map((item) => String(item?.name ?? item).trim()).filter(Boolean))]
}

export function finiteNumber(value) {
  const parsed = Number(value)
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : undefined
}

export function normalizeHealth(value) {
  const status = String(value ?? '').trim().toLowerCase()
  if (
    ['operational', 'normal', 'healthy', 'green', 'success', 'ok', '正常'].includes(
      status
    )
  ) {
    return 'operational'
  }
  if (['degraded', 'warning', 'yellow', '降级'].includes(status)) {
    return 'degraded'
  }
  if (['failed', 'failure', 'red', '失败'].includes(status)) return 'failed'
  if (['error', 'down', 'offline', '错误'].includes(status)) return 'error'
  return 'unknown'
}

export async function sha256(value) {
  const bytes = new TextEncoder().encode(String(value))
  const digest = await crypto.subtle.digest('SHA-256', bytes)
  return Array.from(new Uint8Array(digest))
    .map((byte) => byte.toString(16).padStart(2, '0'))
    .join('')
}

export function randomId() {
  return crypto.randomUUID()
}

export function normalizeURL(value) {
  try {
    const url = new URL(String(value))
    if (url.protocol !== 'https:') return ''
    url.search = ''
    url.hash = ''
    return url.toString().replace(/\/$/, '')
  } catch {
    return ''
  }
}

export function sanitizeError(error) {
  const message = error instanceof Error ? error.message : String(error)
  return message
    .replace(/sk-[A-Za-z0-9_-]+/g, 'sk-[redacted]')
    .replace(/Bearer\s+\S+/gi, 'Bearer [redacted]')
    .slice(0, 500)
}
