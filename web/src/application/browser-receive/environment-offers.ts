import type { PortableEnvironmentOffer, WorkspaceEnvironmentOffer } from '../../output/planning'
import { MINIMUM_OPFS_QUOTA_RESERVE } from '../../output/workspace/budget'
import { BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS, DEFAULT_PORTABLE_ARTIFACT_LIMIT, DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES, DEFAULT_PORTABLE_MAXIMUM_PARTS } from '../../transfer/intent'
import type { OriginPrivateStorageManager } from '../../output/browser/environment'

const WORKSPACE_ROUTE_ID = 'browser-origin-private-workspace'

const PORTABLE_ROUTE_ID = 'browser-portable-memory'

export async function quotaAvailability(
  storage: Partial<Pick<OriginPrivateStorageManager, 'estimate'>>,
  signal: AbortSignal,
): Promise<bigint | null> {
  signal.throwIfAborted()
  const estimateQuota = storage.estimate
  if (typeof estimateQuota !== 'function') return null

  let estimate: unknown
  try {
    estimate = await estimateQuota.call(storage)
  } catch {
    signal.throwIfAborted()
    // Quota is observational; provider failure must not revoke an installed workspace route.
    return null
  }
  signal.throwIfAborted()
  if (estimate === null || typeof estimate !== 'object') return null
  const { quota, usage } = estimate as StorageEstimate
  if (typeof quota !== 'number' || typeof usage !== 'number' ||
      !Number.isSafeInteger(quota) || !Number.isSafeInteger(usage) || quota < 0 || usage < 0) {
    return null
  }
  return BigInt(Math.max(0, quota - usage))
}

export function workspaceEnvironmentOffer(
  quotaAvailabilityEstimateBytes: bigint | null,
): WorkspaceEnvironmentOffer {
  return Object.freeze({
    routeId: WORKSPACE_ROUTE_ID,
    kind: 'origin-private-workspace',
    persistence: 'durable-owned-repository',
    minimumQuotaReserveBytes: MINIMUM_OPFS_QUOTA_RESERVE,
    quotaAvailabilityEstimateBytes,
  })
}

export function portableEnvironmentOffer(): PortableEnvironmentOffer {
  return Object.freeze({
    routeId: PORTABLE_ROUTE_ID,
    kind: 'portable-memory',
    persistence: 'none',
    maximumArtifactBytes: DEFAULT_PORTABLE_ARTIFACT_LIMIT,
    assemblyPartBytes: DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
    maximumParts: DEFAULT_PORTABLE_MAXIMUM_PARTS,
    objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  })
}
