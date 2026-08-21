import {
  catalogNameCollisionKey,
  snapshotPortableCatalogPath,
  isPortableCatalogName,
} from '../../../catalog/path-policy'
import { encodeBase64Url } from '../../../crypto/bytes'
import type { ReceiveLifecycleState } from '../../workspace/state'
import {
  RECEIVE_OPERATION_SCHEMA_VERSION,
  RECEIVE_RECORD_RECEIPT,
  operationRecordId,
  type PersistedReceiveRecord,
} from '../../workspace/records'
import {
  canonicalPath,
  canonicalText,
  snapshotCanonicalBytes,
  snapshotIdentity,
} from '../../workspace/canonical'
import {
  canonicalReceiveLifecycleStateBytes,
  decodeReceiveLifecycleState,
} from '../../workspace/state-codec'

export const COMPATIBLE_NAME_LEDGER_FORMAT_VERSION = 'compatible-name-ledger/v1' as const
export const COMPATIBLE_NAME_PENDING_OUTCOME_FORMAT_VERSION =
  'compatible-name-pending-outcome/v1' as const
export const MAX_COMPATIBLE_NAME_COMMITTED_MAPPINGS = 1_048_576
export const MAX_COMPATIBLE_NAME_REPAIR_SUMMARY_PATHS = 8

const BASE32_TOKEN_PATTERN = /^[a-z2-7]{6}$/u

export type CompatibleNameEntryKind = 'file' | 'directory'
export type CompatibleNamePairPlacement = 'inside-logical-root' | 'beside-mapped-root'
export type CompatibleNameActivationState = 'prepared' | 'pair-ready' | 'active'
export type CompatibleNameOwnershipState = 'selected' | 'owned'
export type CompatibleNameCommitState = 'uncommitted' | 'committed'
export type CompatibleNamePairKind = 'script' | 'sidecar'
export type CompatibleNameFooterState = 'active' | 'completed' | 'stopped' | 'failed'
export type CompatibleNameTerminalFooterState = Exclude<CompatibleNameFooterState, 'active'>
export type CompatibleNameOrdinaryTerminalLifecycle = Extract<
  ReceiveLifecycleState,
  { readonly kind:
    | 'published'
    | 'partial-directory'
    | 'restart-required'
    | 'discarded'
    | 'expired'
    | 'needs-attention' }
>

export interface CompatibleNamePairIdentityV1 {
  readonly physicalName: string
  readonly handleId: string
  readonly ownedObjectId: string
  readonly ownershipState: 'claimed' | 'owned'
}

export interface CompatibleNamePendingTerminalOutcomeV1 {
  readonly formatVersion: typeof COMPATIBLE_NAME_PENDING_OUTCOME_FORMAT_VERSION
  readonly footerState: CompatibleNameTerminalFooterState
  readonly ordinaryLifecycle: CompatibleNameOrdinaryTerminalLifecycle
  /** Immutable local receipt needed to finish lifecycle publication without sender work. */
  readonly terminalReceipt: PersistedReceiveRecord
}

export interface CompatibleNameFooterObservationV1 {
  readonly committedCount: number
  readonly state: CompatibleNameFooterState
}

export interface CompatibleNameRepairSummary {
  readonly committedCount: number
  readonly logicalPathSample: readonly (readonly string[])[]
  readonly pairDisplayNames: Readonly<{
    script: string
    sidecar: string
  }>
  readonly placement: CompatibleNamePairPlacement
  readonly runCommand: string
  readonly latestObservedFooter?: CompatibleNameFooterObservationV1
  readonly pendingCatchUp: boolean
}

export interface CompatibleNameOperationHeaderV1 {
  readonly formatVersion: typeof COMPATIBLE_NAME_LEDGER_FORMAT_VERSION
  readonly operationId: string
  readonly primaryToken: string
  readonly authorityRef: string
  readonly root: Readonly<{
    logicalName: string
    physicalName: string
  }>
  readonly templateId: string
  readonly pairPlacement: CompatibleNamePairPlacement
  readonly pair: Readonly<{
    script: CompatibleNamePairIdentityV1
    sidecar: CompatibleNamePairIdentityV1
  }>
  readonly activationState: CompatibleNameActivationState
  readonly pendingTerminalOutcome?: CompatibleNamePendingTerminalOutcomeV1
  readonly repairSummary?: CompatibleNameRepairSummary
}

export interface CompatibleNameMappingV1 {
  readonly formatVersion: typeof COMPATIBLE_NAME_LEDGER_FORMAT_VERSION
  readonly id: string
  readonly operationId: string
  readonly logicalPath: readonly string[]
  readonly entryKind: CompatibleNameEntryKind
  readonly physicalComponent: string
  readonly attempt: number
  readonly token: string
  readonly ownershipState: CompatibleNameOwnershipState
  readonly ownedObjectId?: string
  readonly commitState: CompatibleNameCommitState
  readonly commitOrdinal?: number
}

export interface CompatibleNameOperationSnapshotV1 {
  readonly header: CompatibleNameOperationHeaderV1
  readonly mappings: readonly CompatibleNameMappingV1[]
}

export interface CompatibleNameOperationBootstrapV1 {
  readonly header: CompatibleNameOperationHeaderV1
  readonly initialMapping: CompatibleNameMappingV1
}

export type CompatibleNameOperationHeaderSpec = Omit<
  CompatibleNameOperationHeaderV1,
  'formatVersion'
>

export type CompatibleNameMappingSpec = Omit<
  CompatibleNameMappingV1,
  'formatVersion' | 'id'
>

export function compatibleNameOperationHeaderV1(
  input: CompatibleNameOperationHeaderSpec,
): CompatibleNameOperationHeaderV1 {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const authorityRef = snapshotIdentity(input.authorityRef, 32, 'authority reference')
  const primaryToken = compatibleToken(input.primaryToken, 'primary compatible-name token')
  const root = Object.freeze({
    logicalName: portableComponent(input.root.logicalName, 'logical result-entry name'),
    physicalName: portableComponent(input.root.physicalName, 'physical result-entry name'),
  })
  const pairPlacement = compatiblePairPlacement(input.pairPlacement)
  if (pairPlacement === 'inside-logical-root' && root.logicalName !== root.physicalName) {
    throw new TypeError('inside-root restoration pair requires an unmapped result root')
  }
  const pair = Object.freeze({
    script: compatiblePairIdentity(input.pair.script, 'script'),
    sidecar: compatiblePairIdentity(input.pair.sidecar, 'sidecar'),
  })
  if (catalogNameCollisionKey(pair.script.physicalName) ===
        catalogNameCollisionKey(pair.sidecar.physicalName) ||
      pair.script.handleId === pair.sidecar.handleId ||
      pair.script.ownedObjectId === pair.sidecar.ownedObjectId) {
    throw new TypeError('restoration pair identities must be distinct')
  }
  const activationState = compatibleActivationState(input.activationState)
  const pairOwned = pair.script.ownershipState === 'owned' &&
    pair.sidecar.ownershipState === 'owned'
  if ((activationState === 'prepared' && pairOwned) ||
      (activationState !== 'prepared' && !pairOwned)) {
    throw new TypeError('compatible-name activation state disagrees with pair ownership')
  }
  const pendingTerminalOutcome = input.pendingTerminalOutcome === undefined
    ? undefined
    : compatibleNamePendingTerminalOutcomeV1(input.pendingTerminalOutcome)
  if (pendingTerminalOutcome !== undefined &&
      pendingTerminalOutcome.ordinaryLifecycle.operationId !== operationId) {
    throw new TypeError('pending terminal outcome escaped its compatible-name operation')
  }
  const repairSummary = input.repairSummary === undefined
    ? undefined
    : compatibleNameRepairSummary(input.repairSummary)
  if (repairSummary !== undefined) assertSummaryMatchesPair(repairSummary, pairPlacement, pair)
  if ((activationState === 'active') !== (repairSummary !== undefined)) {
    throw new TypeError('durable compatible-name activation and repair summary disagree')
  }
  return Object.freeze({
    formatVersion: COMPATIBLE_NAME_LEDGER_FORMAT_VERSION,
    operationId,
    primaryToken,
    authorityRef,
    root,
    templateId: nonEmptyCanonicalText(input.templateId, 'restoration template ID'),
    pairPlacement,
    pair,
    activationState,
    ...(pendingTerminalOutcome === undefined ? {} : { pendingTerminalOutcome }),
    ...(repairSummary === undefined ? {} : { repairSummary }),
  })
}

export function compatibleNameMappingV1(
  input: CompatibleNameMappingSpec,
): CompatibleNameMappingV1 {
  const operationId = snapshotIdentity(input.operationId, 16, 'operation ID')
  const logicalPath = snapshotPortableCatalogPath(input.logicalPath)
  const entryKind = compatibleEntryKind(input.entryKind)
  const ownershipState = compatibleOwnershipState(input.ownershipState)
  const commitState = compatibleCommitState(input.commitState)
  const attempt = boundedNonNegativeInteger(input.attempt, 'compatible-name attempt')
  const token = compatibleToken(input.token, 'selected compatible-name token')
  const ownedObjectId = input.ownedObjectId === undefined
    ? undefined
    : snapshotIdentity(input.ownedObjectId, 32, 'owned object ID')
  const commitOrdinal = input.commitOrdinal === undefined
    ? undefined
    : boundedPositiveInteger(input.commitOrdinal, 'compatible-name commit ordinal')
  if ((ownershipState === 'owned') !== (ownedObjectId !== undefined)) {
    throw new TypeError('compatible-name ownership state and object correlation disagree')
  }
  if ((commitState === 'committed') !== (commitOrdinal !== undefined) ||
      (commitState === 'committed' && ownershipState !== 'owned')) {
    throw new TypeError('compatible-name commit state and ordinal disagree')
  }
  return Object.freeze({
    formatVersion: COMPATIBLE_NAME_LEDGER_FORMAT_VERSION,
    id: compatibleNameMappingId(operationId, logicalPath, entryKind),
    operationId,
    logicalPath,
    entryKind,
    physicalComponent: portableComponent(input.physicalComponent, 'physical component'),
    attempt,
    token,
    ownershipState,
    ...(ownedObjectId === undefined ? {} : { ownedObjectId }),
    commitState,
    ...(commitOrdinal === undefined ? {} : { commitOrdinal }),
  })
}

export function compatibleNameOperationBootstrapV1(
  input: CompatibleNameOperationBootstrapV1,
): CompatibleNameOperationBootstrapV1 {
  const { header: headerInput, initialMapping: mappingInput } = input
  const header = compatibleNameOperationHeaderV1(headerInput)
  const initialMapping = compatibleNameMappingV1(mappingInput)
  if (header.activationState !== 'prepared' ||
      header.pendingTerminalOutcome !== undefined || header.repairSummary !== undefined ||
      header.pair.script.ownershipState !== 'claimed' ||
      header.pair.sidecar.ownershipState !== 'claimed') {
    throw new TypeError('compatible-name bootstrap must contain pristine repair state')
  }
  if (initialMapping.operationId !== header.operationId ||
      initialMapping.ownershipState !== 'selected' ||
      initialMapping.commitState !== 'uncommitted') {
    throw new TypeError('compatible-name bootstrap mapping is not a pristine operation claim')
  }
  if (initialMapping.attempt === 0 && initialMapping.token !== header.primaryToken) {
    throw new TypeError('primary compatible-name selection does not use the operation token')
  }
  const initialParent = compatibleMappingPhysicalParent(header, initialMapping.logicalPath)
  const pairParent = compatiblePairPhysicalParent(header)
  if (samePath(initialParent, pairParent) && (
      catalogNameCollisionKey(initialMapping.physicalComponent) ===
        catalogNameCollisionKey(header.pair.script.physicalName) ||
      catalogNameCollisionKey(initialMapping.physicalComponent) ===
        catalogNameCollisionKey(header.pair.sidecar.physicalName))) {
    throw new TypeError('compatible target collides with its restoration pair claim')
  }
  return Object.freeze({ header, initialMapping })
}

export function compatibleNamePendingTerminalOutcomeV1(
  input: CompatibleNamePendingTerminalOutcomeV1,
): CompatibleNamePendingTerminalOutcomeV1 {
  if (input.formatVersion !== COMPATIBLE_NAME_PENDING_OUTCOME_FORMAT_VERSION) {
    throw new TypeError('compatible-name pending outcome version is invalid')
  }
  const footerState = compatibleTerminalFooterState(input.footerState)
  const ordinaryLifecycle = decodeReceiveLifecycleState(
    canonicalReceiveLifecycleStateBytes(input.ordinaryLifecycle),
  )
  if (!isOrdinaryTerminalLifecycle(ordinaryLifecycle)) {
    throw new TypeError('compatible-name pending outcome is not an ordinary terminal lifecycle')
  }
  const terminalReceipt = snapshotTerminalReceipt(input.terminalReceipt, ordinaryLifecycle)
  return Object.freeze({
    formatVersion: COMPATIBLE_NAME_PENDING_OUTCOME_FORMAT_VERSION,
    footerState,
    ordinaryLifecycle,
    terminalReceipt,
  })
}

function snapshotTerminalReceipt(
  input: PersistedReceiveRecord,
  lifecycle: CompatibleNameOrdinaryTerminalLifecycle,
): PersistedReceiveRecord {
  const digest = terminalLifecycleReceiptDigest(lifecycle)
  if (input.schemaVersion !== RECEIVE_OPERATION_SCHEMA_VERSION ||
      input.operationId !== lifecycle.operationId || input.kind !== RECEIVE_RECORD_RECEIPT ||
      input.digest !== digest || input.id !== operationRecordId(
        lifecycle.operationId,
        RECEIVE_RECORD_RECEIPT,
        digest,
      )) {
    throw new TypeError('pending terminal receipt disagrees with ordinary lifecycle authority')
  }
  return Object.freeze({
    id: input.id,
    schemaVersion: input.schemaVersion,
    operationId: input.operationId,
    kind: input.kind,
    canonicalBytes: snapshotCanonicalBytes(input.canonicalBytes),
    digest,
  })
}

function terminalLifecycleReceiptDigest(
  lifecycle: CompatibleNameOrdinaryTerminalLifecycle,
): string {
  switch (lifecycle.kind) {
    case 'published':
    case 'partial-directory':
    case 'restart-required':
      return lifecycle.receiptDigest
    case 'discarded': return lifecycle.cleanupReceiptDigest
    case 'expired': return lifecycle.expiryReceiptDigest
    case 'needs-attention': return lifecycle.lastVerifiedRecordDigest
  }
}

function isOrdinaryTerminalLifecycle(
  lifecycle: ReceiveLifecycleState,
): lifecycle is CompatibleNameOrdinaryTerminalLifecycle {
  switch (lifecycle.kind) {
    case 'published':
    case 'partial-directory':
    case 'restart-required':
    case 'discarded':
    case 'expired':
    case 'needs-attention':
      return true
    default:
      return false
  }
}

export function compatibleNameRepairSummary(
  input: CompatibleNameRepairSummary,
): CompatibleNameRepairSummary {
  const committedCount = boundedNonNegativeInteger(
    input.committedCount,
    'compatible-name committed count',
  )
  if (input.logicalPathSample.length > MAX_COMPATIBLE_NAME_REPAIR_SUMMARY_PATHS ||
      input.logicalPathSample.length > committedCount) {
    throw new TypeError('compatible-name repair sample exceeds its bound')
  }
  const logicalPathSample = Object.freeze(input.logicalPathSample.map(snapshotPortableCatalogPath))
  const sampleKeys = logicalPathSample.map(path => encodeBase64Url(canonicalPath(path)))
  if (new Set(sampleKeys).size !== sampleKeys.length) {
    throw new TypeError('compatible-name repair sample repeats a logical path')
  }
  const latestObservedFooter = input.latestObservedFooter === undefined
    ? undefined
    : compatibleNameFooterObservationV1(input.latestObservedFooter)
  if (latestObservedFooter !== undefined && latestObservedFooter.committedCount > committedCount) {
    throw new TypeError('compatible-name footer exceeds the committed ledger prefix')
  }
  return Object.freeze({
    committedCount,
    logicalPathSample,
    pairDisplayNames: Object.freeze({
      script: portableComponent(input.pairDisplayNames.script, 'script display name'),
      sidecar: portableComponent(input.pairDisplayNames.sidecar, 'sidecar display name'),
    }),
    placement: compatiblePairPlacement(input.placement),
    runCommand: nonEmptyCanonicalText(input.runCommand, 'restoration run command'),
    ...(latestObservedFooter === undefined ? {} : { latestObservedFooter }),
    pendingCatchUp: booleanValue(input.pendingCatchUp, 'pending catch-up state'),
  })
}

export function compatibleNameFooterObservationV1(
  input: CompatibleNameFooterObservationV1,
): CompatibleNameFooterObservationV1 {
  return Object.freeze({
    committedCount: boundedNonNegativeInteger(input.committedCount, 'footer committed count'),
    state: compatibleFooterState(input.state),
  })
}

export function compatibleNameMappingId(
  operationIdInput: string,
  logicalPathInput: readonly string[],
  entryKindInput: CompatibleNameEntryKind,
): string {
  const operationId = snapshotIdentity(operationIdInput, 16, 'operation ID')
  const logicalPath = snapshotPortableCatalogPath(logicalPathInput)
  const entryKind = compatibleEntryKind(entryKindInput)
  return `${COMPATIBLE_NAME_LEDGER_FORMAT_VERSION}/${operationId}/${entryKind}/${
    encodeBase64Url(canonicalPath(logicalPath))}`
}

/**
 * Ledger paths are relative to the restoration anchor. Claim identities add the logical result root
 * only for inside placement so equal relative parents still identify distinct physical directories.
 */
export function compatibleMappingPhysicalParent(
  header: Pick<CompatibleNameOperationHeaderV1, 'pairPlacement' | 'root'>,
  logicalPathInput: readonly string[],
): readonly string[] {
  const logicalPath = snapshotPortableCatalogPath(logicalPathInput)
  const relativeParent = logicalPath.slice(0, -1)
  return header.pairPlacement === 'inside-logical-root'
    ? Object.freeze([header.root.logicalName, ...relativeParent])
    : Object.freeze(relativeParent)
}

export function compatiblePairPhysicalParent(
  header: Pick<CompatibleNameOperationHeaderV1, 'pairPlacement' | 'root'>,
): readonly string[] {
  return header.pairPlacement === 'inside-logical-root'
    ? Object.freeze([header.root.logicalName])
    : Object.freeze([])
}

function compatiblePairIdentity(
  input: CompatibleNamePairIdentityV1,
  label: CompatibleNamePairKind,
): CompatibleNamePairIdentityV1 {
  const ownershipState = input.ownershipState
  if (ownershipState !== 'claimed' && ownershipState !== 'owned') {
    throw new TypeError(`${label} pair ownership state is invalid`)
  }
  return Object.freeze({
    physicalName: portableComponent(input.physicalName, `${label} physical name`),
    handleId: nonEmptyCanonicalText(input.handleId, `${label} handle ID`),
    ownedObjectId: snapshotIdentity(input.ownedObjectId, 32, `${label} owned object ID`),
    ownershipState,
  })
}

function assertSummaryMatchesPair(
  summary: CompatibleNameRepairSummary,
  placement: CompatibleNamePairPlacement,
  pair: CompatibleNameOperationHeaderV1['pair'],
): void {
  if (summary.placement !== placement ||
      summary.pairDisplayNames.script !== pair.script.physicalName ||
      summary.pairDisplayNames.sidecar !== pair.sidecar.physicalName) {
    throw new TypeError('compatible-name repair summary disagrees with its restoration pair')
  }
}

function compatibleEntryKind(value: CompatibleNameEntryKind): CompatibleNameEntryKind {
  if (value !== 'file' && value !== 'directory') {
    throw new TypeError('compatible-name entry kind is invalid')
  }
  return value
}

function compatiblePairPlacement(value: CompatibleNamePairPlacement): CompatibleNamePairPlacement {
  if (value !== 'inside-logical-root' && value !== 'beside-mapped-root') {
    throw new TypeError('compatible-name pair placement is invalid')
  }
  return value
}

function compatibleActivationState(value: CompatibleNameActivationState): CompatibleNameActivationState {
  if (value !== 'prepared' && value !== 'pair-ready' && value !== 'active') {
    throw new TypeError('compatible-name activation state is invalid')
  }
  return value
}

function compatibleOwnershipState(value: CompatibleNameOwnershipState): CompatibleNameOwnershipState {
  if (value !== 'selected' && value !== 'owned') {
    throw new TypeError('compatible-name ownership state is invalid')
  }
  return value
}

function compatibleCommitState(value: CompatibleNameCommitState): CompatibleNameCommitState {
  if (value !== 'uncommitted' && value !== 'committed') {
    throw new TypeError('compatible-name commit state is invalid')
  }
  return value
}

function compatibleFooterState(value: CompatibleNameFooterState): CompatibleNameFooterState {
  if (value !== 'active' && value !== 'completed' && value !== 'stopped' && value !== 'failed') {
    throw new TypeError('compatible-name footer state is invalid')
  }
  return value
}

function compatibleTerminalFooterState(
  value: CompatibleNameTerminalFooterState,
): CompatibleNameTerminalFooterState {
  if (value !== 'completed' && value !== 'stopped' && value !== 'failed') {
    throw new TypeError('compatible-name terminal footer state is invalid')
  }
  return value
}

function compatibleToken(value: string, label: string): string {
  if (typeof value !== 'string' || !BASE32_TOKEN_PATTERN.test(value)) {
    throw new TypeError(`${label} must be six lowercase RFC 4648 Base32 characters`)
  }
  return value
}

function portableComponent(value: string, label: string): string {
  if (typeof value !== 'string' || !isPortableCatalogName(value)) {
    throw new TypeError(`${label} violates the canonical path-component policy`)
  }
  return value
}

function nonEmptyCanonicalText(value: string, label: string): string {
  const bytes = canonicalText(value)
  if (bytes.byteLength === 0) throw new TypeError(`${label} must not be empty`)
  return value
}

function boundedNonNegativeInteger(value: number, label: string): number {
  if (!Number.isSafeInteger(value) || value < 0 || value > MAX_COMPATIBLE_NAME_COMMITTED_MAPPINGS) {
    throw new TypeError(`${label} is invalid`)
  }
  return value
}

function boundedPositiveInteger(value: number, label: string): number {
  if (boundedNonNegativeInteger(value, label) === 0) throw new TypeError(`${label} is invalid`)
  return value
}

function booleanValue(value: boolean, label: string): boolean {
  if (typeof value !== 'boolean') throw new TypeError(`${label} is invalid`)
  return value
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}
