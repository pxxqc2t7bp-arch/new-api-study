import assert from 'node:assert/strict'
import test from 'node:test'

import {
  AUTH_REFRESH_SKEW_MS,
  parseAuthRefreshResponse,
  shouldRefreshAuthSession,
} from '../dist/auth-session.js'

test('refreshes missing and expiring access tokens', () => {
  const now = 1_000_000
  assert.equal(shouldRefreshAuthSession({}, now), true)
  assert.equal(
    shouldRefreshAuthSession(
      { accessToken: 'access', expiresAt: now + AUTH_REFRESH_SKEW_MS },
      now
    ),
    true
  )
  assert.equal(
    shouldRefreshAuthSession(
      { accessToken: 'access', expiresAt: now + AUTH_REFRESH_SKEW_MS + 1 },
      now
    ),
    false
  )
  assert.equal(
    shouldRefreshAuthSession({ accessToken: 'legacy', expiresAt: 0 }, now),
    false
  )
})

test('accepts a complete rotated session response', () => {
  const before = Date.now()
  const session = parseAuthRefreshResponse({
    code: 0,
    data: {
      access_token: 'new-access',
      refresh_token: 'new-refresh',
      expires_in: 3600,
    },
  })

  assert.equal(session.accessToken, 'new-access')
  assert.equal(session.refreshToken, 'new-refresh')
  assert.ok(session.expiresAt >= before + 3_600_000)
})

test('rejects incomplete refresh responses', () => {
  assert.throws(
    () =>
      parseAuthRefreshResponse({
        code: 0,
        data: { access_token: 'new-access', expires_in: 3600 },
      }),
    /invalid auth refresh response/
  )
})
