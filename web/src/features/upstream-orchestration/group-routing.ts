import type { UpstreamGroup, UpstreamRoute } from './types'

export function getUnroutedUpstreamGroups(
  groups: UpstreamGroup[],
  routes: UpstreamRoute[]
): UpstreamGroup[] {
  const routedGroups = new Set<string>()
  for (const route of routes) {
    if (route.detached || route.state !== 'active') continue
    routedGroups.add(
      `${route.source_id}\u0000${route.external_group_id.trim()}`
    )
  }
  return groups.filter(
    (group) =>
      !routedGroups.has(`${group.source_id}\u0000${group.external_id.trim()}`)
  )
}

export function getUpstreamGroupModels(group: UpstreamGroup): string[] {
  try {
    const parsed: unknown = JSON.parse(group.models)
    if (!Array.isArray(parsed)) return []
    return parsed.filter(
      (value): value is string =>
        typeof value === 'string' && value.trim().length > 0
    )
  } catch {
    return []
  }
}
