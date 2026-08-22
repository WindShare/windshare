import { decodeBase64Url, encodeBase64Url } from '../../../../crypto/bytes'
import type { ArtifactChoiceID } from '../../../../transfer/intent'
import { requireDirectZipFsaOffset } from '../../format'
import {
  canonicalFrame,
  canonicalText,
  canonicalU8,
  canonicalU64,
  concatCanonicalBytes,
  equalCanonicalBytes,
  snapshotCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from '../../../workspace/canonical'
import { directZipJournalBudgetDigestV1 } from '../budget'
import {
  DIRECT_ZIP_JOURNAL_SCHEMA_VERSION,
  DIRECT_ZIP_PAGE_CENTRAL,
  DIRECT_ZIP_PAGE_EPOCH,
  DIRECT_ZIP_PAGE_LAYOUT,
  type DirectZipCheckpointPhase,
  type DirectZipCheckpointV1,
  type DirectZipDiscoveryEvidenceV1,
  type DirectZipJournalBudgetUsageV1,
  type DirectZipPageAccountingPredecessorV1,
  type DirectZipPageChainV1,
  type DirectZipPageKind,
  type DirectZipPolicyDigestsV1,
} from '../model'

const ZERO_DIGEST = encodeBase64Url(new Uint8Array(32))
const DIRECT_ZIP_EPOCH_CANDIDATE_DOMAIN = 'windshare/direct-zip-epoch-candidate/v1'
const DIRECT_ZIP_CLOSING_CANDIDATE_DOMAIN = 'windshare/direct-zip-closing-candidate/v1'

export function directZipStateId(operationId: string): string {
  return `windshare/direct-zip-state/v1/${snapshotIdentity(operationId, 16, 'operation ID')}`
}

export function directZipCandidateId(operationId: string, candidateId: string): string {
  return `windshare/direct-zip-candidate/v1/${snapshotIdentity(operationId, 16, 'operation ID')}/${snapshotIdentity(candidateId, 16, 'candidate ID')}`
}

export function directZipPageId(
  operationId: string,
  pageKind: DirectZipPageKind,
  chainId: string,
  pageOrdinal: number,
): string {
  const ordinal = requirePageOrdinal(pageOrdinal).toString(10).padStart(7, '0')
  return `windshare/direct-zip-page/v1/${snapshotIdentity(operationId, 16, 'operation ID')}/${requirePageKind(pageKind)}/${snapshotIdentity(chainId, 16, 'page chain ID')}/${ordinal}`
}

export function directZipPageKindByte(kind: DirectZipPageKind): number {
  switch (kind) {
    case 'layout': return DIRECT_ZIP_PAGE_LAYOUT
    case 'central': return DIRECT_ZIP_PAGE_CENTRAL
    case 'epoch': return DIRECT_ZIP_PAGE_EPOCH
  }
}

export function directZipPageDomain(kind: DirectZipPageKind): string {
  switch (kind) {
    case 'layout': return 'windshare/direct-zip-layout-page/v1'
    case 'central': return 'windshare/direct-zip-central-page/v1'
    case 'epoch': return 'windshare/direct-zip-epoch-page/v1'
  }
}

export function directZipCandidateDomain(kind: 'epoch' | 'closing'): string {
  return kind === 'epoch' ? DIRECT_ZIP_EPOCH_CANDIDATE_DOMAIN : DIRECT_ZIP_CLOSING_CANDIDATE_DOMAIN
}

export function snapshotDiscoveryEvidence(input: DirectZipDiscoveryEvidenceV1): DirectZipDiscoveryEvidenceV1 {
  return Object.freeze({
    cursorCanonicalBytes: snapshotCanonicalBytes(input.cursorCanonicalBytes),
    directoryAdmissionDigest: snapshotDigest(
      input.directoryAdmissionDigest,
      'directory admission digest',
    ),
    discoveryRootDigest: snapshotDigest(input.discoveryRootDigest, 'discovery root digest'),
  })
}

export function canonicalDiscoveryEvidence(input: DirectZipDiscoveryEvidenceV1): CanonicalBytes {
  return concatCanonicalBytes([
    canonicalFrame(input.cursorCanonicalBytes),
    digestFrame(input.directoryAdmissionDigest, 'directory admission digest'),
    digestFrame(input.discoveryRootDigest, 'discovery root digest'),
  ])
}

export function snapshotPageChain(input: DirectZipPageChainV1, label: string): DirectZipPageChainV1 {
  const pageCount = requireU64(input.pageCount, `${label} page count`)
  const recordCount = requireU64(input.recordCount, `${label} record count`)
  const canonicalMetadataBytes = requireU64(
    input.canonicalMetadataBytes,
    `${label} canonical metadata bytes`,
  )
  const rootDigest = snapshotFixedBase64(input.rootDigest, 32, `${label} page root`, true)
  if ((pageCount === 0n) !== (rootDigest === ZERO_DIGEST) ||
      (pageCount === 0n && (recordCount !== 0n || canonicalMetadataBytes !== 0n))) {
    throw new TypeError(`${label} page chain has inconsistent empty authority`)
  }
  return Object.freeze({
    chainId: snapshotIdentity(input.chainId, 16, `${label} page chain ID`),
    rootDigest,
    pageCount,
    recordCount,
    canonicalMetadataBytes,
  })
}

export function canonicalPageChain(input: DirectZipPageChainV1): CanonicalBytes {
  return concatCanonicalBytes([
    identityFrame(input.chainId, 16, 'page chain ID'),
    fixedFrame(input.rootDigest, 32, 'page root digest', true),
    canonicalFrame(canonicalU64(input.pageCount)),
    canonicalFrame(canonicalU64(input.recordCount)),
    canonicalFrame(canonicalU64(input.canonicalMetadataBytes)),
  ])
}

export function snapshotClosingReplay(
  input: DirectZipCheckpointV1['closingReplay'],
  phase: DirectZipCheckpointPhase,
): DirectZipCheckpointV1['closingReplay'] {
  if ((phase === 'closing') !== (input !== undefined)) {
    throw new TypeError('closing checkpoint authority must include exactly one replay root')
  }
  if (input === undefined) return undefined
  requireDirectZipFsaOffset(input.archiveOffset, 'closing replay archive offset')
  return Object.freeze({
    archiveOffset: input.archiveOffset,
    centralRecordRootDigest: snapshotFixedBase64(
      input.centralRecordRootDigest,
      32,
      'closing central record root',
      true,
    ),
    ...(input.completion === undefined ? {} : {
      completion: Object.freeze({
        exactArchiveBytes: snapshotFsaOffset(
          input.completion.exactArchiveBytes,
          'completed archive length',
        ),
        preClosingEpochRootDigest: snapshotFixedBase64(
          input.completion.preClosingEpochRootDigest,
          32,
          'pre-closing epoch root',
          true,
        ),
      }),
    }),
  })
}

export function canonicalClosingReplay(input: DirectZipCheckpointV1['closingReplay']): CanonicalBytes {
  if (input === undefined) return canonicalU8(1)
  return concatCanonicalBytes([
    canonicalU8(2),
    canonicalFrame(canonicalU64(input.archiveOffset)),
    fixedFrame(input.centralRecordRootDigest, 32, 'closing central record root', true),
    canonicalFrame(input.completion === undefined
      ? canonicalU8(1)
      : concatCanonicalBytes([
          canonicalU8(2),
          canonicalFrame(canonicalU64(input.completion.exactArchiveBytes)),
          fixedFrame(input.completion.preClosingEpochRootDigest, 32, 'pre-closing epoch root', true),
        ])),
  ])
}

export async function snapshotPolicies(input: DirectZipPolicyDigestsV1): Promise<DirectZipPolicyDigestsV1> {
  const policies = Object.freeze({
    encodingPolicyDigest: snapshotDigest(input.encodingPolicyDigest, 'ZIP encoding policy digest'),
    layoutPolicyDigest: snapshotDigest(input.layoutPolicyDigest, 'ZIP layout policy digest'),
    checkpointPolicyDigest: snapshotDigest(
      input.checkpointPolicyDigest,
      'Direct ZIP checkpoint policy digest',
    ),
    journalBudgetDigest: snapshotDigest(
      input.journalBudgetDigest,
      'Direct ZIP journal budget digest',
    ),
    epochPolicyDigest: snapshotDigest(input.epochPolicyDigest, 'Direct ZIP epoch policy digest'),
  })
  if (policies.journalBudgetDigest !== await directZipJournalBudgetDigestV1()) {
    throw new TypeError('Direct ZIP journal budget digest is not the frozen V1 policy')
  }
  return policies
}

export function policyFrames(input: DirectZipPolicyDigestsV1): readonly CanonicalBytes[] {
  return [
    digestFrame(input.encodingPolicyDigest, 'ZIP encoding policy digest'),
    digestFrame(input.layoutPolicyDigest, 'ZIP layout policy digest'),
    digestFrame(input.checkpointPolicyDigest, 'Direct ZIP checkpoint policy digest'),
    digestFrame(input.journalBudgetDigest, 'Direct ZIP journal budget digest'),
    digestFrame(input.epochPolicyDigest, 'Direct ZIP epoch policy digest'),
  ]
}

export function snapshotAccountingPredecessor(
  input: DirectZipPageAccountingPredecessorV1,
): DirectZipPageAccountingPredecessorV1 {
  if (input.kind === 'checkpoint') {
    return Object.freeze({
      kind: 'checkpoint',
      checkpointGeneration: requirePositiveU64(
        input.checkpointGeneration,
        'accounting checkpoint generation',
      ),
      checkpointDigest: snapshotDigest(input.checkpointDigest, 'accounting checkpoint digest'),
    })
  }
  if (input.kind !== 'page') throw new TypeError('page accounting predecessor kind is invalid')
  return Object.freeze({
    kind: 'page',
    pageKind: requirePageKind(input.pageKind),
    pageId: snapshotCanonicalText(input.pageId, 'accounting predecessor page ID'),
    pageDigest: snapshotDigest(input.pageDigest, 'accounting predecessor page digest'),
  })
}

export function canonicalAccountingPredecessor(
  input: DirectZipPageAccountingPredecessorV1,
): CanonicalBytes {
  if (input.kind === 'checkpoint') {
    return concatCanonicalBytes([
      canonicalU8(1),
      canonicalFrame(canonicalU64(input.checkpointGeneration)),
      digestFrame(input.checkpointDigest, 'accounting checkpoint digest'),
    ])
  }
  return concatCanonicalBytes([
    canonicalU8(2),
    canonicalFrame(canonicalU8(directZipPageKindByte(input.pageKind))),
    canonicalFrame(canonicalText(input.pageId)),
    digestFrame(input.pageDigest, 'accounting predecessor page digest'),
  ])
}

export function snapshotPageEntries(input: readonly Uint8Array[]): readonly CanonicalBytes[] {
  if (!Array.isArray(input) || input.length === 0) {
    throw new TypeError('Direct ZIP immutable page must contain at least one record')
  }
  return Object.freeze(input.map(snapshotCanonicalBytes))
}

export function snapshotRanking(
  input: readonly ArtifactChoiceID[],
  selectedChoiceId: ArtifactChoiceID,
): readonly ArtifactChoiceID[] {
  if (!Array.isArray(input) || input.length === 0) {
    throw new TypeError('pre-click ranking is empty')
  }
  const ranking = input.map((choiceId) =>
    snapshotIdentity(choiceId, 32, 'ranked artifact choice ID') as ArtifactChoiceID)
  if (new Set(ranking).size !== ranking.length || !ranking.includes(selectedChoiceId)) {
    throw new TypeError('pre-click ranking must contain unique choices and the selected choice')
  }
  return Object.freeze(ranking)
}

export function canonicalRanking(input: readonly ArtifactChoiceID[]): CanonicalBytes {
  return concatCanonicalBytes([
    canonicalU64(BigInt(input.length)),
    ...input.map((choiceId) => identityFrame(choiceId, 32, 'ranked artifact choice ID')),
  ])
}

export function canonicalBudgetUsage(input: DirectZipJournalBudgetUsageV1): CanonicalBytes {
  return concatCanonicalBytes([
    canonicalFrame(canonicalU64(input.memberCount)),
    canonicalFrame(canonicalU64(input.canonicalMetadataBytes)),
  ])
}

export function checkpointPhaseByte(phase: DirectZipCheckpointPhase): number {
  switch (phase) {
    case 'between-members': return 1
    case 'inside-member': return 2
    case 'closing': return 3
  }
}

export function requireCheckpointPhase(input: DirectZipCheckpointPhase): DirectZipCheckpointPhase {
  if (input === 'between-members' || input === 'inside-member' || input === 'closing') return input
  throw new TypeError('Direct ZIP checkpoint phase is invalid')
}

export function requirePageKind(input: DirectZipPageKind): DirectZipPageKind {
  if (input === 'layout' || input === 'central' || input === 'epoch') return input
  throw new TypeError('Direct ZIP page kind is invalid')
}

export function requirePageOrdinal(input: number): number {
  if (!Number.isSafeInteger(input) || input < 0 || input >= 1_000_000) {
    throw new TypeError('Direct ZIP page ordinal is invalid')
  }
  return input
}

export function requireU64(input: bigint, label: string): bigint {
  try {
    canonicalU64(input)
  } catch {
    throw new TypeError(`${label} must be a canonical u64`)
  }
  return input
}

export function requirePositiveU64(input: bigint, label: string): bigint {
  requireU64(input, label)
  if (input === 0n) throw new TypeError(`${label} must not be zero`)
  return input
}

export function requireUnixMilliseconds(input: number, label: string): number {
  if (!Number.isSafeInteger(input) || input < 0) throw new TypeError(`${label} is invalid`)
  return input
}

export function snapshotFsaOffset(input: bigint, label: string): bigint {
  requireDirectZipFsaOffset(input, label)
  return input
}

export function optionalDigest(input: string | undefined, label: string): string | undefined {
  return input === undefined ? undefined : snapshotDigest(input, label)
}

export function optionalCanonicalText(input: string | undefined, label: string): string | undefined {
  return input === undefined ? undefined : snapshotCanonicalText(input, label)
}

export function optionalDigestFrame(input: string | undefined, label: string): CanonicalBytes {
  return canonicalFrame(input === undefined
    ? canonicalU8(1)
    : concatCanonicalBytes([canonicalU8(2), digestFrame(input, label)]))
}

export function optionalTextFrame(input: string | undefined): CanonicalBytes {
  return canonicalFrame(input === undefined
    ? canonicalU8(1)
    : concatCanonicalBytes([canonicalU8(2), canonicalFrame(canonicalText(input))]))
}

export function identityFrame(input: string, width: number, label: string): CanonicalBytes {
  return canonicalFrame(canonicalFixedBase64(input, width, label, false))
}

export function digestFrame(input: string, label: string): CanonicalBytes {
  return identityFrame(input, 32, label)
}

export function fixedFrame(
  input: string,
  width: number,
  label: string,
  allowZero: boolean,
): CanonicalBytes {
  return canonicalFrame(canonicalFixedBase64(input, width, label, allowZero))
}

export function snapshotDigest(input: string, label: string): string {
  return snapshotIdentity(input, 32, label)
}

export function snapshotFixedBase64(
  input: string,
  width: number,
  label: string,
  allowZero: boolean,
): string {
  return encodeBase64Url(canonicalFixedBase64(input, width, label, allowZero))
}

export function canonicalFixedBase64(
  input: string,
  width: number,
  label: string,
  allowZero: boolean,
): CanonicalBytes {
  const bytes = typeof input === 'string' ? decodeBase64Url(input) : undefined
  if (bytes === undefined || bytes.byteLength !== width || encodeBase64Url(bytes) !== input ||
      (!allowZero && bytes.every((byte) => byte === 0))) {
    throw new TypeError(`${label} must be a canonical ${allowZero ? '' : 'non-zero '}${width}-byte value`)
  }
  return Uint8Array.from(bytes) as CanonicalBytes
}

export function snapshotCanonicalText(input: string, label: string): string {
  const bytes = canonicalText(input)
  if (bytes.byteLength === 0) throw new TypeError(`${label} must not be empty`)
  return input
}

export function checkedBudgetSubtraction(left: bigint, right: bigint, label: string): bigint {
  if (typeof left !== 'bigint' || typeof right !== 'bigint' || right < 0n || left < right) {
    throw new TypeError(`${label} is inconsistent`)
  }
  return left - right
}

export function checkedU64Addition(left: bigint, right: bigint, label: string): bigint {
  const maximum = 0xffff_ffff_ffff_ffffn
  if (left > maximum - right) throw new TypeError(`${label} overflowed`)
  return left + right
}

export function assertCanonicalProjection(
  input: Readonly<{ canonicalBytes: Uint8Array; digest: string }>,
  rebuilt: Readonly<{ canonicalBytes: Uint8Array; digest: string }>,
  label: string,
): void {
  if (input.digest !== rebuilt.digest ||
      !equalCanonicalBytes(input.canonicalBytes, rebuilt.canonicalBytes)) {
    throw new TypeError(`${label} canonical authority changed`)
  }
}

export function requireJournalVersion(input: number): void {
  if (input !== DIRECT_ZIP_JOURNAL_SCHEMA_VERSION) {
    throw new TypeError('Direct ZIP journal schema version is invalid')
  }
}

export function invalidCandidateKind(): never {
  throw new TypeError('Direct ZIP candidate kind is invalid')
}
