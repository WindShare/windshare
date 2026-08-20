import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createOriginalFileArtifact,
  createReceiveIntent,
  createSelectionSpec,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  type OriginalFileArtifact,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import { IndexedDbReceiveOperationRepository } from '../../src/output/browser/indexeddb-repository'
import {
  OriginPrivateWorkspaceBudgetAuthority,
  type OriginPrivateWorkspaceBudgetClaim,
} from '../../src/output/origin-private/admission'
import { decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'

const CLAIM_LEASE_MILLISECONDS = 1_000
const CLAIM_HEARTBEAT_MILLISECONDS = 500

export const DURABLE_FIXTURE_CAPACITY_BYTES = 1_000_000_000n
export const DURABLE_FIXTURE_INITIAL_TIME = 1_000

export type DurableIntent = ReceiveIntent & { readonly artifact: OriginalFileArtifact }

export interface DurableIdentities {
  readonly operationId: string
  readonly workspaceId: string
  readonly repositoryRef: string
  readonly shareInstance: string
  readonly syntheticRoot: string
  readonly fileId: string
  readonly fileRevision: string
  readonly directoryId: string
  readonly generation: string
  readonly rootOwnedObjectId: string
  readonly transferJobId: string
  readonly expiryReceiptDigest: string
  readonly firstPublicationAttemptId: string
  readonly secondPublicationAttemptId: string
}

export async function durableIdentities(key: string): Promise<DurableIdentities> {
  if (!/^[A-Za-z0-9-]{1,80}$/u.test(key)) throw new TypeError('durable fixture key is invalid')
  return Object.freeze({
    operationId: await durableFixtureIdentity(key, 'operation', 16),
    workspaceId: await durableFixtureIdentity(key, 'workspace', 16),
    repositoryRef: await durableFixtureIdentity(key, 'repository', 32),
    shareInstance: await durableFixtureIdentity(key, 'share', 16),
    syntheticRoot: await durableFixtureIdentity(key, 'selection-root', 16),
    fileId: await durableFixtureIdentity(key, 'file', 16),
    fileRevision: await durableFixtureIdentity(key, 'revision', 16),
    directoryId: await durableFixtureIdentity(key, 'directory', 16),
    generation: await durableFixtureIdentity(key, 'generation', 16),
    rootOwnedObjectId: await durableFixtureIdentity(key, 'workspace-root-object', 32),
    transferJobId: await durableFixtureIdentity(key, 'transfer-job', 16),
    expiryReceiptDigest: await durableFixtureIdentity(key, 'expiry-receipt', 32),
    firstPublicationAttemptId: await durableFixtureIdentity(key, 'publication-one', 16),
    secondPublicationAttemptId: await durableFixtureIdentity(key, 'publication-two', 16),
  })
}

export async function durableFixtureIdentity(
  key: string,
  label: string,
  width: 16 | 32,
): Promise<string> {
  const digest = new Uint8Array(await crypto.subtle.digest(
    'SHA-256',
    new TextEncoder().encode(`windshare/w3c-test/${key}/${label}`),
  ))
  return encodeBase64Url(digest.slice(0, width))
}

export async function durableIntent(ids: DurableIdentities): Promise<DurableIntent> {
  const artifact = await createOriginalFileArtifact({
    fileId: ids.fileId,
    sourcePath: 'root/browser-file.bin',
    suggestedName: 'browser-file.bin',
  })
  const workspace = await createWorkspaceBinding({
    operationId: ids.operationId,
    workspaceId: ids.workspaceId,
    artifact,
    repositoryRef: ids.repositoryRef,
  })
  const intent = await createReceiveIntent({
    selection: await createSelectionSpec({
      shareInstance: ids.shareInstance,
      syntheticRoot: ids.syntheticRoot,
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    artifact,
    plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
  if (intent.artifact.kind !== 'original-file') throw new TypeError('durable fixture artifact changed')
  return intent as DurableIntent
}

export async function readDurableLifecycle(
  repository: IndexedDbReceiveOperationRepository,
  operationId: string,
) {
  const record = await repository.readLifecycle(operationId)
  if (record === undefined) throw new Error('durable operation lacks lifecycle state')
  return decodeStoredReceiveLifecycleState(record)
}

export async function workspaceBudgetAuthority(input: {
  readonly operationId: string
  readonly databaseName: string
  readonly now: number
  readonly token: string
}): Promise<OriginPrivateWorkspaceBudgetAuthority> {
  return OriginPrivateWorkspaceBudgetAuthority.open(input.operationId, {
    estimate: async () => ({ usage: 0, quota: Number(DURABLE_FIXTURE_CAPACITY_BYTES) }),
    jobLimitBytes: DURABLE_FIXTURE_CAPACITY_BYTES,
    processLimitBytes: DURABLE_FIXTURE_CAPACITY_BYTES,
    minimumReserveBytes: 0n,
    databaseName: input.databaseName,
    now: () => input.now,
    leaseMilliseconds: CLAIM_LEASE_MILLISECONDS,
    heartbeatMilliseconds: CLAIM_HEARTBEAT_MILLISECONDS,
    randomToken: () => input.token,
  })
}

export function originPrivateClaim(claim: {
  readonly budgetDigest: string
  release(): Promise<void>
}): OriginPrivateWorkspaceBudgetClaim {
  if (!('readmit' in claim) || typeof claim.readmit !== 'function') {
    throw new TypeError('origin-private admission did not return a readmission claim')
  }
  return claim as OriginPrivateWorkspaceBudgetClaim
}
