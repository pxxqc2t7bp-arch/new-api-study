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
import type { SidebarData } from '@/components/layout/types'

import type {
  AppAuthorizeRequest,
  AppAuthorizeResult,
  AppLaunchContext,
  AppNavigationItem,
  AppSurface,
} from './types'

// RED-only boundary for GET /api/app_plugins through the shared api client.
export async function listAppPlugins(
  _signal?: AbortSignal
): Promise<AppNavigationItem[]> {
  throw new Error('app_navigation_unsupported')
}

// Only the authenticated server can resolve a surface's start URL and origin.
export async function getAppLaunchContext(
  _appKey: string,
  _surface: AppSurface,
  _signal?: AbortSignal
): Promise<AppLaunchContext> {
  throw new Error('embedded_auth_unsupported')
}

export async function authorizeApp(
  _appKey: string,
  _request: AppAuthorizeRequest,
  _idempotencyKey: string,
  _signal?: AbortSignal
): Promise<AppAuthorizeResult> {
  throw new Error('embedded_auth_unsupported')
}

export function resolveAppNavigation(
  _apps: AppNavigationItem[],
  _dashboardPath: string,
  _surface: AppSurface
): AppNavigationItem | null {
  throw new Error('app_navigation_unsupported')
}

// The GREEN phase will compose useSidebarData with session-scoped App data here.
export function useAppSidebarData(): SidebarData & { error: Error | null } {
  return {
    navGroups: [],
    error: new Error('app_navigation_unsupported'),
  }
}
