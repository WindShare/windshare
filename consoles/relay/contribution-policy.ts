import type { ContributionPolicy } from './api'

export const GIB = 1024 ** 3
export const BYTES_PER_MEGABIT = 125_000

// Floor to whole display units so valid inputs scale into JS safe integers (and
// fit the relay's i64/u64 bounds) without awkward repeating decimals in browser
// range messages.
export const MAX_DAILY_GIB = Math.floor(Number.MAX_SAFE_INTEGER / GIB)
export const MAX_RATE_MBPS = Math.floor(Number.MAX_SAFE_INTEGER / BYTES_PER_MEGABIT)
export const MAX_CONNECTIONS = Number.MAX_SAFE_INTEGER

export interface ContributionDraft {
  enabled: boolean
  daily_bytes: string
  bytes_per_second: string
  connections: string
  host_uplink: string
}

export function draftFromPolicy(policy: ContributionPolicy): ContributionDraft {
  return {
    enabled: policy.enabled,
    daily_bytes: String(policy.daily_bytes / GIB),
    bytes_per_second: String(policy.bytes_per_second / BYTES_PER_MEGABIT),
    connections: String(policy.connections),
    host_uplink: policy.host_uplink === null ? '' : String(policy.host_uplink / BYTES_PER_MEGABIT),
  }
}

function parseScaledLimit(raw: string, unit: number, min: number, maxDisplay: number): number | null {
  const trimmed = raw.trim()
  if (!trimmed) return null
  const value = Number(trimmed)
  if (!Number.isFinite(value) || value < 0 || value > maxDisplay) return null
  if (unit === 1 && !Number.isInteger(value)) return null
  const scaled = Math.round(value * unit)
  if (!Number.isSafeInteger(scaled) || scaled < min) return null
  return scaled === 0 ? 0 : scaled
}

export function parseContributionDraft(draft: ContributionDraft): ContributionPolicy | null {
  const daily_bytes = parseScaledLimit(draft.daily_bytes, GIB, 0, MAX_DAILY_GIB)
  const bytes_per_second = parseScaledLimit(draft.bytes_per_second, BYTES_PER_MEGABIT, 1, MAX_RATE_MBPS)
  const connections = parseScaledLimit(draft.connections, 1, 1, MAX_CONNECTIONS)
  const hostUplinkText = draft.host_uplink.trim()
  const host_uplink = hostUplinkText
    ? parseScaledLimit(hostUplinkText, BYTES_PER_MEGABIT, 1, MAX_RATE_MBPS)
    : null
  if (
    daily_bytes === null
    || bytes_per_second === null
    || connections === null
    || (hostUplinkText !== '' && host_uplink === null)
  ) {
    return null
  }
  return { enabled: draft.enabled, daily_bytes, bytes_per_second, connections, host_uplink }
}
