import type { UpstreamRoute } from './types'

export type UpstreamHAStatus = {
  authoritative: boolean
  nonHaModels: string[]
}

export function calculateUpstreamHA(routes: UpstreamRoute[]): UpstreamHAStatus {
  let authoritative = true
  const sourcesByModel = new Map<string, Set<number>>()
  for (const route of routes) {
    if (route.state !== 'active' || route.detached) continue
    if (route.effective_models == null) {
      authoritative = false
      continue
    }
    for (const model of route.effective_models) {
      const modelName = model.trim()
      if (!modelName) continue
      const key = `${modelName}:${route.protocol}`
      const sourceIDs = sourcesByModel.get(key) ?? new Set<number>()
      sourceIDs.add(route.source_id)
      sourcesByModel.set(key, sourceIDs)
    }
  }
  return {
    authoritative,
    nonHaModels: authoritative
      ? [...sourcesByModel.entries()]
          .filter(([, sourceIDs]) => sourceIDs.size < 2)
          .map(([key]) => key)
          .sort()
      : [],
  }
}
