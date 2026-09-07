export const AUTH_REFRESH_SKEW_MS = 60 * 1000

export function shouldRefreshAuthSession(session, now = Date.now()) {
  if (!session?.accessToken) return true
  const expiresAt = Number(session.expiresAt)
  return (
    Number.isFinite(expiresAt) &&
    expiresAt > 0 &&
    expiresAt <= now + AUTH_REFRESH_SKEW_MS
  )
}

export function parseAuthRefreshResponse(payload) {
  const data =
    payload && typeof payload === 'object' && 'data' in payload
      ? payload.data
      : payload
  const accessToken = String(data?.access_token || '').trim()
  const refreshToken = String(data?.refresh_token || '').trim()
  const expiresIn = Number(data?.expires_in)
  if (
    !accessToken ||
    !refreshToken ||
    !Number.isFinite(expiresIn) ||
    expiresIn <= 0
  ) {
    throw new Error('invalid auth refresh response')
  }
  return {
    accessToken,
    refreshToken,
    expiresAt: Date.now() + expiresIn * 1000,
  }
}
