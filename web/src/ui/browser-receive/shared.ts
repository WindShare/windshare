import { encodeBase64Url } from '../../crypto/bytes'
import type {
  PortableEnvironmentOffer,
  WorkspaceEnvironmentOffer,
} from '../../output/planning'
import {
  DEFAULT_OPFS_JOB_WORKSPACE_LIMIT,
  DEFAULT_OPFS_PROCESS_WORKSPACE_LIMIT,
  MINIMUM_OPFS_QUOTA_RESERVE,
} from '../../output/workspace/budget'
import { reduceReceiveLifecycle, type LifecycleEvent } from '../../output/workspace/lifecycle'
import type { ReceiveOperationRepository } from '../../output/workspace/repository'
import { decodeStoredReceiveLifecycleState } from '../../output/workspace/state-codec'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
import type { AdmittedWorkspaceContent } from '../../output/workspace/stages'
import type { SealedWorkspaceZipPreparationV1 } from '../../output/workspace/preparation'
import type { WorkspaceMaterializationEvidence } from '../../transfer/settlement/persistent-execution'
import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
  DEFAULT_PORTABLE_MAXIMUM_PARTS,
  type ArtifactChoiceID,
  type ReceiveIntent,
} from '../../transfer/intent'
import type { ExactSingleFileEvidence } from '../../transfer/output-session'
import type { OriginPrivateStorageManager } from './contracts'

const WORKSPACE_ROUTE_ID = 'browser-origin-private-workspace'
const PORTABLE_ROUTE_ID = 'browser-portable-memory'
const AUTHORITY_REFERENCE_BYTES = 32
type LifecycleEventPayload = LifecycleEvent extends infer Event
  ? Event extends LifecycleEvent
    ? Omit<Event, 'expectedGeneration' | 'leaseId'>
    : never
  : never

export async function transitionLifecycle(
  repository: ReceiveOperationRepository,
  intent: ReceiveIntent,
  leaseId: string,
  payload: LifecycleEventPayload,
  expected?: ReceiveLifecycleState,
): Promise<ReceiveLifecycleState> {
  const current = await readLifecycle(repository, intent.operationId)
  if (expected !== undefined && current.generation !== expected.generation) {
    throw new DOMException('Receive lifecycle changed before the requested transition', 'InvalidStateError')
  }
  const reduction = reduceReceiveLifecycle(current, {
    ...payload,
    expectedGeneration: current.generation,
    leaseId,
  } as LifecycleEvent, {
    planKind: intent.plan.kind,
    preparationRequired: intent.plan.preparation !== 'none',
    activeLeaseId: leaseId,
    nowMilliseconds: Date.now(),
  })
  if (reduction.status !== 'applied') {
    throw new DOMException('Receive lifecycle transition was stale', 'InvalidStateError')
  }
  await repository.commitTransition({
    operationId: intent.operationId,
    expectedLifecycleGeneration: current.generation,
    expectedLeaseId: leaseId,
    lifecycle: reduction.state,
  })
  return reduction.state
}

export async function readLifecycle(
  repository: ReceiveOperationRepository,
  operationId: string,
): Promise<ReceiveLifecycleState> {
  const record = await repository.readLifecycle(operationId)
  if (record === undefined) throw new TypeError('Receive operation has no lifecycle authority')
  return decodeStoredReceiveLifecycleState(record)
}

export async function checkpointSetDigest(
  intent: ReceiveIntent,
  evidence: WorkspaceMaterializationEvidence,
): Promise<string> {
  const entries = evidence.entries
    .filter(entry => entry.kind === 'file')
    .map(entry => `${entry.fileId}:${entry.checkpoint.recordDigest}:${entry.exactSize.toString()}`)
    .sort()
  return digestText(`windshare/workspace-checkpoint-set/v1\n${intent.digest}\n${entries.join('\n')}`)
}

export async function operationDigest(intent: ReceiveIntent, purpose: string): Promise<string> {
  return digestText(`windshare/ui-operation-receipt/v1\n${intent.digest}\n${purpose}`)
}

export async function digestText(value: string): Promise<string> {
  const bytes = new TextEncoder().encode(value)
  return encodeBase64Url(new Uint8Array(await crypto.subtle.digest('SHA-256', bytes)))
}

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
    jobHardLimitBytes: DEFAULT_OPFS_JOB_WORKSPACE_LIMIT,
    processHardLimitBytes: DEFAULT_OPFS_PROCESS_WORKSPACE_LIMIT,
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

export function requireMatchingSingleFileAdmission(
  admitted: AdmittedWorkspaceContent,
  evidence: ExactSingleFileEvidence,
): void {
  const frozen = admitted.budget.evidence
  if (frozen.kind !== 'single-file' || frozen.fileId !== evidence.fileId ||
      frozen.containingDirectoryId !== evidence.containingDirectoryId ||
      frozen.generation !== evidence.generation ||
      frozen.catalogSize !== evidence.catalogSize) {
    throw new TypeError('Workspace continuation changed its admitted file evidence')
  }
}

export function requirePreparation(
  preparation: SealedWorkspaceZipPreparationV1 | undefined,
): SealedWorkspaceZipPreparationV1 {
  if (preparation === undefined) throw new TypeError('Workspace ZIP lost sealed preparation')
  return preparation
}

export function requireSameIntent(expected: ReceiveIntent, supplied: ReceiveIntent): void {
  if (expected.operationId !== supplied.operationId || expected.digest !== supplied.digest) {
    throw new TypeError('Receive authority escaped its frozen intent')
  }
}

export function snapshotPreClickRanking(
  selected: ArtifactChoiceID,
  ranking: readonly ArtifactChoiceID[],
): readonly ArtifactChoiceID[] {
  const snapshot = Object.freeze([...ranking])
  if (snapshot.length === 0 || !snapshot.includes(selected) ||
      new Set(snapshot).size !== snapshot.length) {
    throw new DOMException(
      'Artifact activation lost the exact pre-click choice ranking',
      'InvalidStateError',
    )
  }
  return snapshot
}

export function randomAuthorityReference(): string {
  const bytes = new Uint8Array(AUTHORITY_REFERENCE_BYTES)
  crypto.getRandomValues(bytes)
  return encodeBase64Url(bytes)
}

export function unavailableRoute(): DOMException {
  return new DOMException('No installed browser authority matches this action', 'NotSupportedError')
}

export function isWorkspaceTerminal(state: ReceiveLifecycleState): boolean {
  return state.kind === 'published' || state.kind === 'partial-directory' ||
    state.kind === 'restart-required' || state.kind === 'discarded' ||
    state.kind === 'expired' || state.kind === 'needs-attention' ||
    (state.kind === 'download-started' && state.attemptKind === 'portable')
}
