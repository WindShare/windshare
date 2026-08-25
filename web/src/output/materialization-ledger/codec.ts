import { DIRECTORY_ADMISSION_LAYOUT_VERSION } from '../../transfer/directory-admission'
import {
  sameMaterializationRootRelativePath,
  snapshotMaterializationRootRelativePath,
  type MaterializationRootRelativePath,
} from '../../transfer/job/coordinate/direct-tree'
import {
  FILE_CHECKPOINT_ID_BYTES,
  FILE_ID_BYTES,
  FILE_REVISION_BYTES,
  OPERATION_ID_BYTES,
} from '../persistence/checkpoint'
import { CanonicalRecordReader } from '../workspace/canonical-reader'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalRecord,
  canonicalU8,
  concatCanonicalBytes,
  equalCanonicalBytes,
  snapshotCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from '../workspace/canonical'
import {
  MATERIALIZATION_LEDGER_DIRECTORY_FINALIZATION_ORDER,
  MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER,
  MATERIALIZATION_LEDGER_SCHEMA_VERSION,
  MATERIALIZATION_LEDGER_U64_MAX,
  MaterializationLedgerDirectoryOutcome,
  MaterializationLedgerEntryKind,
  canonicalMaterializationPath,
  decodeMaterializationPath,
  requireDiscriminant,
  requireExactKeys,
  requireRecord,
  u64,
  type MaterializationLedgerAppendClassification,
  type MaterializationLedgerBindingInput,
  type MaterializationLedgerBindingV1,
  type MaterializationLedgerEntryCursor,
  type MaterializationLedgerEntryOrder,
  type MaterializationLedgerEntryV1,
  type MaterializationLedgerRootFact,
  type MaterializationLedgerRootFinalization,
} from './model'

const LEDGER_BINDING_DOMAIN = 'windshare/materialization-ledger/v1/binding'
const LEDGER_PATH_DOMAIN = 'windshare/materialization-ledger/v1/path'

const MATERIALIZER_FSA_TREE_DISCRIMINANT = 1
const FSA_RESERVED_ROOT_RELATIVE_DISCRIMINANT = 1
const ROOT_ABSENT_DISCRIMINANT = 0
const ROOT_FILE_DISCRIMINANT = 1
const ROOT_DIRECTORY_DISCRIMINANT = 2
const ROOT_FINALIZATION_MISSING_DISCRIMINANT = 0
const ROOT_FINALIZATION_FINALIZED_DISCRIMINANT = 1
const ROOT_FINALIZATION_ISOLATED_DISCRIMINANT = 2

export async function createMaterializationLedgerBinding(
  input: MaterializationLedgerBindingInput,
): Promise<MaterializationLedgerBindingV1> {
  requireExactKeys(input, [
    'operationId',
    'receiveIntentDigest',
    'materializationBindingDigest',
    'authorityRef',
  ], 'materialization ledger binding')
  const operationId = snapshotIdentity(input.operationId, OPERATION_ID_BYTES, 'operation ID')
  const receiveIntentDigest = snapshotIdentity(
    input.receiveIntentDigest,
    FILE_CHECKPOINT_ID_BYTES,
    'receive intent digest',
  )
  const materializationBindingDigest = snapshotIdentity(
    input.materializationBindingDigest,
    FILE_CHECKPOINT_ID_BYTES,
    'materialization binding digest',
  )
  const authorityRef = snapshotIdentity(
    input.authorityRef,
    FILE_CHECKPOINT_ID_BYTES,
    'authority reference',
  )
  const canonicalBytes = canonicalRecord(LEDGER_BINDING_DOMAIN, MATERIALIZATION_LEDGER_SCHEMA_VERSION, [
    canonicalFrame(canonicalIdentity(operationId, OPERATION_ID_BYTES, 'operation ID')),
    canonicalFrame(canonicalIdentity(
      receiveIntentDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'receive intent digest',
    )),
    canonicalFrame(canonicalIdentity(
      materializationBindingDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'materialization binding digest',
    )),
    canonicalFrame(canonicalIdentity(authorityRef, FILE_CHECKPOINT_ID_BYTES, 'authority reference')),
    canonicalU8(MATERIALIZER_FSA_TREE_DISCRIMINANT),
    canonicalU8(FSA_RESERVED_ROOT_RELATIVE_DISCRIMINANT),
    canonicalU8(DIRECTORY_ADMISSION_LAYOUT_VERSION),
  ])
  return Object.freeze({
    schemaVersion: MATERIALIZATION_LEDGER_SCHEMA_VERSION,
    operationId,
    receiveIntentDigest,
    materializationBindingDigest,
    authorityRef,
    materializerKind: 'fsa-tree',
    pathCoordinate: 'fsa-reserved-root-relative',
    layoutVersion: DIRECTORY_ADMISSION_LAYOUT_VERSION,
    ledgerBindingDigest: await canonicalDigest(canonicalBytes),
    canonicalBytes,
  })
}

export async function decodeMaterializationLedgerBindingV1(
  input: unknown,
): Promise<MaterializationLedgerBindingV1> {
  const record = requireRecord(input, 'materialization ledger binding')
  requireExactKeys(record, [
    'schemaVersion',
    'operationId',
    'receiveIntentDigest',
    'materializationBindingDigest',
    'authorityRef',
    'materializerKind',
    'pathCoordinate',
    'layoutVersion',
    'ledgerBindingDigest',
    'canonicalBytes',
  ], 'materialization ledger binding')
  const reader = CanonicalRecordReader.open(
    requireCanonicalBytes(record.canonicalBytes, 'ledger binding canonical bytes'),
    LEDGER_BINDING_DOMAIN,
    MATERIALIZATION_LEDGER_SCHEMA_VERSION,
  )
  const decoded = await createMaterializationLedgerBinding({
    operationId: reader.framedIdentity(OPERATION_ID_BYTES, 'operation ID'),
    receiveIntentDigest: reader.framedIdentity(
      FILE_CHECKPOINT_ID_BYTES,
      'receive intent digest',
    ),
    materializationBindingDigest: reader.framedIdentity(
      FILE_CHECKPOINT_ID_BYTES,
      'materialization binding digest',
    ),
    authorityRef: reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'authority reference'),
  })
  requireDiscriminant(
    reader.byte('materializer kind'),
    MATERIALIZER_FSA_TREE_DISCRIMINANT,
    'materializer kind',
  )
  requireDiscriminant(
    reader.byte('path coordinate'),
    FSA_RESERVED_ROOT_RELATIVE_DISCRIMINANT,
    'path coordinate',
  )
  requireDiscriminant(
    reader.byte('layout version'),
    DIRECTORY_ADMISSION_LAYOUT_VERSION,
    'layout version',
  )
  reader.finish('materialization ledger binding')
  requireRecordProjection(record, decoded, 'materialization ledger binding')
  return decoded
}

export async function materializationLedgerPathKey(
  relativePathInput: MaterializationRootRelativePath,
): Promise<string> {
  const relativePath = snapshotMaterializationRootRelativePath(relativePathInput)
  return canonicalDigest(canonicalRecord(LEDGER_PATH_DOMAIN, MATERIALIZATION_LEDGER_SCHEMA_VERSION, [
    canonicalFrame(canonicalMaterializationPath(relativePath)),
  ]))
}

export function compareMaterializationLedgerEntryCursors(
  left: MaterializationLedgerEntryCursor,
  right: MaterializationLedgerEntryCursor,
): number {
  return compareText(left.pathKey, right.pathKey) ||
    left.entryOrder - right.entryOrder ||
    compareText(left.entryId, right.entryId)
}

export function materializationLedgerEntryCursor(
  entry: MaterializationLedgerEntryV1,
): MaterializationLedgerEntryCursor {
  return Object.freeze({
    pathKey: snapshotIdentity(entry.pathKey, FILE_CHECKPOINT_ID_BYTES, 'path key'),
    entryOrder: entryOrder(entry.entryOrder),
    entryId: snapshotIdentity(entry.entryId, FILE_CHECKPOINT_ID_BYTES, 'entry ID'),
  })
}

export function snapshotMaterializationLedgerEntryCursor(
  input: MaterializationLedgerEntryCursor,
): MaterializationLedgerEntryCursor {
  requireExactKeys(input, ['pathKey', 'entryOrder', 'entryId'], 'ledger entry cursor')
  return Object.freeze({
    pathKey: snapshotIdentity(input.pathKey, FILE_CHECKPOINT_ID_BYTES, 'path key'),
    entryOrder: entryOrder(input.entryOrder),
    entryId: snapshotIdentity(input.entryId, FILE_CHECKPOINT_ID_BYTES, 'entry ID'),
  })
}

export function classifyMaterializationLedgerAppend(
  existing: MaterializationLedgerEntryV1 | undefined,
  incoming: MaterializationLedgerEntryV1,
): MaterializationLedgerAppendClassification {
  if (existing === undefined) return 'insert'
  if (existing.operationId !== incoming.operationId ||
      existing.ledgerBindingDigest !== incoming.ledgerBindingDigest ||
      existing.pathKey !== incoming.pathKey || existing.entryOrder !== incoming.entryOrder) {
    throw new TypeError('materialization ledger append compared unrelated entry keys')
  }
  if (!sameMaterializationRootRelativePath(existing.relativePath, incoming.relativePath)) {
    throw new TypeError('materialization ledger path-key collision changed canonical path bytes')
  }
  if (existing.entryId === incoming.entryId && existing.entryDigest === incoming.entryDigest &&
      equalCanonicalBytes(existing.canonicalBytes, incoming.canonicalBytes)) {
    return 'idempotent'
  }
  throw new TypeError('materialization ledger immutable path/order entry conflicts')
}

export function checkedLedgerAdd(left: bigint, right: bigint, label: string): bigint {
  const leftValue = u64(left, label)
  const rightValue = u64(right, label)
  if (leftValue > MATERIALIZATION_LEDGER_U64_MAX - rightValue) {
    throw new RangeError(`${label} exceeds u64`)
  }
  return leftValue + rightValue
}

export function canonicalMaterializationLedgerRootFact(
  factInput: MaterializationLedgerRootFact,
): CanonicalBytes {
  return canonicalRootFact(factInput)
}

export function canonicalRootFact(factInput: MaterializationLedgerRootFact): CanonicalBytes {
  const fact = snapshotRootFact(factInput)
  if (fact.kind === 'absent') return canonicalU8(ROOT_ABSENT_DISCRIMINANT)
  if (fact.kind === MaterializationLedgerEntryKind.FileFinalized) {
    return concatCanonicalBytes([
      canonicalU8(ROOT_FILE_DISCRIMINANT),
      canonicalFrame(canonicalMaterializationPath(fact.relativePath)),
      canonicalFrame(canonicalIdentity(fact.pathKey, FILE_CHECKPOINT_ID_BYTES, 'root path key')),
      canonicalFrame(canonicalIdentity(fact.entryId, FILE_CHECKPOINT_ID_BYTES, 'root entry ID')),
      canonicalFrame(canonicalIdentity(fact.fileId, FILE_ID_BYTES, 'root file ID')),
      canonicalFrame(canonicalIdentity(fact.fileRevision, FILE_REVISION_BYTES, 'root file revision')),
    ])
  }
  return concatCanonicalBytes([
    canonicalU8(ROOT_DIRECTORY_DISCRIMINANT),
    canonicalFrame(canonicalMaterializationPath(fact.relativePath)),
    canonicalFrame(canonicalIdentity(fact.pathKey, FILE_CHECKPOINT_ID_BYTES, 'root path key')),
    canonicalFrame(canonicalIdentity(
      fact.admissionEntryId,
      FILE_CHECKPOINT_ID_BYTES,
      'root directory admission ID',
    )),
    canonicalFrame(canonicalIdentity(
      fact.admissionEntryDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'root directory admission digest',
    )),
    canonicalFrame(canonicalIdentity(fact.directoryId, FILE_ID_BYTES, 'root directory ID')),
    canonicalFrame(canonicalIdentity(fact.generation, FILE_ID_BYTES, 'root directory generation')),
    canonicalFrame(canonicalIdentity(
      fact.ownedObjectId,
      FILE_CHECKPOINT_ID_BYTES,
      'root owned object ID',
    )),
    canonicalFrame(canonicalRootFinalization(fact.finalization)),
  ])
}

export function decodeRootFact(bytes: Uint8Array): MaterializationLedgerRootFact {
  const reader = CanonicalRecordReader.value(bytes)
  const discriminant = reader.byte('root path fact')
  if (discriminant === ROOT_ABSENT_DISCRIMINANT) {
    reader.finish('absent root path fact')
    return Object.freeze({ kind: 'absent' })
  }
  const relativePath = decodeMaterializationPath(reader.frame('root relative path'))
  if (relativePath.length !== 0) throw new TypeError('root path fact must name []')
  const pathKey = reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'root path key')
  if (discriminant === ROOT_FILE_DISCRIMINANT) {
    const result = snapshotRootFact({
      kind: MaterializationLedgerEntryKind.FileFinalized,
      relativePath,
      pathKey,
      entryId: reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'root entry ID'),
      fileId: reader.framedIdentity(FILE_ID_BYTES, 'root file ID'),
      fileRevision: reader.framedIdentity(FILE_REVISION_BYTES, 'root file revision'),
    })
    reader.finish('root file fact')
    return result
  }
  requireDiscriminant(discriminant, ROOT_DIRECTORY_DISCRIMINANT, 'root path fact')
  const result = snapshotRootFact({
    kind: 'directory',
    relativePath,
    pathKey,
    admissionEntryId: reader.framedIdentity(
      FILE_CHECKPOINT_ID_BYTES,
      'root directory admission ID',
    ),
    admissionEntryDigest: reader.framedIdentity(
      FILE_CHECKPOINT_ID_BYTES,
      'root directory admission digest',
    ),
    directoryId: reader.framedIdentity(FILE_ID_BYTES, 'root directory ID'),
    generation: reader.framedIdentity(FILE_ID_BYTES, 'root directory generation'),
    ownedObjectId: reader.framedIdentity(
      FILE_CHECKPOINT_ID_BYTES,
      'root owned object ID',
    ),
    finalization: decodeRootFinalization(reader.frame('root directory finalization')),
  })
  reader.finish('root directory fact')
  return result
}

export function snapshotRootFact(input: MaterializationLedgerRootFact): MaterializationLedgerRootFact {
  if (input.kind === 'absent') {
    requireExactKeys(input, ['kind'], 'absent root path fact')
    return Object.freeze({ kind: 'absent' })
  }
  const relativePath = snapshotMaterializationRootRelativePath(input.relativePath)
  if (relativePath.length !== 0) throw new TypeError('root path fact must name []')
  const pathKey = snapshotIdentity(input.pathKey, FILE_CHECKPOINT_ID_BYTES, 'root path key')
  if (input.kind === MaterializationLedgerEntryKind.FileFinalized) {
    requireExactKeys(input, [
      'kind',
      'relativePath',
      'pathKey',
      'entryId',
      'fileId',
      'fileRevision',
    ], 'root file fact')
    return Object.freeze({
      kind: MaterializationLedgerEntryKind.FileFinalized,
      relativePath,
      pathKey,
      entryId: snapshotIdentity(input.entryId, FILE_CHECKPOINT_ID_BYTES, 'root entry ID'),
      fileId: snapshotIdentity(input.fileId, FILE_ID_BYTES, 'root file ID'),
      fileRevision: snapshotIdentity(
        input.fileRevision,
        FILE_REVISION_BYTES,
        'root file revision',
      ),
    })
  }
  requireExactKeys(input, [
    'kind',
    'relativePath',
    'pathKey',
    'admissionEntryId',
    'admissionEntryDigest',
    'directoryId',
    'generation',
    'ownedObjectId',
    'finalization',
  ], 'root directory fact')
  if (input.kind !== 'directory') throw new TypeError('root path fact kind is invalid')
  return Object.freeze({
    kind: 'directory',
    relativePath,
    pathKey,
    admissionEntryId: snapshotIdentity(
      input.admissionEntryId,
      FILE_CHECKPOINT_ID_BYTES,
      'root directory admission ID',
    ),
    admissionEntryDigest: snapshotIdentity(
      input.admissionEntryDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'root directory admission digest',
    ),
    directoryId: snapshotIdentity(input.directoryId, FILE_ID_BYTES, 'root directory ID'),
    generation: snapshotIdentity(input.generation, FILE_ID_BYTES, 'root directory generation'),
    ownedObjectId: snapshotIdentity(
      input.ownedObjectId,
      FILE_CHECKPOINT_ID_BYTES,
      'root owned object ID',
    ),
    finalization: snapshotRootFinalization(input.finalization),
  })
}

function canonicalRootFinalization(input: MaterializationLedgerRootFinalization): CanonicalBytes {
  const value = snapshotRootFinalization(input)
  switch (value.kind) {
    case 'missing':
      return canonicalU8(ROOT_FINALIZATION_MISSING_DISCRIMINANT)
    case MaterializationLedgerDirectoryOutcome.Finalized:
      return concatCanonicalBytes([
        canonicalU8(ROOT_FINALIZATION_FINALIZED_DISCRIMINANT),
        canonicalFrame(canonicalIdentity(
          value.entryId,
          FILE_CHECKPOINT_ID_BYTES,
          'root finalization entry ID',
        )),
        canonicalFrame(canonicalIdentity(
          value.entryDigest,
          FILE_CHECKPOINT_ID_BYTES,
          'root finalization entry digest',
        )),
      ])
    case MaterializationLedgerDirectoryOutcome.IsolatedFailure:
      return concatCanonicalBytes([
        canonicalU8(ROOT_FINALIZATION_ISOLATED_DISCRIMINANT),
        canonicalFrame(canonicalIdentity(
          value.entryId,
          FILE_CHECKPOINT_ID_BYTES,
          'root finalization entry ID',
        )),
        canonicalFrame(canonicalIdentity(
          value.entryDigest,
          FILE_CHECKPOINT_ID_BYTES,
          'root finalization entry digest',
        )),
      ])
  }
}

function decodeRootFinalization(bytes: Uint8Array): MaterializationLedgerRootFinalization {
  const reader = CanonicalRecordReader.value(bytes)
  const discriminant = reader.byte('root finalization')
  if (discriminant === ROOT_FINALIZATION_MISSING_DISCRIMINANT) {
    reader.finish('missing root finalization')
    return Object.freeze({ kind: 'missing' })
  }
  const entryId = reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'root finalization entry ID')
  const entryDigest = reader.framedIdentity(
    FILE_CHECKPOINT_ID_BYTES,
    'root finalization entry digest',
  )
  reader.finish('root finalization')
  if (discriminant === ROOT_FINALIZATION_FINALIZED_DISCRIMINANT) {
    return Object.freeze({
      kind: MaterializationLedgerDirectoryOutcome.Finalized,
      entryId,
      entryDigest,
    })
  }
  requireDiscriminant(
    discriminant,
    ROOT_FINALIZATION_ISOLATED_DISCRIMINANT,
    'root finalization',
  )
  return Object.freeze({
    kind: MaterializationLedgerDirectoryOutcome.IsolatedFailure,
    entryId,
    entryDigest,
  })
}

function snapshotRootFinalization(
  input: MaterializationLedgerRootFinalization,
): MaterializationLedgerRootFinalization {
  if (input.kind === 'missing') {
    requireExactKeys(input, ['kind'], 'missing root finalization')
    return Object.freeze({ kind: 'missing' })
  }
  requireExactKeys(input, ['kind', 'entryId', 'entryDigest'], 'root finalization')
  if (input.kind !== MaterializationLedgerDirectoryOutcome.Finalized &&
      input.kind !== MaterializationLedgerDirectoryOutcome.IsolatedFailure) {
    throw new TypeError('root finalization kind is invalid')
  }
  return Object.freeze({
    kind: input.kind,
    entryId: snapshotIdentity(
      input.entryId,
      FILE_CHECKPOINT_ID_BYTES,
      'root finalization entry ID',
    ),
    entryDigest: snapshotIdentity(
      input.entryDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'root finalization entry digest',
    ),
  })
}

export function entryOrder(value: number): MaterializationLedgerEntryOrder {
  if (value !== MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER &&
      value !== MATERIALIZATION_LEDGER_DIRECTORY_FINALIZATION_ORDER) {
    throw new TypeError('materialization ledger entry order is invalid')
  }
  return value
}

export async function validateBinding(
  binding: MaterializationLedgerBindingV1,
): Promise<MaterializationLedgerBindingV1> {
  return decodeMaterializationLedgerBindingV1(binding)
}

export function requireExpectedBindingDigest(
  digest: string,
  binding: MaterializationLedgerBindingV1,
): void {
  if (digest !== binding.ledgerBindingDigest) {
    throw new TypeError('materialization ledger record belongs to a foreign binding')
  }
}

export function requireCanonicalBytes(value: unknown, label: string): CanonicalBytes {
  if (!(value instanceof Uint8Array)) throw new TypeError(`${label} must be bytes`)
  return snapshotCanonicalBytes(value)
}

export function requireRecordProjection(
  record: Record<string, unknown>,
  rebuilt: object,
  label: string,
): void {
  for (const key of Object.keys(rebuilt)) {
    if (!sameProjection(record[key], (rebuilt as Record<string, unknown>)[key])) {
      throw new TypeError(`${label} projection disagrees with canonical bytes`)
    }
  }
}

function sameProjection(left: unknown, right: unknown): boolean {
  if (left instanceof Uint8Array && right instanceof Uint8Array) {
    return equalCanonicalBytes(left, right)
  }
  if (Array.isArray(left) && Array.isArray(right)) {
    return left.length === right.length &&
      left.every((value, index) => sameProjection(value, right[index]))
  }
  if (typeof left === 'object' && left !== null &&
      typeof right === 'object' && right !== null) {
    const leftRecord = left as Record<string, unknown>
    const rightRecord = right as Record<string, unknown>
    const leftKeys = Object.keys(leftRecord).sort()
    const rightKeys = Object.keys(rightRecord).sort()
    return leftKeys.length === rightKeys.length &&
      leftKeys.every((key, index) =>
        key === rightKeys[index] && sameProjection(leftRecord[key], rightRecord[key]))
  }
  return left === right
}

export function nonZeroU64(value: bigint, label: string): bigint {
  const result = u64(value, label)
  if (result === 0n) throw new RangeError(`${label} must not be zero`)
  return result
}

function compareText(left: string, right: string): number {
  if (left < right) return -1
  if (left > right) return 1
  return 0
}
