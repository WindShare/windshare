import { isPortableCatalogName, V2_CATALOG_NAME_BYTES } from '../../../catalog/path-policy'
import { encodeBase64Url } from '../../../crypto/bytes'
import { compatibleNameReadablePrefix } from '../../file-system-access/compatible-name/naming'
import {
  DIRECT_ZIP_BINDING_DIGEST_BYTES,
  DIRECT_ZIP_CANDIDATE_ID_BYTES,
  DIRECT_ZIP_OPERATION_ID_BYTES,
  DIRECT_ZIP_OWNERSHIP_NONCE_BYTES,
  snapshotDirectZipOwnershipMarkerV1,
  type DirectZipOwnershipMarkerV1,
} from '../format'
import {
  snapshotDirectZipFixedBytes,
  type DirectZipCanonicalBytes,
} from '../format/canonical'

export const DIRECT_ZIP_BROWSER_TARGET_ENABLED_BY_DEFAULT = false
export const DIRECT_ZIP_RESERVATION_MAXIMUM_CANDIDATES = 8
export const DIRECT_ZIP_STABLE_NAME_SEPARATOR = '.windshare-'
export const DIRECT_ZIP_STABLE_NAME_EXTENSION = '.zip'

const TEXT_ENCODER = new TextEncoder()
const CANDIDATE_TOKEN_CHARACTERS = 22
const STABLE_NAME_FIXED_UTF8_BYTES = TEXT_ENCODER.encode(
  `${DIRECT_ZIP_STABLE_NAME_SEPARATOR}${'a'.repeat(CANDIDATE_TOKEN_CHARACTERS)}` +
  DIRECT_ZIP_STABLE_NAME_EXTENSION,
).byteLength
const STABLE_NAME_PREFIX_MAX_UTF8_BYTES = V2_CATALOG_NAME_BYTES - STABLE_NAME_FIXED_UTF8_BYTES

export type DirectZipEvidenceComparison = 'same' | 'different' | 'unknown'
export type DirectZipPermissionState = PermissionState | 'unsupported'
export type DirectZipTargetStage =
  | 'permission-query'
  | 'permission-request'
  | 'candidate-persist'
  | 'exact-name-lookup'
  | 'exact-name-create'
  | 'bootstrap-write'
  | 'bootstrap-close'
  | 'snapshot'
  | 'epoch-open'
  | 'epoch-write'
  | 'epoch-truncate'
  | 'epoch-close'
  | 'epoch-abort'
  | 'range-proof'
  | 'cleanup-delete'
  | 'cleanup-observe'

export interface DirectZipParentBinding<ParentHandle> {
  readonly handleRef: string
  readonly bindingDigest: DirectZipCanonicalBytes
  readonly persistedHandle: ParentHandle
}

export interface DirectZipFileBinding<FileHandle> {
  readonly handleRef: string
  readonly bindingDigest: DirectZipCanonicalBytes
  readonly persistedHandle: FileHandle
}

export interface DirectZipReservationCandidate<ParentHandle> {
  readonly operationId: DirectZipCanonicalBytes
  readonly candidateId: DirectZipCanonicalBytes
  readonly resultRootComponent: string
  readonly stableName: string
  readonly ownershipNonce: DirectZipCanonicalBytes
  readonly targetRef: DirectZipCanonicalBytes
  readonly bindingDigest: DirectZipCanonicalBytes
  readonly marker: DirectZipOwnershipMarkerV1
  readonly parentBinding: DirectZipParentBinding<ParentHandle>
}

export interface DirectZipOwnedTargetBinding<ParentHandle, FileHandle>
  extends DirectZipReservationCandidate<ParentHandle> {
  readonly fileBinding: DirectZipFileBinding<FileHandle>
  readonly bootstrapPrefixLength: bigint
}

export interface DirectZipReservationCandidateDraft<ParentHandle> {
  readonly operationId: DirectZipCanonicalBytes
  readonly candidateId: DirectZipCanonicalBytes
  readonly resultRootComponent: string
  readonly stableName: string
  readonly ownershipNonce: DirectZipCanonicalBytes
  readonly parentBinding: DirectZipParentBinding<ParentHandle>
}

export type DirectZipReservationRetirementReason =
  | 'occupied-name'
  | 'bootstrap-marker-mismatch'
  | 'binding-refused'

export interface DirectZipTargetObservationV1 {
  readonly size: bigint
  readonly lastModified: number
  readonly marker:
    | Readonly<{ readonly kind: 'matching'; readonly prefixLength: bigint }>
    | Readonly<{ readonly kind: 'foreign' }>
    | Readonly<{ readonly kind: 'partial' }>
    | Readonly<{ readonly kind: 'malformed' }>
  readonly parentLocator: DirectZipEvidenceComparison
  readonly fileLocator: DirectZipEvidenceComparison
}

export interface DirectZipCommittedEpochProofV1 {
  readonly start: bigint
  readonly end: bigint
  readonly predecessorRoot: DirectZipCanonicalBytes
  readonly epochRoot: DirectZipCanonicalBytes
}

export interface DirectZipCheckpointTargetExpectation {
  readonly committedLength: bigint
  readonly observation: DirectZipTargetObservationV1
  readonly committedEpochs: readonly DirectZipCommittedEpochProofV1[]
}

export interface DirectZipCandidateTargetExpectation {
  readonly stagedEnd: bigint
  readonly observation: DirectZipTargetObservationV1
  readonly epoch: DirectZipCommittedEpochProofV1
}

export type DirectZipProofStatus = 'unchecked' | 'verified' | 'mismatch'

export type DirectZipRecoveryResolution =
  | Readonly<{ readonly kind: 'replay-predecessor' }>
  | Readonly<{ readonly kind: 'promote-candidate' }>
  | Readonly<{ readonly kind: 'truncate-to-predecessor' }>

export type DirectZipLifecycleDecision =
  | Readonly<{
    readonly kind: 'authorization-required'
    readonly stage: DirectZipTargetStage
    readonly reason: 'permission-prompt' | 'permission-denied' | 'permission-api-unavailable'
  }>
  | Readonly<{
    readonly kind: 'target-verification-required'
    readonly stage: DirectZipTargetStage
    readonly reason:
      | 'parent-binding-changed'
      | 'ownership-marker-incomplete'
      | 'ownership-marker-malformed'
      | 'observation-changed'
      | 'candidate-ambiguous'
      | 'unknown-tail'
      | 'native-effect-ambiguous'
    readonly proof: 'ownership-marker' | 'predecessor-epochs' | 'candidate-range' | 'fresh-observation'
  }>
  | Readonly<{
    readonly kind: 'destination-space-required'
    readonly stage: DirectZipTargetStage
  }>
  | Readonly<{
    readonly kind: 'restart-required'
    readonly stage: DirectZipTargetStage
    readonly reason: 'target-deleted'
  }>
  | Readonly<{
    readonly kind: 'needs-attention'
    readonly stage: DirectZipTargetStage
    readonly reason:
      | 'foreign-replacement'
      | 'committed-prefix-lost'
      | 'committed-prefix-mismatch'
      | 'ownership-unknown'
      | 'cleanup-refused'
      | 'reservation-exhausted'
  }>

export type DirectZipTargetOutcome<Value, Effect = never> =
  | Readonly<{ readonly kind: 'ready'; readonly value: Value }>
  | Readonly<{
    readonly kind: 'gated'
    readonly decision: DirectZipLifecycleDecision
    readonly retainedEffect?: Effect
  }>

export interface DirectZipTargetTraceEvent {
  readonly name:
    | 'direct_zip.target.lock'
    | 'direct_zip.target.permission'
    | 'direct_zip.target.candidate'
    | 'direct_zip.target.bootstrap'
    | 'direct_zip.target.observation'
    | 'direct_zip.target.recovery'
    | 'direct_zip.target.writable'
    | 'direct_zip.target.cleanup'
  readonly operation_id: string
  readonly stage: DirectZipTargetStage
  readonly outcome: string
  readonly candidate_id?: string
  readonly stable_name?: string
  readonly target_length?: string
  readonly decision?: DirectZipLifecycleDecision['kind'] | DirectZipRecoveryResolution['kind']
  readonly native_error_name?: string
}

export type DirectZipTargetTrace = (event: DirectZipTargetTraceEvent) => void

export function directZipStableTargetName(
  resultRootComponent: string,
  candidateId: Uint8Array,
): string {
  if (!isPortableCatalogName(resultRootComponent)) {
    throw new TypeError('direct ZIP result-root component violates the canonical path policy')
  }
  const identity = snapshotDirectZipFixedBytes(
    candidateId,
    DIRECT_ZIP_CANDIDATE_ID_BYTES,
    'direct ZIP reservation candidate ID',
  )
  const token = encodeBase64Url(identity)
  if (token.length !== CANDIDATE_TOKEN_CHARACTERS) {
    throw new Error('direct ZIP candidate identity did not encode to 22 base64url characters')
  }
  const compatibleRoot = compatibleNameReadablePrefix(resultRootComponent)
  const prefix = truncateUtf8(compatibleRoot, STABLE_NAME_PREFIX_MAX_UTF8_BYTES)
  const stableName = `${prefix}${DIRECT_ZIP_STABLE_NAME_SEPARATOR}${token}` +
    DIRECT_ZIP_STABLE_NAME_EXTENSION
  if (!isPortableCatalogName(stableName) || TEXT_ENCODER.encode(stableName).byteLength > V2_CATALOG_NAME_BYTES) {
    throw new TypeError('direct ZIP stable target name violates the compatible-name policy')
  }
  return stableName
}

export function snapshotDirectZipReservationCandidate<ParentHandle>(
  draft: DirectZipReservationCandidateDraft<ParentHandle>,
  persisted: Readonly<{
    readonly targetRef: Uint8Array
    readonly bindingDigest: Uint8Array
  }>,
): DirectZipReservationCandidate<ParentHandle> {
  const operationId = snapshotDirectZipFixedBytes(
    draft.operationId,
    DIRECT_ZIP_OPERATION_ID_BYTES,
    'direct ZIP operation ID',
  )
  const candidateId = snapshotDirectZipFixedBytes(
    draft.candidateId,
    DIRECT_ZIP_CANDIDATE_ID_BYTES,
    'direct ZIP reservation candidate ID',
  )
  const ownershipNonce = snapshotDirectZipFixedBytes(
    draft.ownershipNonce,
    DIRECT_ZIP_OWNERSHIP_NONCE_BYTES,
    'direct ZIP ownership nonce',
  )
  const targetRef = snapshotDirectZipFixedBytes(
    persisted.targetRef,
    DIRECT_ZIP_BINDING_DIGEST_BYTES,
    'direct ZIP target reference',
  )
  const bindingDigest = snapshotDirectZipFixedBytes(
    persisted.bindingDigest,
    DIRECT_ZIP_BINDING_DIGEST_BYTES,
    'direct ZIP binding digest',
  )
  const stableName = directZipStableTargetName(draft.resultRootComponent, candidateId)
  if (draft.stableName !== stableName) {
    throw new TypeError('persisted direct ZIP candidate changed its exact stable target name')
  }
  const marker = snapshotDirectZipOwnershipMarkerV1({
    operationId,
    candidateId,
    ownershipNonce,
    bindingDigest,
  })
  return Object.freeze({
    operationId,
    candidateId,
    resultRootComponent: draft.resultRootComponent,
    stableName,
    ownershipNonce,
    targetRef,
    bindingDigest,
    marker,
    parentBinding: snapshotParentBinding(draft.parentBinding),
  })
}

export function sameDirectZipTargetObservation(
  left: DirectZipTargetObservationV1,
  right: DirectZipTargetObservationV1,
): boolean {
  return left.size === right.size && left.lastModified === right.lastModified &&
    left.marker.kind === right.marker.kind &&
    (left.marker.kind !== 'matching' || right.marker.kind !== 'matching' ||
      left.marker.prefixLength === right.marker.prefixLength) &&
    left.parentLocator === right.parentLocator && left.fileLocator === right.fileLocator
}

export function directZipTraceIdentity(identity: Uint8Array): string {
  return encodeBase64Url(snapshotDirectZipFixedBytes(
    identity,
    DIRECT_ZIP_OPERATION_ID_BYTES,
    'direct ZIP operation ID',
  ))
}

export function directZipCandidateTraceIdentity(identity: Uint8Array): string {
  return encodeBase64Url(snapshotDirectZipFixedBytes(
    identity,
    DIRECT_ZIP_CANDIDATE_ID_BYTES,
    'direct ZIP candidate ID',
  ))
}

function snapshotParentBinding<ParentHandle>(
  binding: DirectZipParentBinding<ParentHandle>,
): DirectZipParentBinding<ParentHandle> {
  if (binding === null || typeof binding !== 'object' || binding.handleRef.length === 0 ||
      binding.persistedHandle === null || binding.persistedHandle === undefined) {
    throw new TypeError('direct ZIP parent binding evidence is invalid')
  }
  return Object.freeze({
    handleRef: binding.handleRef,
    bindingDigest: snapshotDirectZipFixedBytes(
      binding.bindingDigest,
      DIRECT_ZIP_BINDING_DIGEST_BYTES,
      'direct ZIP parent binding digest',
    ),
    persistedHandle: binding.persistedHandle,
  })
}

function truncateUtf8(value: string, maximumBytes: number): string {
  let result = ''
  let bytes = 0
  for (const scalar of value) {
    const scalarBytes = TEXT_ENCODER.encode(scalar).byteLength
    if (bytes + scalarBytes > maximumBytes) break
    result += scalar
    bytes += scalarBytes
  }
  if (result.length === 0) throw new TypeError('direct ZIP compatible target prefix is empty')
  return result
}
