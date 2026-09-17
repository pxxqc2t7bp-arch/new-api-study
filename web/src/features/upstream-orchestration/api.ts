import { api } from '@/lib/api'

import type {
  ApiResponse,
  UpstreamMetric,
  UpstreamOverview,
  UpstreamPriceEvidence,
} from './types'

export async function getUpstreamOverview() {
  const response = await api.get<ApiResponse<UpstreamOverview>>(
    '/api/upstream-orchestration/overview'
  )
  return response.data.data
}

export async function getUpstreamMetrics() {
  const response = await api.get<ApiResponse<UpstreamMetric[]>>(
    '/api/upstream-orchestration/metrics?limit=500'
  )
  return response.data.data ?? []
}

export async function getUpstreamPrices() {
  const response = await api.get<ApiResponse<UpstreamPriceEvidence[]>>(
    '/api/upstream-orchestration/prices?limit=200'
  )
  return response.data.data ?? []
}

export async function requestUpstreamSync() {
  const response = await api.post<ApiResponse<unknown>>(
    '/api/upstream-orchestration/sync-request'
  )
  return response.data
}

export async function reconcileUpstreams() {
  const response = await api.post<ApiResponse<unknown>>(
    '/api/upstream-orchestration/reconcile'
  )
  return response.data
}

export async function createPairingCode() {
  const response = await api.post<
    ApiResponse<{ device_id: string; pairing_code: string; expires_at: number }>
  >('/api/upstream-orchestration/devices/pairing-code', {
    device_name: 'Chrome',
  })
  return response.data.data
}

export async function updateUpstreamRoute(
  routeId: number,
  action: 'probe' | 'pause' | 'resume' | 'detach'
) {
  const response = await api.post<ApiResponse<unknown>>(
    `/api/upstream-orchestration/routes/${routeId}/${action}`,
    {}
  )
  return response.data
}
