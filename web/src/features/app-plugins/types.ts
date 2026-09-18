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
export type AppSurface = 'direct' | 'embedded'

export type AppNavigationItem = {
  key: string
  name: Record<string, string>
  version: string
  enabled_surfaces: AppSurface[]
  dashboard_path: string
  direct_url: string
  granted_scopes: string[]
}

export type AppLaunchContext = {
  app_key: string
  surface: AppSurface
  start_url: string
  origin: string
}

export type EmbeddedReadyMessage = {
  type: 'seedance.auth.ready'
  version: 1
  transaction_id: string
  state: string
  nonce: string
  code_challenge: string
}

export type AppAuthorizeRequest = {
  surface: AppSurface
  transaction_id: string
  state: string
  nonce: string
  code_challenge: string
  code_challenge_method: 'S256'
}

export type AppAuthorizeResult = {
  launch_url: string
  surface: AppSurface
  expires_in: 60
}
