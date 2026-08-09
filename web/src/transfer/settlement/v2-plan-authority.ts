import { decodeBase64Url, encodeBase64Url } from '../../crypto/bytes'
import type { ReceiveLifecycleState } from '../../output/workspace/state'
import {
  snapshotCanonicalModifiedTime,
  snapshotMaterializationPath,
} from '../directory-admission'
import {
  validateReceiveIntent,
  type DirectAtomicPlan,
  type DirectTreePlan,
  type OriginalFileArtifact,
  type PortableHandoffPlan,
  type ReceiveIntent,
  type WorkspaceThenPublishPlan,
  type ZipArchiveArtifact,
} from '../intent'
import {
  validatePlanExecutionBinding,
  type DirectAtomicExecution,
  type DirectTreeExecution,
  type ExactPreparationEvidence,
  type ExactSingleFileEvidence,
  type ExecutionAdmissionResult,
  type PortableExecution,
  type V2PlanExecutionAuthority,
  type WorkspaceExecution,
} from '../output-session'

const CATALOG_IDENTITY_BYTES = 16
const MAXIMUM_EXACT_PREPARATION_ENTRIES = 1_000_000
const U64_MAXIMUM = 0xffff_ffff_ffff_ffffn

type DirectTreeIntent = ReceiveIntent & Readonly<{ plan: DirectTreePlan }>
type DirectAtomicIntent = ReceiveIntent & Readonly<{ plan: DirectAtomicPlan }>
type WorkspaceOriginalIntent = ReceiveIntent & Readonly<{
  plan: WorkspaceThenPublishPlan
  artifact: OriginalFileArtifact
}>
type WorkspaceZipIntent = ReceiveIntent & Readonly<{
  plan: WorkspaceThenPublishPlan
  artifact: ZipArchiveArtifact
}>
type PortableIntent = ReceiveIntent & Readonly<{ plan: PortableHandoffPlan }>
type PortableOriginalIntent = PortableIntent & Readonly<{ artifact: OriginalFileArtifact }>
type PortableZipIntent = PortableIntent & Readonly<{ artifact: ZipArchiveArtifact }>

export interface V2DirectTreeExecutionRoute {
  open(intent: DirectTreeIntent, signal: AbortSignal): Promise<DirectTreeExecution>
}

export interface V2DirectAtomicExecutionRoute {
  open(intent: DirectAtomicIntent, signal: AbortSignal): Promise<DirectAtomicExecution>
}

export interface V2WorkspaceOriginalExecutionRoute {
  admit(
    intent: WorkspaceOriginalIntent,
    evidence: ExactSingleFileEvidence,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>>
}

export interface V2WorkspaceZipExecutionRoute {
  prepare(
    intent: WorkspaceZipIntent,
    evidence: ExactPreparationEvidence,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<WorkspaceExecution>>
}

export interface V2PortableOriginalExecutionRoute {
  prepare(
    intent: PortableOriginalIntent,
    evidence: ExactPreparationEvidence,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<PortableExecution>>
}

export interface V2PortableZipExecutionRoute {
  prepare(
    intent: PortableZipIntent,
    evidence: ExactPreparationEvidence,
    signal: AbortSignal,
  ): Promise<ExecutionAdmissionResult<PortableExecution>>
}

export interface V2UnopenedExecutionLifecycle {
  abortUnopened(
    intent: ReceiveIntent,
    reason: unknown,
    signal: AbortSignal,
  ): Promise<ReceiveLifecycleState>
  recordSettlementUnknown(
    intent: ReceiveIntent,
    signal: AbortSignal,
  ): Promise<Extract<ReceiveLifecycleState, { readonly kind: 'needs-attention' }>>
}

/**
 * Every property is an already-acquired operation-bound route. An absent property
 * is an unavailable product offer, never permission to substitute another plan.
 */
export interface V2PlanExecutionRouteRegistry {
  readonly directTree?: V2DirectTreeExecutionRoute
  readonly directAtomic?: V2DirectAtomicExecutionRoute
  readonly workspaceOriginal?: V2WorkspaceOriginalExecutionRoute
  readonly workspaceZip?: V2WorkspaceZipExecutionRoute
  readonly portableOriginal?: V2PortableOriginalExecutionRoute
  readonly portableZip?: V2PortableZipExecutionRoute
  readonly lifecycle: V2UnopenedExecutionLifecycle
}

export class V2PlanRouteUnavailableError extends Error {
  readonly planKind: ReceiveIntent['plan']['kind']
  readonly artifactKind: ReceiveIntent['artifact']['kind']

  constructor(intent: ReceiveIntent) {
    super('No acquired production route matches the frozen receive intent')
    this.name = 'V2PlanRouteUnavailableError'
    this.planKind = intent.plan.kind
    this.artifactKind = intent.artifact.kind
  }
}

/**
 * Creates one authority for one immutable operation. Route callbacks are injected
 * by browser composition only after picker/repository authority has been acquired;
 * this layer verifies identity and dispatches without choosing a backend.
 */
export async function createV2PlanExecutionAuthority(input: {
  readonly intent: ReceiveIntent
  readonly routes: V2PlanExecutionRouteRegistry
}): Promise<V2PlanExecutionAuthority> {
  const boundIntent = await validateReceiveIntent(input.intent)
  const routes = snapshotRouteRegistry(input.routes)
  let claimedRoute: string | undefined

  const claim = async <Intent extends ReceiveIntent>(
    supplied: Intent,
    route: string,
    signal: AbortSignal,
  ): Promise<Intent> => {
    signal.throwIfAborted()
    const intent = await requireBoundIntent(boundIntent, supplied) as Intent
    if (claimedRoute !== undefined) {
      throw new TypeError('plan execution authority was already consumed')
    }
    claimedRoute = route
    return intent
  }

  const authority: V2PlanExecutionAuthority = {
    openDirectTree: async (supplied, signal) => {
      const intent = await claim(supplied, 'direct-tree', signal)
      const route = routes.directTree
      if (route === undefined) throw new V2PlanRouteUnavailableError(intent)
      const execution = validatePlanExecutionBinding(intent, await route.open(intent, signal))
      signal.throwIfAborted()
      return execution
    },
    openDirectAtomic: async (supplied, signal) => {
      const intent = await claim(supplied, 'direct-atomic', signal)
      const route = routes.directAtomic
      if (route === undefined) throw new V2PlanRouteUnavailableError(intent)
      const execution = validatePlanExecutionBinding(intent, await route.open(intent, signal))
      signal.throwIfAborted()
      return execution
    },
    openWorkspaceOriginal: async (supplied, evidence, signal) => {
      const intent = await claim(supplied, 'workspace-original', signal)
      const route = routes.workspaceOriginal
      if (route === undefined) throw new V2PlanRouteUnavailableError(intent)
      const snapshot = snapshotExactSingleFileEvidence(intent, evidence)
      const result = await route.admit(intent, snapshot, signal)
      signal.throwIfAborted()
      return validateExecutionAdmission(intent, result)
    },
    prepareWorkspaceZip: async (supplied, evidence, signal) => {
      const intent = await claim(supplied, 'workspace-zip', signal)
      const route = routes.workspaceZip
      if (route === undefined) throw new V2PlanRouteUnavailableError(intent)
      const result = await route.prepare(intent, snapshotExactPreparationEvidence(evidence), signal)
      signal.throwIfAborted()
      return validateExecutionAdmission(intent, result)
    },
    preparePortable: async (supplied, evidence, signal) => {
      const intent = await claim(
        supplied,
        `portable-${supplied.artifact.kind}`,
        signal,
      )
      const snapshot = snapshotExactPreparationEvidence(evidence)
      let result: ExecutionAdmissionResult<PortableExecution> | undefined
      switch (intent.artifact.kind) {
        case 'original-file': {
          const route = routes.portableOriginal
          if (route === undefined) throw new V2PlanRouteUnavailableError(intent)
          result = await route.prepare(intent as PortableOriginalIntent, snapshot, signal)
          break
        }
        case 'zip-archive': {
          const route = routes.portableZip
          if (route === undefined) throw new V2PlanRouteUnavailableError(intent)
          result = await route.prepare(intent as PortableZipIntent, snapshot, signal)
          break
        }
        case 'directory-tree': throw new V2PlanRouteUnavailableError(intent)
      }
      signal.throwIfAborted()
      if (result === undefined) throw new V2PlanRouteUnavailableError(intent)
      return validateExecutionAdmission(intent, result)
    },
    abortUnopened: async (supplied, reason, signal) => {
      signal.throwIfAborted()
      const intent = await requireBoundIntent(boundIntent, supplied)
      const state = await routes.lifecycle.abortUnopened(intent, reason, signal)
      signal.throwIfAborted()
      return validateLifecycleIdentity(intent, state)
    },
    recordSettlementUnknown: async (supplied, signal) => {
      signal.throwIfAborted()
      const intent = await requireBoundIntent(boundIntent, supplied)
      const state = await routes.lifecycle.recordSettlementUnknown(intent, signal)
      signal.throwIfAborted()
      validateLifecycleIdentity(intent, state)
      if (state.kind !== 'needs-attention') {
        throw new TypeError('unknown settlement must stop in NeedsAttention')
      }
      return state
    },
  }
  return Object.freeze(authority)
}

function snapshotRouteRegistry(input: V2PlanExecutionRouteRegistry): V2PlanExecutionRouteRegistry {
  if (typeof input !== 'object' || input === null || typeof input.lifecycle !== 'object' ||
      input.lifecycle === null || typeof input.lifecycle.abortUnopened !== 'function' ||
      typeof input.lifecycle.recordSettlementUnknown !== 'function') {
    throw new TypeError('plan route registry requires unopened and unknown lifecycle authority')
  }
  return Object.freeze({
    ...(input.directTree === undefined
      ? {}
      : { directTree: snapshotRoute(input.directTree, 'open') }),
    ...(input.directAtomic === undefined
      ? {}
      : { directAtomic: snapshotRoute(input.directAtomic, 'open') }),
    ...(input.workspaceOriginal === undefined
      ? {}
      : { workspaceOriginal: snapshotRoute(input.workspaceOriginal, 'admit') }),
    ...(input.workspaceZip === undefined
      ? {}
      : { workspaceZip: snapshotRoute(input.workspaceZip, 'prepare') }),
    ...(input.portableOriginal === undefined
      ? {}
      : { portableOriginal: snapshotRoute(input.portableOriginal, 'prepare') }),
    ...(input.portableZip === undefined
      ? {}
      : { portableZip: snapshotRoute(input.portableZip, 'prepare') }),
    lifecycle: Object.freeze({
      abortUnopened: input.lifecycle.abortUnopened.bind(input.lifecycle),
      recordSettlementUnknown: input.lifecycle.recordSettlementUnknown.bind(input.lifecycle),
    }),
  })
}

function snapshotRoute<Route extends object, Method extends keyof Route>(
  route: Route,
  method: Method,
): Readonly<Pick<Route, Method>> {
  if (typeof route !== 'object' || route === null) {
    throw new TypeError(`plan execution route requires ${String(method)}`)
  }
  const callback = route[method]
  if (typeof callback !== 'function') {
    throw new TypeError(`plan execution route requires ${String(method)}`)
  }
  return Object.freeze({ [method]: callback.bind(route) }) as Readonly<Pick<Route, Method>>
}

export function snapshotExactSingleFileEvidence(
  intent: WorkspaceOriginalIntent,
  input: ExactSingleFileEvidence,
): ExactSingleFileEvidence {
  const sourcePath = snapshotMaterializationPath(input.sourcePath)
  const catalogSize = requireU64(input.catalogSize, 'single-file catalog size')
  const evidence = Object.freeze({
    fileId: canonicalIdentity(input.fileId, CATALOG_IDENTITY_BYTES, 'file ID'),
    containingDirectoryId: canonicalIdentity(
      input.containingDirectoryId,
      CATALOG_IDENTITY_BYTES,
      'containing directory ID',
    ),
    generation: canonicalIdentity(input.generation, CATALOG_IDENTITY_BYTES, 'directory generation'),
    catalogSize,
    sourcePath,
    ...(input.modifiedTime === undefined
      ? {}
      : { modifiedTime: snapshotCanonicalModifiedTime(input.modifiedTime) }),
  })
  if (evidence.fileId !== intent.artifact.fileId ||
      evidence.sourcePath.join('/') !== intent.artifact.sourcePath) {
    throw new TypeError('single-file admission evidence does not bind the frozen artifact')
  }
  return evidence
}

export function snapshotExactPreparationEvidence(
  input: ExactPreparationEvidence,
): ExactPreparationEvidence {
  if (!Array.isArray(input.generations) || !Array.isArray(input.entries) ||
      input.generations.length > MAXIMUM_EXACT_PREPARATION_ENTRIES ||
      input.entries.length > MAXIMUM_EXACT_PREPARATION_ENTRIES) {
    throw new TypeError('exact preparation evidence exceeds its entry bound')
  }
  const generations = Object.freeze(input.generations.map((reference) => Object.freeze({
    directoryId: canonicalIdentity(reference.directoryId, CATALOG_IDENTITY_BYTES, 'directory ID'),
    generation: canonicalIdentity(reference.generation, CATALOG_IDENTITY_BYTES, 'directory generation'),
  })))
  const entries = Object.freeze(input.entries.map((entry) => {
    const paths = {
      sourcePath: snapshotMaterializationPath(entry.sourcePath),
      artifactPath: snapshotMaterializationPath(entry.artifactPath),
    }
    const modified = entry.modifiedTime === undefined
      ? {}
      : { modifiedTime: snapshotCanonicalModifiedTime(entry.modifiedTime) }
    if (entry.kind === 'directory') {
      if (paths.artifactPath.length === 0 ||
          (paths.sourcePath.length === 0 && entry.role !== 'result-root')) {
        throw new TypeError('exact preparation directory path is invalid')
      }
      return Object.freeze({
        kind: 'directory' as const,
        ...paths,
        ...modified,
        directoryId: canonicalIdentity(entry.directoryId, CATALOG_IDENTITY_BYTES, 'directory ID'),
        generation: canonicalIdentity(entry.generation, CATALOG_IDENTITY_BYTES, 'directory generation'),
        role: entry.role,
      })
    }
    if (paths.sourcePath.length === 0 || paths.artifactPath.length === 0) {
      throw new TypeError('exact preparation file path is invalid')
    }
    return Object.freeze({
      kind: 'file' as const,
      ...paths,
      ...modified,
      fileId: canonicalIdentity(entry.fileId, CATALOG_IDENTITY_BYTES, 'file ID'),
      containingDirectoryId: canonicalIdentity(
        entry.containingDirectoryId,
        CATALOG_IDENTITY_BYTES,
        'containing directory ID',
      ),
      generation: canonicalIdentity(entry.generation, CATALOG_IDENTITY_BYTES, 'directory generation'),
      exactSize: requireU64(entry.exactSize, 'preparation file size'),
    })
  }))
  const fileCount = BigInt(entries.filter(entry => entry.kind === 'file').length)
  const directoryCount = BigInt(entries.length) - fileCount
  const selectedRawBytes = entries.reduce(
    (total, entry) => entry.kind === 'file'
      ? checkedAdd(total, entry.exactSize)
      : total,
    0n,
  )
  if (requireU64(input.entryCount, 'preparation entry count') !== BigInt(entries.length) ||
      requireU64(input.fileCount, 'preparation file count') !== fileCount ||
      requireU64(input.directoryCount, 'preparation directory count') !== directoryCount ||
      requireU64(input.selectedRawBytes, 'preparation selected bytes') !== selectedRawBytes) {
    throw new TypeError('exact preparation evidence aggregate values are inconsistent')
  }
  return Object.freeze({
    generations,
    entries,
    entryCount: BigInt(entries.length),
    fileCount,
    directoryCount,
    selectedRawBytes,
  })
}

async function requireBoundIntent(
  bound: ReceiveIntent,
  supplied: ReceiveIntent,
): Promise<ReceiveIntent> {
  const intent = await validateReceiveIntent(supplied)
  if (intent.operationId !== bound.operationId || intent.digest !== bound.digest ||
      intent.artifact.digest !== bound.artifact.digest ||
      intent.plan.kind !== bound.plan.kind || planBindingDigest(intent) !== planBindingDigest(bound)) {
    throw new TypeError('plan execution authority belongs to another receive operation')
  }
  return intent
}

function planBindingDigest(intent: ReceiveIntent): string {
  switch (intent.plan.kind) {
    case 'direct-tree':
    case 'direct-atomic': return intent.plan.reservation.digest
    case 'workspace-then-publish': return intent.plan.workspace.digest
    case 'portable-handoff': return intent.plan.portable.digest
  }
}

function validateExecutionAdmission<Execution extends WorkspaceExecution | PortableExecution>(
  intent: ReceiveIntent,
  result: ExecutionAdmissionResult<Execution>,
): ExecutionAdmissionResult<Execution> {
  if (result.kind === 'accepted') {
    return Object.freeze({
      kind: 'accepted',
      execution: validatePlanExecutionBinding(intent, result.execution),
    })
  }
  if (result.kind !== 'rejected') throw new TypeError('execution admission result is invalid')
  const state = validateLifecycleIdentity(intent, result.state)
  if (state.kind !== 'discarded' && state.kind !== 'needs-attention') {
    throw new TypeError('rejected execution admission did not prove cleanup authority')
  }
  return Object.freeze({ kind: 'rejected', state })
}

function validateLifecycleIdentity(
  intent: ReceiveIntent,
  state: ReceiveLifecycleState,
): ReceiveLifecycleState {
  if (state.operationId !== intent.operationId || state.receiveIntentDigest !== intent.digest) {
    throw new TypeError('lifecycle state belongs to another receive operation')
  }
  return state
}

function canonicalIdentity(value: string, width: number, label: string): string {
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== width ||
      decoded.every(byte => byte === 0) || encodeBase64Url(decoded) !== value) {
    throw new TypeError(`${label} must be a canonical non-zero identity`)
  }
  return value
}

function requireU64(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > U64_MAXIMUM) {
    throw new TypeError(`${label} is outside u64`)
  }
  return value
}

function checkedAdd(left: bigint, right: bigint): bigint {
  const result = left + right
  if (result > U64_MAXIMUM) throw new RangeError('exact preparation byte total overflows u64')
  return result
}
