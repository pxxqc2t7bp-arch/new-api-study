/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { AppFrame } from '../app-frame'
import type { AppLaunchContext, EmbeddedReadyMessage } from '../types'

const context: AppLaunchContext = {
  app_key: 'seedance-repro',
  surface: 'embedded',
  start_url: 'https://seedance.example.test/tenant/demo/start-embedded',
  origin: 'https://seedance.example.test',
}
const ready: EmbeddedReadyMessage = {
  type: 'seedance.auth.ready',
  version: 1,
  transaction_id: 'transaction-1',
  state: 's'.repeat(43),
  nonce: 'n'.repeat(43),
  code_challenge: 'c'.repeat(43),
}
const launchURL = `https://seedance.example.test/tenant/demo/callback?code=one-time-code&state=${ready.state}`

let client: QueryClient

beforeEach(() => {
  client = new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0 },
      mutations: { retry: false },
    },
  })
  useAuthStore.getState().auth.setBundle({
    access_token: 'dashboard-test-access',
    token_type: 'Bearer',
    access_expires_at: 2_000_000_000,
    user: { id: 1, username: 'alice', role: 1 },
    session: {
      sid: 'embedded-test-session',
      current: true,
      login_method: 'password',
      ip: '127.0.0.1',
      user_agent: 'vitest',
      created_at: 1,
      last_active_at: 1,
      expires_at: 2_000_000_000,
    },
  })
  vi.spyOn(api, 'get').mockImplementation(async (url, config) => {
    if (url === '/api/app_plugins') {
      return {
        data: {
          success: true,
          message: '',
          data: [
            {
              key: 'seedance-repro',
              name: { en: 'Seedance' },
              version: '1.0.0',
              enabled_surfaces: ['embedded'],
              dashboard_path: '/apps/seedance-repro',
              direct_url: '',
              granted_scopes: ['task.read'],
            },
          ],
        },
      }
    }
    if (
      url === '/api/app_plugins/seedance-repro/launch-context' &&
      config?.params?.surface === 'embedded'
    ) {
      return { data: { success: true, message: '', data: context } }
    }
    throw new Error(`Unexpected GET ${url}`)
  })
})

afterEach(() => {
  cleanup()
  client.clear()
  useAuthStore.getState().auth.reset()
  localStorage.clear()
  sessionStorage.clear()
  vi.restoreAllMocks()
})

async function renderFrame(): Promise<HTMLIFrameElement> {
  render(
    <QueryClientProvider client={client}>
      <AppFrame appKey='seedance-repro' />
    </QueryClientProvider>
  )
  return screen.findByTitle<HTMLIFrameElement>('Seedance')
}

async function sendMessage(
  frame: HTMLIFrameElement,
  init: MessageEventInit = {}
) {
  await act(async () => {
    window.dispatchEvent(
      new MessageEvent('message', {
        origin: context.origin,
        source: frame.contentWindow,
        data: ready,
        ...init,
      })
    )
  })
}

function authorizeResponse() {
  return vi.spyOn(api, 'post').mockImplementation(async (url) => {
    if (url !== '/api/app_plugins/seedance-repro/authorize') {
      throw new Error(`Unexpected POST ${url}`)
    }
    return {
      data: {
        success: true,
        message: '',
        data: { launch_url: launchURL, surface: 'embedded', expires_in: 60 },
      },
    }
  })
}

test('rejects forged embedded handshake messages', async () => {
  const post = authorizeResponse()
  const frame = await renderFrame()
  const forged: MessageEventInit[] = [
    { origin: 'https://seedance.example.test.attacker.example' },
    { source: window },
    { data: null },
    { data: { ...ready, version: 2 } },
    { data: { ...ready, transaction_id: '' } },
    { data: { ...ready, nonce: undefined } },
    { data: { ...ready, code_challenge: '' } },
    { data: { ...ready, launch_url: 'https://attacker.example/' } },
  ]
  for (const message of forged) {
    await sendMessage(frame, message)
    expect(frame).toHaveAttribute('src', context.start_url)
    expect(post).not.toHaveBeenCalled()
  }

  await sendMessage(frame)
  await waitFor(() => expect(frame).toHaveAttribute('src', launchURL))
})

test('navigates the iframe to the one-time launch URL without postMessage secrets', async () => {
  const post = authorizeResponse()
  const frame = await renderFrame()
  expect(frame).toHaveAttribute('src', context.start_url)
  const source = frame.contentWindow
  if (!source) throw new Error('Expected the current iframe browsing context')
  const childMessages = vi.spyOn(source, 'postMessage')
  const parentMessages = vi.spyOn(window, 'postMessage')

  await sendMessage(frame)
  await waitFor(() => expect(frame).toHaveAttribute('src', launchURL))
  await sendMessage(frame)

  expect(post).toHaveBeenCalledTimes(1)
  expect(post).toHaveBeenCalledWith(
    '/api/app_plugins/seedance-repro/authorize',
    {
      surface: 'embedded',
      transaction_id: ready.transaction_id,
      state: ready.state,
      nonce: ready.nonce,
      code_challenge: ready.code_challenge,
      code_challenge_method: 'S256',
    },
    expect.objectContaining({
      headers: expect.objectContaining({
        'Idempotency-Key': expect.any(String),
      }),
      singleUseAuthorization: true,
      skipErrorHandler: true,
    })
  )
  expect(
    JSON.stringify([
      childMessages.mock.calls,
      parentMessages.mock.calls,
      client
        .getQueryCache()
        .getAll()
        .map((query) => query.state.data),
      client
        .getMutationCache()
        .getAll()
        .map((mutation) => mutation.state.data),
      { ...localStorage },
      { ...sessionStorage },
    ])
  ).not.toMatch(
    /one-time-code|launch_url|code_verifier|dashboard-test-access|AppService|AppGrant/
  )
})

test('uses the restricted iframe sandbox', async () => {
  const frame = await renderFrame()

  expect(frame).toHaveAttribute(
    'sandbox',
    'allow-scripts allow-same-origin allow-forms allow-downloads allow-modals allow-popups allow-popups-to-escape-sandbox'
  )
  expect(frame).toHaveAttribute('allow', 'clipboard-read; clipboard-write')
})
