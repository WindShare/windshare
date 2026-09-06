import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  createOriginalFileArtifact, createReceiveIntent, createSelectionSpec,
  createWorkspaceBinding, createWorkspaceThenPublishPlan,
} from '../../../src/transfer/intent'
import { createSingleFileWorkspaceBudget } from '../../../src/output/workspace/budget'
import {
  IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority,
  type WorkspaceBudgetCapacityFacts, type WorkspaceBudgetLeaseRecord,
} from '../../../src/output/origin-private/admission-authority'

const METADATA_BYTES = 32n
const RESERVE_BYTES = 100n
const DEFAULT_QUOTA = 1_000n
const sessions = new Map<string, Awaited<ReturnType<typeof createSession>>>()

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}

async function createSession(databaseName: string, actor: number, token: string, now: number) {
  const artifact = await createOriginalFileArtifact({
    fileId: identity(16, 3), sourcePath: 'root/file.bin', suggestedName: 'file.bin',
  })
  const workspace = await createWorkspaceBinding({
    operationId: identity(16, 10 + actor), workspaceId: identity(16, 5),
    artifact, repositoryRef: identity(32, 6),
  })
  const intent = await createReceiveIntent({
    selection: await createSelectionSpec({
      shareInstance: identity(16, 1), syntheticRoot: identity(16, 2),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    artifact, plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
  const budget = await createSingleFileWorkspaceBudget({
    receiveIntent: intent, fileId: identity(16, 3), containingDirectoryId: identity(16, 8),
    generation: identity(16, 9), catalogSize: 10_000n, durableMetadataBytes: METADATA_BYTES,
  })
  const record: WorkspaceBudgetLeaseRecord = {
    id: intent.operationId, operationId: intent.operationId, token,
    budgetDigest: budget.digest, peakOwnedBytes: budget.peakOwnedBytes,
    expiresAtMilliseconds: now + 1_000,
  }
  return {
    authority: await IndexedDbOriginPrivateWorkspaceBudgetLeaseAuthority.open(databaseName),
    budget, record, objectId: identity(32, 20 + actor),
  }
}

export interface CapacityAction {
  readonly token: string
  readonly kind: 'claim' | 'reclaim' | 'reserve' | 'settle' | 'release-reservation' |
    'release' | 'reconcile' | 'heartbeat' | 'forget'
  readonly now?: number
  readonly quota?: string
  readonly usage?: string
  readonly verified?: string
  readonly current?: string
  readonly target?: string
  readonly headroom?: string
  readonly reservation?: string
}

export async function openCapacitySession(databaseName: string, actor: number, token: string, now = 0) {
  sessions.set(token, await createSession(databaseName, actor, token, now))
}

// Each page owns its own real IDB connection; only quota observations and lease time are deterministic.
export async function capacityAction(input: CapacityAction): Promise<string> {
  const session = sessions.get(input.token)
  if (session === undefined) throw new Error('Capacity session is not open')
  const { authority, record, budget, objectId } = session
  const nowMilliseconds = input.now ?? 0
  const fence = { operationId: record.operationId, token: record.token, nowMilliseconds }
  const facts: WorkspaceBudgetCapacityFacts = {
    estimatedQuotaBytes: BigInt(input.quota ?? DEFAULT_QUOTA),
    currentUsageBytes: BigInt(input.usage ?? 0),
    minimumReserveBytes: RESERVE_BYTES,
    verifiedAlreadyOwnedBytes: BigInt(input.verified ?? 0),
    nowMilliseconds,
  }
  try {
    switch (input.kind) {
      case 'claim': case 'reclaim':
        return (await authority[input.kind](record, budget, facts)).kind
      case 'reserve':
        await authority.reserveGrowth(fence, {
          operationId: record.operationId, objectId,
          currentLength: BigInt(input.current ?? 0), targetLength: BigInt(input.target ?? 0),
          metadataHeadroom: BigInt(input.headroom ?? 0),
        }, input.reservation ?? 'growth', facts)
        break
      case 'settle':
        await authority.settleGrowth(fence, objectId, input.reservation ?? 'growth', BigInt(input.current ?? 0))
        break
      case 'release-reservation':
        await authority.settleGrowth(fence, objectId, input.reservation ?? 'growth')
        break
      case 'release': await authority.release(record.id, record.token); break
      case 'reconcile': await authority.reconcileObject(fence, objectId, BigInt(input.current ?? 0)); break
      case 'heartbeat':
        await authority.heartbeat({ id: record.id, token: record.token,
          nowMilliseconds, expiresAtMilliseconds: nowMilliseconds + 1_000 })
        break
      case 'forget': await authority.forgetDeletedOperation(record.operationId); break
    }
    return 'accepted'
  } catch (error) {
    return error instanceof Error ? error.name : String(error)
  }
}

export function closeCapacitySessions(): void {
  for (const session of sessions.values()) session.authority.close()
  sessions.clear()
}

export async function deleteCapacityDatabase(databaseName: string): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(databaseName)
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error)
    request.onblocked = () => reject(new Error('Capacity fixture connection still open'))
  })
}
