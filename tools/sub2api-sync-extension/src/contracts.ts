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
