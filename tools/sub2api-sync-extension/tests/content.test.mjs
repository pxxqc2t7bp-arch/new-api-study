import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'
import vm from 'node:vm'

function storage(initial = {}) {
  const values = new Map(Object.entries(initial))
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
  }
}

async function loadContent(initial = {}) {
  let listener
  const localStorage = storage(initial)
  const source = await readFile(
    new URL('../dist/content.js', import.meta.url),
    'utf8'
  )
  vm.runInNewContext(source, {
    chrome: {
      runtime: {
        onMessage: {
          addListener: (value) => {
            listener = value
          },
        },
      },
    },
    localStorage,
    sessionStorage: storage(),
  })
  return { listener, localStorage }
}

test('returns the browser-local auth session to the extension worker', async () => {
  const { listener } = await loadContent({
    auth_token: 'access',
    refresh_token: 'refresh',
    token_expires_at: '1234',
  })
  let response
  listener({ type: 'upstream:get-auth-session' }, {}, (value) => {
    response = value
  })

  assert.equal(response.accessToken, 'access')
  assert.equal(response.refreshToken, 'refresh')
  assert.equal(response.expiresAt, 1234)
})

test('commits a rotated session only when the refresh token still matches', async () => {
  const { listener, localStorage } = await loadContent({
    auth_token: 'old-access',
    refresh_token: 'old-refresh',
    token_expires_at: '1234',
  })
  let rejected
  listener(
    {
      type: 'upstream:set-auth-session',
      expectedRefreshToken: 'stale-refresh',
      accessToken: 'bad-access',
      refreshToken: 'bad-refresh',
      expiresAt: 5678,
    },
    {},
    (value) => {
      rejected = value
    }
  )
  assert.equal(rejected.updated, false)
  assert.equal(localStorage.getItem('auth_token'), 'old-access')

  let accepted
  listener(
    {
      type: 'upstream:set-auth-session',
      expectedRefreshToken: 'old-refresh',
      accessToken: 'new-access',
      refreshToken: 'new-refresh',
      expiresAt: 5678,
    },
    {},
    (value) => {
      accepted = value
    }
  )
  assert.equal(accepted.updated, true)
  assert.equal(localStorage.getItem('auth_token'), 'new-access')
  assert.equal(localStorage.getItem('refresh_token'), 'new-refresh')
  assert.equal(localStorage.getItem('token_expires_at'), '5678')
})
