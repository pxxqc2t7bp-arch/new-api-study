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
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { ErrorState } from '@/components/error-state'
import { NavGroup } from '@/components/layout/components/nav-group'
import { SidebarProvider } from '@/components/ui/sidebar'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { resolveAppNavigation, useAppSidebarData } from '../api'
import type { AppNavigationItem, AppSurface } from '../types'

const seedance: AppNavigationItem = {
  key: 'seedance-repro',
  name: { en: 'Seedance' },
  version: '1.0.0',
  enabled_surfaces: ['embedded'],
  dashboard_path: '/apps/seedance-repro',
  direct_url: '',
  granted_scopes: ['task.read'],
}

let client: QueryClient

beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  useAuthStore.getState().auth.setBundle({
    access_token: 'dashboard-test-access',
    token_type: 'Bearer',
    access_expires_at: 2_000_000_000,
    user: { id: 1, username: 'alice', role: 1 },
    session: {
      sid: 'navigation-test-session',
      current: true,
      login_method: 'password',
      ip: '127.0.0.1',
      user_agent: 'vitest',
      created_at: 1,
      last_active_at: 1,
      expires_at: 2_000_000_000,
    },
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

function Navigation() {
  const navigation = useAppSidebarData()
  if (navigation.error) {
    return <ErrorState description={navigation.error.message} />
  }
  return navigation.navGroups.map((group) => (
    <NavGroup key={group.id ?? group.title} {...group} />
  ))
}

async function renderNavigation(apps: AppNavigationItem[]) {
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/app_plugins') {
      return { data: { success: true, message: '', data: apps } }
    }
    if (url === '/api/status') {
      return {
        data: {
          success: true,
          message: '',
          data: {
            chats: [{ 'Infinite Canvas': 'https://canvas.example.test/' }],
          },
        },
      }
    }
    throw new Error(`Unexpected GET ${url}`)
  })
  const router = createRouter({
    routeTree: createRootRoute({ component: Navigation }),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  await router.load()
  render(
    <QueryClientProvider client={client}>
      <SidebarProvider>
        <RouterProvider router={router} />
      </SidebarProvider>
    </QueryClientProvider>
  )
}

test('renders only server-approved app surfaces', async () => {
  await renderNavigation([seedance])

  expect(await screen.findByRole('link', { name: 'Seedance' })).toHaveAttribute(
    'href',
    '/apps/seedance-repro'
  )
  expect(
    screen
      .getAllByRole('link')
      .filter((link) => link.getAttribute('href')?.startsWith('/apps/'))
  ).toHaveLength(1)
  expect(
    document.querySelector('a[href*="surface=direct"]')
  ).not.toBeInTheDocument()
})

test('keeps Infinite Canvas and Seedance as separate navigation items', async () => {
  await renderNavigation([seedance])
  const appLink = await screen.findByRole('link', { name: 'Seedance' })
  await userEvent
    .setup()
    .click(await screen.findByRole('button', { name: 'Chat' }))
  const canvasLink = await screen.findByRole('link', {
    name: 'Infinite Canvas',
  })

  expect(appLink).toHaveAttribute('href', '/apps/seedance-repro')
  expect(canvasLink).toHaveAttribute('href', '/chat/0')
  expect(appLink.closest('li')).not.toBe(canvasLink.closest('li'))
})

test('rejects unregistered app navigation', () => {
  const rejected: Array<[string, AppSurface]> = [
    ['/apps/unregistered', 'embedded'],
    ['/keys', 'embedded'],
    ['https://attacker.example/apps/seedance-repro', 'embedded'],
    ['//attacker.example/apps/seedance-repro', 'embedded'],
    ['/apps/seedance-repro', 'direct'],
  ]
  for (const [path, surface] of rejected) {
    expect(resolveAppNavigation([seedance], path, surface)).toBeNull()
  }
  expect(
    resolveAppNavigation(
      [{ ...seedance, dashboard_path: '/keys' }],
      '/keys',
      'embedded'
    )
  ).toBeNull()
  expect(
    resolveAppNavigation(
      [{ ...seedance, enabled_surfaces: [] }],
      '/apps/seedance-repro',
      'embedded'
    )
  ).toBeNull()
  expect(
    resolveAppNavigation([seedance], '/apps/seedance-repro', 'embedded')
  ).toEqual(seedance)
})
