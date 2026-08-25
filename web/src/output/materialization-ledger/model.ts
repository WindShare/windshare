import {
  snapshotCanonicalModifiedTime,
  type CanonicalModifiedTime,
} from '../../transfer/directory-admission'
import { isFault, type Fault } from '../../transfer/fault'
import {
  snapshotMaterializationRootRelativePath,
  type MaterializationRootRelativePath,
} from '../../transfer/job/coordinate/direct-tree'
import {
  VerifiedFinalOutputFile,
  type OutputFileOwnership,
  type OutputSourceIdentity,
} from '../../transfer/output-session'
import {
  FILE_CHECKPOINT_ID_BYTES,
  FILE_ID_BYTES,
  FILE_REVISION_BYTES,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import { CanonicalRecordReader } from '../workspace/canonical-reader'
import {
  canonicalFrame,
  canonicalIdentity,
  canonicalText,
  canonicalU8,
  canonicalU64,
  concatCanonicalBytes,
  equalCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from '../workspace/canonical'
const TEXT_DECODER = new TextDecoder('utf-8', { fatal: true })

const DIRECTORY_FINALIZED_OUTCOME_DISCRIMINANT = 1
const DIRECTORY_ISOLATED_OUTCOME_DISCRIMINANT = 2
const ABSENT_DISCRIMINANT = 0
const PRESENT_DISCRIMINANT = 1
export const MATERIALIZATION_LEDGER_SCHEMA_VERSION = 1 as const
export const MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT = 128 as const
export const MATERIALIZATION_LEDGER_MAX_OBJECTS = 1_048_576
export const MATERIALIZATION_LEDGER_MAX_ENTRY_EVENTS = MATERIALIZATION_LEDGER_MAX_OBJECTS * 2
export const MATERIALIZATION_LEDGER_U64_MAX = 0xffff_ffff_ffff_ffffn

export const MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER = 1 as const
export const MATERIALIZATION_LEDGER_DIRECTORY_FINALIZATION_ORDER = 2 as const

export const MaterializationLedgerEntryKind = {
  FileFinalized: 'file-finalized',
  DirectoryAdmitted: 'directory-admitted',
  DirectoryFinalized: 'directory-finalized',
} as const

export const MaterializationLedgerDirectoryOutcome = {
  Finalized: 'finalized',
  IsolatedFailure: 'isolated-failure',
} as const

export const MaterializationLedgerSealPurpose = {
  ResumableSnapshot: 'resumable-snapshot',
  Terminal: 'terminal',
} as const

export type MaterializationLedgerEntryKind =
  typeof MaterializationLedgerEntryKind[keyof typeof MaterializationLedgerEntryKind]
export type MaterializationLedgerDirectoryOutcome =
  typeof MaterializationLedgerDirectoryOutcome[keyof typeof MaterializationLedgerDirectoryOutcome]
export type MaterializationLedgerSealPurpose =
  typeof MaterializationLedgerSealPurpose[keyof typeof MaterializationLedgerSealPurpose]
export type MaterializationLedgerAppendClassification = 'insert' | 'idempotent'
export type MaterializationLedgerEntryOrder =
  | typeof MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER
  | typeof MATERIALIZATION_LEDGER_DIRECTORY_FINALIZATION_ORDER

export interface MaterializationLedgerBindingInput {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly materializationBindingDigest: string
  readonly authorityRef: string
}

export interface MaterializationLedgerBindingV1 extends MaterializationLedgerBindingInput {
  readonly schemaVersion: typeof MATERIALIZATION_LEDGER_SCHEMA_VERSION
  readonly materializerKind: 'fsa-tree'
  readonly pathCoordinate: 'fsa-reserved-root-relative'
  readonly layoutVersion: 2
  readonly ledgerBindingDigest: string
  readonly canonicalBytes: CanonicalBytes
}

export interface FinalFileCheckpointReference {
  readonly recordId: string
  readonly recordDigest: string
  readonly checkpointGeneration: bigint
}

export interface MaterializationFinalFileProofV1 {
  readonly schemaVersion: typeof MATERIALIZATION_LEDGER_SCHEMA_VERSION
  readonly kind: 'final-file-proof'
  readonly operationId: string
  readonly ledgerBindingDigest: string
  readonly proofId: string
  readonly proofDigest: string
  readonly recordId: string
  readonly fileId: string
  readonly finalOutput: VerifiedFinalOutputFile
  readonly checkpoint: FinalFileCheckpointReference
  readonly canonicalBytes: CanonicalBytes
}

interface MaterializationLedgerEntryBase {
  readonly schemaVersion: typeof MATERIALIZATION_LEDGER_SCHEMA_VERSION
  readonly operationId: string
  readonly ledgerBindingDigest: string
  readonly entryId: string
  readonly entryDigest: string
  readonly pathKey: string
  readonly relativePath: MaterializationRootRelativePath
  readonly entryOrder: MaterializationLedgerEntryOrder
  readonly canonicalBytes: CanonicalBytes
}

export interface MaterializationFileFinalizedEntryV1 extends MaterializationLedgerEntryBase {
  readonly kind: typeof MaterializationLedgerEntryKind.FileFinalized
  readonly entryOrder: typeof MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER
  readonly shareInstance: string
  readonly fileId: string
  readonly fileRevision: string
  readonly exactSize: bigint
  readonly outputBackend: string
  readonly outputSessionId: string
  readonly ownedFileIdentity: string
  readonly checkpoint: FinalFileCheckpointReference
  readonly finalProofId: string
  readonly finalProofDigest: string
}

export interface StableParentDirectoryCoordinates {
  readonly relativePath: MaterializationRootRelativePath
  readonly directoryId: string
  readonly generation: string
  readonly ownedObjectId: string
}

export interface StableDirectoryCoordinates extends StableParentDirectoryCoordinates {
  readonly parent?: StableParentDirectoryCoordinates
  readonly modifiedTime?: CanonicalModifiedTime
}

export interface MaterializationDirectoryAdmittedEntryV1 extends
  MaterializationLedgerEntryBase,
  StableDirectoryCoordinates {
  readonly kind: typeof MaterializationLedgerEntryKind.DirectoryAdmitted
  readonly entryOrder: typeof MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER
}

export type MaterializationDirectoryFinalization =
  | Readonly<{
      kind: typeof MaterializationLedgerDirectoryOutcome.Finalized
    }>
  | Readonly<{
      kind: typeof MaterializationLedgerDirectoryOutcome.IsolatedFailure
      fault: Fault
    }>

export interface MaterializationDirectoryFinalizedEntryV1 extends
  MaterializationLedgerEntryBase,
  StableDirectoryCoordinates {
  readonly kind: typeof MaterializationLedgerEntryKind.DirectoryFinalized
  readonly entryOrder: typeof MATERIALIZATION_LEDGER_DIRECTORY_FINALIZATION_ORDER
  readonly admissionEntryId: string
  readonly admissionEntryDigest: string
  readonly outcome: MaterializationDirectoryFinalization
}

export type MaterializationLedgerEntryV1 =
  | MaterializationFileFinalizedEntryV1
  | MaterializationDirectoryAdmittedEntryV1
  | MaterializationDirectoryFinalizedEntryV1

export interface MaterializationLedgerEntryCursor {
  readonly pathKey: string
  readonly entryOrder: MaterializationLedgerEntryOrder
  readonly entryId: string
}

export interface MaterializationLedgerEntryPage {
  readonly entries: readonly MaterializationLedgerEntryV1[]
  readonly continuation?: MaterializationLedgerEntryCursor
}

export interface MaterializationLedgerPageRequest {
  readonly after?: MaterializationLedgerEntryCursor
  readonly limit: typeof MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT
}

export type MaterializationLedgerRootFinalization =
  | Readonly<{ kind: 'missing' }>
  | Readonly<{
      kind: typeof MaterializationLedgerDirectoryOutcome.Finalized
      entryId: string
      entryDigest: string
    }>
  | Readonly<{
      kind: typeof MaterializationLedgerDirectoryOutcome.IsolatedFailure
      entryId: string
      entryDigest: string
    }>

export type MaterializationLedgerRootFact =
  | Readonly<{ kind: 'absent' }>
  | Readonly<{
      kind: typeof MaterializationLedgerEntryKind.FileFinalized
      relativePath: MaterializationRootRelativePath
      pathKey: string
      entryId: string
      fileId: string
      fileRevision: string
    }>
  | Readonly<{
      kind: 'directory'
      relativePath: MaterializationRootRelativePath
      pathKey: string
      admissionEntryId: string
      admissionEntryDigest: string
      directoryId: string
      generation: string
      ownedObjectId: string
      finalization: MaterializationLedgerRootFinalization
    }>

export interface MaterializationLedgerCounts {
  readonly entryEventCount: bigint
  readonly fileCount: bigint
  readonly fileBytes: bigint
  readonly materializedDirectoryCount: bigint
  readonly visibleDirectoryCount: bigint
  readonly finalizedDirectoryCount: bigint
  readonly isolatedDirectoryCount: bigint
}

export interface MaterializationLedgerPageSummaryV1 extends MaterializationLedgerCounts {
  readonly schemaVersion: typeof MATERIALIZATION_LEDGER_SCHEMA_VERSION
  readonly operationId: string
  readonly ledgerBindingDigest: string
  readonly sealId: string
  readonly pageId: string
  readonly pageDigest: string
  readonly pageOrdinal: bigint
  readonly firstEntry: MaterializationLedgerEntryCursor
  readonly lastEntry: MaterializationLedgerEntryCursor
  readonly canonicalEntryBytes: bigint
  readonly orderedEntriesDigest: string
  readonly rootPathFact: MaterializationLedgerRootFact
  readonly canonicalBytes: CanonicalBytes
}

export interface MaterializationLedgerSealV1 extends MaterializationLedgerCounts {
  readonly schemaVersion: typeof MATERIALIZATION_LEDGER_SCHEMA_VERSION
  readonly operationId: string
  readonly ledgerBindingDigest: string
  readonly sealId: string
  readonly sealDigest: string
  readonly sealSequence: bigint
  readonly purpose: MaterializationLedgerSealPurpose
  readonly pageCount: bigint
  readonly candidateCheckpointCount: bigint
  readonly lastEntry?: MaterializationLedgerEntryCursor
  readonly entryPageRoot: string
  readonly aggregateRoot: string
  readonly rootPathFact: MaterializationLedgerRootFact
  readonly canonicalBytes: CanonicalBytes
}

export interface FinalizedFileMaterializationRecords {
  readonly finalProof: MaterializationFinalFileProofV1
  readonly ledgerEntry: MaterializationFileFinalizedEntryV1
  readonly finalCheckpoint: FileCheckpointV2
}

export interface MaterializationLedgerValidatedPage {
  readonly entries: readonly MaterializationLedgerEntryV1[]
  readonly continuation?: MaterializationLedgerEntryCursor
  readonly directoryCarry?: MaterializationDirectoryAdmittedEntryV1
}

export function checkpointReference(checkpoint: FileCheckpointV2): FinalFileCheckpointReference {
  return Object.freeze({
    recordId: snapshotIdentity(checkpoint.recordId, FILE_CHECKPOINT_ID_BYTES, 'checkpoint record ID'),
    recordDigest: snapshotIdentity(
      checkpoint.checksum,
      FILE_CHECKPOINT_ID_BYTES,
      'checkpoint record digest',
    ),
    checkpointGeneration: u64(checkpoint.checkpointGeneration, 'checkpoint generation'),
  })
}

export function canonicalCheckpointReference(referenceInput: FinalFileCheckpointReference): CanonicalBytes {
  const reference = snapshotCheckpointReference(referenceInput)
  return concatCanonicalBytes([
    canonicalFrame(canonicalIdentity(
      reference.recordId,
      FILE_CHECKPOINT_ID_BYTES,
      'checkpoint record ID',
    )),
    canonicalFrame(canonicalIdentity(
      reference.recordDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'checkpoint record digest',
    )),
    canonicalFrame(canonicalU64(reference.checkpointGeneration)),
  ])
}

export function snapshotCheckpointReference(
  input: FinalFileCheckpointReference,
): FinalFileCheckpointReference {
  requireExactKeys(
    input,
    ['recordId', 'recordDigest', 'checkpointGeneration'],
    'final checkpoint reference',
  )
  return Object.freeze({
    recordId: snapshotIdentity(input.recordId, FILE_CHECKPOINT_ID_BYTES, 'checkpoint record ID'),
    recordDigest: snapshotIdentity(
      input.recordDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'checkpoint record digest',
    ),
    checkpointGeneration: u64(input.checkpointGeneration, 'checkpoint generation'),
  })
}

export function decodeCheckpointReference(bytes: Uint8Array): FinalFileCheckpointReference {
  const reader = CanonicalRecordReader.value(bytes)
  const result = Object.freeze({
    recordId: reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'checkpoint record ID'),
    recordDigest: reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'checkpoint record digest'),
    checkpointGeneration: reader.framedU64('checkpoint generation'),
  })
  reader.finish('final checkpoint reference')
  return result
}

export function canonicalFinalOutput(proof: VerifiedFinalOutputFile): CanonicalBytes {
  return concatCanonicalBytes([
    canonicalFrame(canonicalText(proof.ownership.backend)),
    canonicalFrame(canonicalText(proof.ownership.outputSessionId)),
    canonicalFrame(canonicalMaterializationPath(proof.ownership.canonicalPath)),
    canonicalFrame(canonicalText(proof.ownership.ownedFileIdentity)),
    canonicalFrame(canonicalIdentity(proof.source.shareInstance, FILE_ID_BYTES, 'share instance')),
    canonicalFrame(canonicalIdentity(proof.source.fileId, FILE_ID_BYTES, 'file ID')),
    canonicalFrame(canonicalIdentity(
      proof.source.fileRevision,
      FILE_REVISION_BYTES,
      'file revision',
    )),
    canonicalFrame(canonicalU64(proof.fileSize)),
  ])
}

export function decodeFinalOutput(bytes: Uint8Array): VerifiedFinalOutputFile {
  const reader = CanonicalRecordReader.value(bytes)
  const ownership: OutputFileOwnership = {
    backend: decodeText(reader.frame('output backend'), 'output backend'),
    outputSessionId: decodeText(reader.frame('output session ID'), 'output session ID'),
    canonicalPath: decodeMaterializationPath(reader.frame('materialization relative path')),
    ownedFileIdentity: decodeText(reader.frame('owned file identity'), 'owned file identity'),
  }
  const source: OutputSourceIdentity = {
    shareInstance: reader.framedIdentity(FILE_ID_BYTES, 'share instance'),
    fileId: reader.framedIdentity(FILE_ID_BYTES, 'file ID'),
    fileRevision: reader.framedIdentity(FILE_REVISION_BYTES, 'file revision'),
  }
  const fileSize = reader.framedU64('final file size')
  reader.finish('verified final output')
  return new VerifiedFinalOutputFile(ownership, source, fileSize)
}

export function snapshotStableDirectoryCoordinates(
  input: StableDirectoryCoordinates,
): StableDirectoryCoordinates {
  const inputRecord = requireRecord(input, 'stable directory coordinates')
  requireExactKeys(inputRecord, [
    'relativePath',
    'directoryId',
    'generation',
    'ownedObjectId',
    ...(hasOwn(inputRecord, 'parent') ? ['parent'] : []),
    ...(hasOwn(inputRecord, 'modifiedTime') ? ['modifiedTime'] : []),
  ], 'stable directory coordinates')
  const relativePath = snapshotMaterializationRootRelativePath(input.relativePath)
  const parent = input.parent === undefined ? undefined : snapshotParentCoordinates(input.parent)
  if ((relativePath.length === 0) !== (parent === undefined)) {
    throw new TypeError('only the materialization root may omit stable parent coordinates')
  }
  if (parent !== undefined && (
    parent.relativePath.length + 1 !== relativePath.length ||
    !parent.relativePath.every((segment, index) => segment === relativePath[index])
  )) {
    throw new TypeError('stable parent coordinates are not the immediate materialization parent')
  }
  const modifiedTime = input.modifiedTime === undefined
    ? undefined
    : snapshotCanonicalModifiedTime(input.modifiedTime)
  return Object.freeze({
    relativePath,
    directoryId: snapshotIdentity(input.directoryId, FILE_ID_BYTES, 'directory ID'),
    generation: snapshotIdentity(input.generation, FILE_ID_BYTES, 'directory generation'),
    ownedObjectId: snapshotIdentity(
      input.ownedObjectId,
      FILE_CHECKPOINT_ID_BYTES,
      'owned directory object ID',
    ),
    ...(parent === undefined ? {} : { parent }),
    ...(modifiedTime === undefined ? {} : { modifiedTime }),
  })
}

function snapshotParentCoordinates(
  input: StableParentDirectoryCoordinates,
): StableParentDirectoryCoordinates {
  requireExactKeys(
    input,
    ['relativePath', 'directoryId', 'generation', 'ownedObjectId'],
    'stable parent directory coordinates',
  )
  return Object.freeze({
    relativePath: snapshotMaterializationRootRelativePath(input.relativePath),
    directoryId: snapshotIdentity(input.directoryId, FILE_ID_BYTES, 'parent directory ID'),
    generation: snapshotIdentity(input.generation, FILE_ID_BYTES, 'parent directory generation'),
    ownedObjectId: snapshotIdentity(
      input.ownedObjectId,
      FILE_CHECKPOINT_ID_BYTES,
      'owned parent directory object ID',
    ),
  })
}

export function canonicalOptionalParent(parent: StableParentDirectoryCoordinates | undefined): CanonicalBytes {
  if (parent === undefined) return canonicalU8(ABSENT_DISCRIMINANT)
  const snapshot = snapshotParentCoordinates(parent)
  return concatCanonicalBytes([
    canonicalU8(PRESENT_DISCRIMINANT),
    canonicalFrame(canonicalMaterializationPath(snapshot.relativePath)),
    canonicalFrame(canonicalIdentity(snapshot.directoryId, FILE_ID_BYTES, 'parent directory ID')),
    canonicalFrame(canonicalIdentity(
      snapshot.generation,
      FILE_ID_BYTES,
      'parent directory generation',
    )),
    canonicalFrame(canonicalIdentity(
      snapshot.ownedObjectId,
      FILE_CHECKPOINT_ID_BYTES,
      'owned parent directory object ID',
    )),
  ])
}

export function decodeOptionalParent(bytes: Uint8Array): StableParentDirectoryCoordinates | undefined {
  const reader = CanonicalRecordReader.value(bytes)
  const discriminant = reader.byte('parent directory presence')
  if (discriminant === ABSENT_DISCRIMINANT) {
    reader.finish('absent parent directory coordinates')
    return undefined
  }
  requireDiscriminant(discriminant, PRESENT_DISCRIMINANT, 'parent directory presence')
  const result = snapshotParentCoordinates({
    relativePath: decodeMaterializationPath(reader.frame('parent relative path')),
    directoryId: reader.framedIdentity(FILE_ID_BYTES, 'parent directory ID'),
    generation: reader.framedIdentity(FILE_ID_BYTES, 'parent directory generation'),
    ownedObjectId: reader.framedIdentity(
      FILE_CHECKPOINT_ID_BYTES,
      'owned parent directory object ID',
    ),
  })
  reader.finish('parent directory coordinates')
  return result
}

export function canonicalOptionalModifiedTime(value: CanonicalModifiedTime | undefined): CanonicalBytes {
  if (value === undefined) return canonicalU8(ABSENT_DISCRIMINANT)
  const snapshot = snapshotCanonicalModifiedTime(value)
  return concatCanonicalBytes([
    canonicalU8(PRESENT_DISCRIMINANT),
    canonicalFrame(canonicalU64(snapshot.seconds)),
    canonicalFrame(canonicalU64(BigInt(snapshot.nanoseconds))),
    canonicalU8(snapshot.precision),
  ])
}

export function decodeOptionalModifiedTime(bytes: Uint8Array): CanonicalModifiedTime | undefined {
  const reader = CanonicalRecordReader.value(bytes)
  const discriminant = reader.byte('modified time presence')
  if (discriminant === ABSENT_DISCRIMINANT) {
    reader.finish('absent modified time')
    return undefined
  }
  requireDiscriminant(discriminant, PRESENT_DISCRIMINANT, 'modified time presence')
  const seconds = reader.framedU64('modified time seconds')
  const nanoseconds = reader.framedU64('modified time nanoseconds')
  if (nanoseconds > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new TypeError('modified time nanoseconds exceed the runtime bound')
  }
  const result = snapshotCanonicalModifiedTime({
    seconds,
    nanoseconds: Number(nanoseconds),
    precision: reader.byte('modified time precision') as 1 | 2 | 3,
  })
  reader.finish('modified time')
  return result
}

export function decodeStableDirectoryCoordinates(
  reader: CanonicalRecordReader,
  relativePath: MaterializationRootRelativePath,
): StableDirectoryCoordinates {
  const directoryId = reader.framedIdentity(FILE_ID_BYTES, 'directory ID')
  const generation = reader.framedIdentity(FILE_ID_BYTES, 'directory generation')
  const ownedObjectId = reader.framedIdentity(
    FILE_CHECKPOINT_ID_BYTES,
    'owned directory object ID',
  )
  const parent = decodeOptionalParent(reader.frame('parent directory coordinates'))
  const modifiedTime = decodeOptionalModifiedTime(reader.frame('directory modified time'))
  return snapshotStableDirectoryCoordinates({
    relativePath,
    directoryId,
    generation,
    ownedObjectId,
    ...(parent === undefined ? {} : { parent }),
    ...(modifiedTime === undefined ? {} : { modifiedTime }),
  })
}

export function snapshotDirectoryOutcome(
  input: MaterializationDirectoryFinalization,
): MaterializationDirectoryFinalization {
  if (input.kind === MaterializationLedgerDirectoryOutcome.Finalized) {
    requireExactKeys(input, ['kind'], 'finalized directory outcome')
    return Object.freeze({ kind: MaterializationLedgerDirectoryOutcome.Finalized })
  }
  requireExactKeys(input, ['kind', 'fault'], 'isolated directory outcome')
  if (input.kind !== MaterializationLedgerDirectoryOutcome.IsolatedFailure ||
      !isFault(input.fault)) {
    throw new TypeError('directory finalization outcome is invalid')
  }
  return Object.freeze({
    kind: MaterializationLedgerDirectoryOutcome.IsolatedFailure,
    fault: Object.freeze({ ...input.fault }),
  })
}

export function canonicalDirectoryOutcome(outcome: MaterializationDirectoryFinalization): CanonicalBytes {
  if (outcome.kind === MaterializationLedgerDirectoryOutcome.Finalized) {
    return canonicalU8(DIRECTORY_FINALIZED_OUTCOME_DISCRIMINANT)
  }
  return concatCanonicalBytes([
    canonicalU8(DIRECTORY_ISOLATED_OUTCOME_DISCRIMINANT),
    canonicalFrame(canonicalText(outcome.fault.domain)),
    canonicalFrame(canonicalText(outcome.fault.scope)),
    canonicalFrame(canonicalText(outcome.fault.code)),
  ])
}

export function decodeDirectoryOutcome(bytes: Uint8Array): MaterializationDirectoryFinalization {
  const reader = CanonicalRecordReader.value(bytes)
  const discriminant = reader.byte('directory outcome')
  if (discriminant === DIRECTORY_FINALIZED_OUTCOME_DISCRIMINANT) {
    reader.finish('finalized directory outcome')
    return Object.freeze({ kind: MaterializationLedgerDirectoryOutcome.Finalized })
  }
  requireDiscriminant(
    discriminant,
    DIRECTORY_ISOLATED_OUTCOME_DISCRIMINANT,
    'directory outcome',
  )
  const candidate = {
    domain: decodeText(reader.frame('fault domain'), 'fault domain'),
    scope: decodeText(reader.frame('fault scope'), 'fault scope'),
    code: decodeText(reader.frame('fault code'), 'fault code'),
  }
  reader.finish('isolated directory outcome')
  if (!isFault(candidate)) throw new TypeError('isolated directory fault is invalid')
  return Object.freeze({
    kind: MaterializationLedgerDirectoryOutcome.IsolatedFailure,
    fault: Object.freeze({ ...candidate }) as Fault,
  })
}

export function canonicalMaterializationPath(
  pathInput: readonly string[],
): CanonicalBytes {
  const path = snapshotMaterializationRootRelativePath(pathInput)
  return concatCanonicalBytes([
    canonicalU64(BigInt(path.length)),
    ...path.map(segment => canonicalFrame(canonicalText(segment))),
  ])
}

export function decodeMaterializationPath(bytes: Uint8Array): MaterializationRootRelativePath {
  const reader = CanonicalRecordReader.value(bytes)
  const count = reader.u64('materialization path segment count')
  if (count > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new TypeError('materialization path segment count exceeds the runtime bound')
  }
  const segments: string[] = []
  for (let index = 0; index < Number(count); index += 1) {
    segments.push(decodeText(reader.frame('materialization path segment'), 'path segment'))
  }
  reader.finish('materialization relative path')
  return snapshotMaterializationRootRelativePath(segments)
}

export function decodeText(bytes: Uint8Array, label: string): string {
  let value: string
  try {
    value = TEXT_DECODER.decode(bytes)
  } catch (cause) {
    throw new TypeError(`${label} is not well-formed UTF-8`, { cause })
  }
  if (!equalCanonicalBytes(canonicalText(value), bytes)) {
    throw new TypeError(`${label} is not canonical text`)
  }
  return value
}

export function requireRecord(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new TypeError(`${label} must be a record`)
  }
  return value as Record<string, unknown>
}

export function requireExactKeys(
  value: object,
  expected: readonly string[],
  label: string,
): void {
  const actual = Object.keys(value).sort()
  const wanted = [...expected].sort()
  if (actual.length !== wanted.length ||
      actual.some((key, index) => key !== wanted[index])) {
    throw new TypeError(`${label} fields are not exact`)
  }
}

export function requireDiscriminant(actual: number, expected: number, label: string): void {
  if (actual !== expected) throw new TypeError(`${label} discriminant is invalid`)
}

export function u64(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > MATERIALIZATION_LEDGER_U64_MAX) {
    throw new RangeError(`${label} must be a u64`)
  }
  return value
}

export function hasOwn(value: object, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(value, key)
}
