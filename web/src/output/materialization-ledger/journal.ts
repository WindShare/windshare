import {
  sameMaterializationRootRelativePath,
  snapshotMaterializationRootRelativePath,
  type MaterializationRootRelativePath,
} from '../../transfer/job/coordinate/direct-tree'
import { VerifiedFinalOutputFile } from '../../transfer/output-session'
import {
  FILE_CHECKPOINT_ID_BYTES,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_ID_BYTES,
  FILE_REVISION_BYTES,
  fileCheckpointIsComplete,
  validateFileCheckpoint,
  type FileCheckpointV2,
} from '../persistence/checkpoint'
import {
  durableCheckpointNamespaceIdentity,
  sameDurableCheckpointNamespace,
} from '../persistence/namespace'
import { CanonicalRecordReader } from '../workspace/canonical-reader'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalRecord,
  canonicalText,
  canonicalU8,
  canonicalU64,
  type CanonicalBytes,
} from '../workspace/canonical'
import {
  materializationLedgerPathKey,
  requireCanonicalBytes,
  requireExpectedBindingDigest,
  requireRecordProjection,
  validateBinding,
} from './codec'
import {
  MATERIALIZATION_LEDGER_DIRECTORY_FINALIZATION_ORDER,
  MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER,
  MATERIALIZATION_LEDGER_SCHEMA_VERSION,
  MaterializationLedgerEntryKind,
  canonicalCheckpointReference,
  canonicalDirectoryOutcome,
  canonicalMaterializationPath,
  canonicalFinalOutput,
  canonicalOptionalModifiedTime,
  canonicalOptionalParent,
  checkpointReference,
  decodeCheckpointReference,
  decodeDirectoryOutcome,
  decodeFinalOutput,
  decodeMaterializationPath,
  decodeText,
  decodeStableDirectoryCoordinates,
  requireExactKeys,
  requireRecord,
  snapshotDirectoryOutcome,
  snapshotStableDirectoryCoordinates,
  type FinalFileCheckpointReference,
  type FinalizedFileMaterializationRecords,
  type MaterializationDirectoryAdmittedEntryV1,
  type MaterializationDirectoryFinalization,
  type MaterializationDirectoryFinalizedEntryV1,
  type MaterializationFileFinalizedEntryV1,
  type MaterializationFinalFileProofV1,
  type MaterializationLedgerAppendClassification,
  type MaterializationLedgerBindingV1,
  type MaterializationLedgerEntryPage,
  type MaterializationLedgerEntryOrder,
  type MaterializationLedgerEntryV1,
  type MaterializationLedgerPageRequest,
  type MaterializationLedgerPageSummaryV1,
  type MaterializationLedgerSealPurpose,
  type MaterializationLedgerSealV1,
  type StableDirectoryCoordinates,
} from './model'

const LEDGER_PROOF_ID_DOMAIN = 'windshare/materialization-ledger/v1/final-proof-id'
const LEDGER_PROOF_DOMAIN = 'windshare/materialization-ledger/v1/final-proof'
const LEDGER_ENTRY_ID_DOMAIN = 'windshare/materialization-ledger/v1/entry-id'
const LEDGER_ENTRY_DOMAIN = 'windshare/materialization-ledger/v1/entry'

const FILE_FINALIZED_DISCRIMINANT = 1
const DIRECTORY_ADMITTED_DISCRIMINANT = 2
const DIRECTORY_FINALIZED_DISCRIMINANT = 3
export interface FinalFileMaterializationCommit {
  readonly binding: MaterializationLedgerBindingV1
  readonly expectedCommittedCheckpoint: FileCheckpointV2
  readonly records: FinalizedFileMaterializationRecords
  readonly expectedPersistedOwnedFileIdentity: string
}

export interface FinalFileMaterializationCommitReceipt {
  readonly classification: MaterializationLedgerAppendClassification
  readonly finalCheckpoint: FileCheckpointV2
  readonly finalProof: MaterializationFinalFileProofV1
  readonly ledgerEntry: FinalizedFileMaterializationRecords['ledgerEntry']
}

export interface FinalFileMaterializationJournal {
  commitFinalFile(
    input: FinalFileMaterializationCommit,
  ): Promise<FinalFileMaterializationCommitReceipt>
}

export interface DirectoryMaterializationLedgerJournal {
  appendDirectoryAdmission(
    binding: MaterializationLedgerBindingV1,
    entry: MaterializationDirectoryAdmittedEntryV1,
  ): Promise<MaterializationLedgerAppendClassification>
  appendDirectoryFinalization(
    binding: MaterializationLedgerBindingV1,
    entry: MaterializationDirectoryFinalizedEntryV1,
  ): Promise<MaterializationLedgerAppendClassification>
}

export interface MaterializationLedgerEntryReader {
  scanMaterializationLedgerEntries(
    binding: MaterializationLedgerBindingV1,
    request: MaterializationLedgerPageRequest,
  ): Promise<MaterializationLedgerEntryPage>
}

export interface MaterializationLedgerPageSummaryScan {
  readonly pages: readonly MaterializationLedgerPageSummaryV1[]
  readonly continuationPageOrdinal?: bigint
}

export interface MaterializationLedgerSealJournal extends MaterializationLedgerEntryReader {
  countCheckpointCandidates(binding: MaterializationLedgerBindingV1): Promise<bigint>
  persistMaterializationLedgerPage(
    binding: MaterializationLedgerBindingV1,
    page: MaterializationLedgerPageSummaryV1,
  ): Promise<MaterializationLedgerAppendClassification>
  scanMaterializationLedgerPages(
    binding: MaterializationLedgerBindingV1,
    sealId: string,
    afterPageOrdinal?: bigint,
  ): Promise<MaterializationLedgerPageSummaryScan>
  persistMaterializationLedgerSeal(
    binding: MaterializationLedgerBindingV1,
    seal: MaterializationLedgerSealV1,
  ): Promise<MaterializationLedgerAppendClassification>
  sealMaterializationLedger(input: Readonly<{
    binding: MaterializationLedgerBindingV1
    sealSequence: bigint
    purpose: MaterializationLedgerSealPurpose
  }>): Promise<MaterializationLedgerSealV1>
}

export interface MaterializationFinalProofReader {
  readMaterializationFinalProof(
    binding: MaterializationLedgerBindingV1,
    proofId: string,
  ): Promise<MaterializationFinalFileProofV1 | undefined>
}

export interface MaterializationLedgerRetirementResult {
  readonly deletedRows: number
  readonly state: 'more' | 'complete'
}

export interface MaterializationLedgerRetirementJournal {
  retireMaterializationLedgerBatch(
    binding: MaterializationLedgerBindingV1,
    limit: typeof MATERIALIZATION_LEDGER_PAGE_ENTRY_LIMIT,
  ): Promise<MaterializationLedgerRetirementResult>
}

export interface MaterializationLedgerJournal extends
  FinalFileMaterializationJournal,
  DirectoryMaterializationLedgerJournal,
  MaterializationLedgerSealJournal,
  MaterializationFinalProofReader,
  MaterializationLedgerRetirementJournal {}

export async function createFinalizedFileMaterializationRecords(input: {
  readonly binding: MaterializationLedgerBindingV1
  readonly finalOutput: VerifiedFinalOutputFile
  readonly finalCheckpoint: FileCheckpointV2
}): Promise<FinalizedFileMaterializationRecords> {
  requireExactKeys(input, ['binding', 'finalOutput', 'finalCheckpoint'], 'final file records')
  const binding = await validateBinding(input.binding)
  validateFileCheckpoint(input.finalCheckpoint)
  validateFinalCheckpointBinding(binding, input.finalCheckpoint)
  const finalOutput = snapshotFinalOutput(input.finalOutput)
  validateFinalOutputAgainstCheckpoint(finalOutput, input.finalCheckpoint)
  const checkpoint = checkpointReference(input.finalCheckpoint)
  const finalProof = await createFinalFileProof(binding, finalOutput, checkpoint)
  const ledgerEntry = await createFileFinalizedEntry(binding, finalOutput, checkpoint, finalProof)
  return Object.freeze({
    finalProof,
    ledgerEntry,
    finalCheckpoint: input.finalCheckpoint,
  })
}

export async function decodeMaterializationFinalFileProofV1(
  input: unknown,
  bindingInput: MaterializationLedgerBindingV1,
): Promise<MaterializationFinalFileProofV1> {
  const binding = await validateBinding(bindingInput)
  const record = requireRecord(input, 'materialization final file proof')
  requireExactKeys(record, [
    'schemaVersion',
    'kind',
    'operationId',
    'ledgerBindingDigest',
    'proofId',
    'proofDigest',
    'recordId',
    'fileId',
    'finalOutput',
    'checkpoint',
    'canonicalBytes',
  ], 'materialization final file proof')
  const reader = CanonicalRecordReader.open(
    requireCanonicalBytes(record.canonicalBytes, 'final proof canonical bytes'),
    LEDGER_PROOF_DOMAIN,
    MATERIALIZATION_LEDGER_SCHEMA_VERSION,
  )
  const ledgerBindingDigest = reader.framedIdentity(
    FILE_CHECKPOINT_ID_BYTES,
    'ledger binding digest',
  )
  requireExpectedBindingDigest(ledgerBindingDigest, binding)
  const checkpoint = decodeCheckpointReference(reader.frame('checkpoint reference'))
  const finalOutput = decodeFinalOutput(reader.frame('verified final output'))
  reader.finish('materialization final file proof')
  const rebuilt = await createFinalFileProof(binding, finalOutput, checkpoint)
  requireRecordProjection(record, rebuilt, 'materialization final file proof')
  return rebuilt
}

export async function decodeMaterializationLedgerEntryV1(
  input: unknown,
  bindingInput: MaterializationLedgerBindingV1,
): Promise<MaterializationLedgerEntryV1> {
  const binding = await validateBinding(bindingInput)
  const record = requireRecord(input, 'materialization ledger entry')
  const canonicalBytes = requireCanonicalBytes(record.canonicalBytes, 'ledger entry canonical bytes')
  const reader = CanonicalRecordReader.open(
    canonicalBytes,
    LEDGER_ENTRY_DOMAIN,
    MATERIALIZATION_LEDGER_SCHEMA_VERSION,
  )
  const discriminant = reader.byte('ledger entry kind')
  const ledgerBindingDigest = reader.framedIdentity(
    FILE_CHECKPOINT_ID_BYTES,
    'ledger binding digest',
  )
  requireExpectedBindingDigest(ledgerBindingDigest, binding)
  const pathKey = reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'path key')
  const relativePath = decodeMaterializationPath(reader.frame('materialization relative path'))
  if (await materializationLedgerPathKey(relativePath) !== pathKey) {
    throw new TypeError('materialization ledger path projection disagrees with canonical bytes')
  }
  let rebuilt: MaterializationLedgerEntryV1
  switch (discriminant) {
    case FILE_FINALIZED_DISCRIMINANT:
      rebuilt = await decodeFileFinalizedEntry(reader, binding, relativePath)
      break
    case DIRECTORY_ADMITTED_DISCRIMINANT:
      rebuilt = await decodeDirectoryAdmittedEntry(reader, binding, relativePath)
      break
    case DIRECTORY_FINALIZED_DISCRIMINANT:
      rebuilt = await decodeDirectoryFinalizedEntry(reader, binding, relativePath)
      break
    default:
      throw new TypeError('materialization ledger entry kind is invalid')
  }
  reader.finish('materialization ledger entry')
  requireExactKeys(record, Object.keys(rebuilt), 'materialization ledger entry')
  requireRecordProjection(record, rebuilt, 'materialization ledger entry')
  return rebuilt
}

export async function createMaterializationDirectoryAdmittedEntry(
  bindingInput: MaterializationLedgerBindingV1,
  coordinatesInput: StableDirectoryCoordinates,
): Promise<MaterializationDirectoryAdmittedEntryV1> {
  const binding = await validateBinding(bindingInput)
  const coordinates = snapshotStableDirectoryCoordinates(coordinatesInput)
  const pathKey = await materializationLedgerPathKey(coordinates.relativePath)
  const entryId = await materializationLedgerEntryId(
    binding,
    pathKey,
    MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER,
  )
  const canonicalBytes = canonicalDirectoryEntryBytes(
    DIRECTORY_ADMITTED_DISCRIMINANT,
    binding,
    pathKey,
    coordinates,
    [],
  )
  return Object.freeze({
    schemaVersion: MATERIALIZATION_LEDGER_SCHEMA_VERSION,
    operationId: binding.operationId,
    ledgerBindingDigest: binding.ledgerBindingDigest,
    entryId,
    entryDigest: await canonicalDigest(canonicalBytes),
    pathKey,
    entryOrder: MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER,
    kind: MaterializationLedgerEntryKind.DirectoryAdmitted,
    ...coordinates,
    canonicalBytes,
  })
}

export async function createMaterializationDirectoryFinalizedEntry(
  bindingInput: MaterializationLedgerBindingV1,
  admissionInput: MaterializationDirectoryAdmittedEntryV1,
  outcomeInput: MaterializationDirectoryFinalization,
): Promise<MaterializationDirectoryFinalizedEntryV1> {
  const binding = await validateBinding(bindingInput)
  const admission = await decodeMaterializationLedgerEntryV1(admissionInput, binding)
  if (admission.kind !== MaterializationLedgerEntryKind.DirectoryAdmitted) {
    throw new TypeError('directory finalization requires the exact admitted directory entry')
  }
  const outcome = snapshotDirectoryOutcome(outcomeInput)
  const entryId = await materializationLedgerEntryId(
    binding,
    admission.pathKey,
    MATERIALIZATION_LEDGER_DIRECTORY_FINALIZATION_ORDER,
  )
  const canonicalBytes = canonicalDirectoryEntryBytes(
    DIRECTORY_FINALIZED_DISCRIMINANT,
    binding,
    admission.pathKey,
    admission,
    [
      canonicalFrame(canonicalIdentity(
        admission.entryId,
        FILE_CHECKPOINT_ID_BYTES,
        'directory admission entry ID',
      )),
      canonicalFrame(canonicalIdentity(
        admission.entryDigest,
        FILE_CHECKPOINT_ID_BYTES,
        'directory admission entry digest',
      )),
      canonicalFrame(canonicalDirectoryOutcome(outcome)),
    ],
  )
  return Object.freeze({
    schemaVersion: MATERIALIZATION_LEDGER_SCHEMA_VERSION,
    operationId: binding.operationId,
    ledgerBindingDigest: binding.ledgerBindingDigest,
    entryId,
    entryDigest: await canonicalDigest(canonicalBytes),
    pathKey: admission.pathKey,
    entryOrder: MATERIALIZATION_LEDGER_DIRECTORY_FINALIZATION_ORDER,
    kind: MaterializationLedgerEntryKind.DirectoryFinalized,
    relativePath: admission.relativePath,
    directoryId: admission.directoryId,
    generation: admission.generation,
    ownedObjectId: admission.ownedObjectId,
    ...(admission.parent === undefined ? {} : { parent: admission.parent }),
    ...(admission.modifiedTime === undefined ? {} : { modifiedTime: admission.modifiedTime }),
    admissionEntryId: admission.entryId,
    admissionEntryDigest: admission.entryDigest,
    outcome,
    canonicalBytes,
  })
}

function validateFinalCheckpointBinding(
  binding: MaterializationLedgerBindingV1,
  checkpoint: FileCheckpointV2,
): void {
  const checkpointBinding = durableCheckpointNamespaceIdentity(checkpoint)
  const expected = durableCheckpointNamespaceIdentity({
    operationId: binding.operationId,
    receiveIntentDigest: binding.receiveIntentDigest,
    materializationBindingDigest: binding.materializationBindingDigest,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: binding.authorityRef,
  })
  if (!sameDurableCheckpointNamespace(checkpointBinding, expected) ||
      !fileCheckpointIsComplete(checkpoint)) {
    throw new TypeError('final checkpoint is foreign or incomplete for the ledger binding')
  }
}

function snapshotFinalOutput(input: VerifiedFinalOutputFile): VerifiedFinalOutputFile {
  requireExactKeys(input, ['ownership', 'source', 'fileSize'], 'verified final output')
  return new VerifiedFinalOutputFile(input.ownership, input.source, input.fileSize)
}

function validateFinalOutputAgainstCheckpoint(
  proof: VerifiedFinalOutputFile,
  checkpoint: FileCheckpointV2,
): void {
  if (proof.source.fileId !== checkpoint.fileId ||
      proof.source.fileRevision !== checkpoint.fileRevision ||
      proof.fileSize !== checkpoint.exactSize ||
      proof.ownership.ownedFileIdentity !== checkpoint.ownedObjectId ||
      !sameMaterializationRootRelativePath(
        snapshotMaterializationRootRelativePath(proof.ownership.canonicalPath),
        snapshotMaterializationRootRelativePath(checkpoint.canonicalPath),
      )) {
    throw new TypeError('verified final output disagrees with its final checkpoint')
  }
}

async function createFinalFileProof(
  binding: MaterializationLedgerBindingV1,
  finalOutput: VerifiedFinalOutputFile,
  checkpoint: FinalFileCheckpointReference,
): Promise<MaterializationFinalFileProofV1> {
  const proofId = await canonicalDigest(canonicalRecord(
    LEDGER_PROOF_ID_DOMAIN,
    MATERIALIZATION_LEDGER_SCHEMA_VERSION,
    [
      canonicalFrame(canonicalIdentity(
        binding.ledgerBindingDigest,
        FILE_CHECKPOINT_ID_BYTES,
        'ledger binding digest',
      )),
      canonicalFrame(canonicalIdentity(
        checkpoint.recordId,
        FILE_CHECKPOINT_ID_BYTES,
        'checkpoint record ID',
      )),
    ],
  ))
  const canonicalBytes = canonicalRecord(LEDGER_PROOF_DOMAIN, MATERIALIZATION_LEDGER_SCHEMA_VERSION, [
    canonicalFrame(canonicalIdentity(
      binding.ledgerBindingDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'ledger binding digest',
    )),
    canonicalFrame(canonicalCheckpointReference(checkpoint)),
    canonicalFrame(canonicalFinalOutput(finalOutput)),
  ])
  return Object.freeze({
    schemaVersion: MATERIALIZATION_LEDGER_SCHEMA_VERSION,
    kind: 'final-file-proof',
    operationId: binding.operationId,
    ledgerBindingDigest: binding.ledgerBindingDigest,
    proofId,
    proofDigest: await canonicalDigest(canonicalBytes),
    recordId: checkpoint.recordId,
    fileId: finalOutput.source.fileId,
    finalOutput,
    checkpoint,
    canonicalBytes,
  })
}

async function createFileFinalizedEntry(
  binding: MaterializationLedgerBindingV1,
  finalOutput: VerifiedFinalOutputFile,
  checkpoint: FinalFileCheckpointReference,
  proof: MaterializationFinalFileProofV1,
): Promise<MaterializationFileFinalizedEntryV1> {
  const relativePath = snapshotMaterializationRootRelativePath(
    finalOutput.ownership.canonicalPath,
  )
  const pathKey = await materializationLedgerPathKey(relativePath)
  const entryId = await materializationLedgerEntryId(
    binding,
    pathKey,
    MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER,
  )
  const canonicalBytes = canonicalFileEntryBytes({
    binding,
    pathKey,
    relativePath,
    shareInstance: finalOutput.source.shareInstance,
    fileId: finalOutput.source.fileId,
    fileRevision: finalOutput.source.fileRevision,
    exactSize: finalOutput.fileSize,
    outputBackend: finalOutput.ownership.backend,
    outputSessionId: finalOutput.ownership.outputSessionId,
    ownedFileIdentity: finalOutput.ownership.ownedFileIdentity,
    checkpoint,
    finalProofId: proof.proofId,
    finalProofDigest: proof.proofDigest,
  })
  return Object.freeze({
    schemaVersion: MATERIALIZATION_LEDGER_SCHEMA_VERSION,
    operationId: binding.operationId,
    ledgerBindingDigest: binding.ledgerBindingDigest,
    entryId,
    entryDigest: await canonicalDigest(canonicalBytes),
    pathKey,
    entryOrder: MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER,
    kind: MaterializationLedgerEntryKind.FileFinalized,
    relativePath,
    shareInstance: finalOutput.source.shareInstance,
    fileId: finalOutput.source.fileId,
    fileRevision: finalOutput.source.fileRevision,
    exactSize: finalOutput.fileSize,
    outputBackend: finalOutput.ownership.backend,
    outputSessionId: finalOutput.ownership.outputSessionId,
    ownedFileIdentity: finalOutput.ownership.ownedFileIdentity,
    checkpoint,
    finalProofId: proof.proofId,
    finalProofDigest: proof.proofDigest,
    canonicalBytes,
  })
}

async function decodeFileFinalizedEntry(
  reader: CanonicalRecordReader,
  binding: MaterializationLedgerBindingV1,
  relativePath: MaterializationRootRelativePath,
): Promise<MaterializationFileFinalizedEntryV1> {
  const values = {
    shareInstance: reader.framedIdentity(FILE_ID_BYTES, 'share instance'),
    fileId: reader.framedIdentity(FILE_ID_BYTES, 'file ID'),
    fileRevision: reader.framedIdentity(FILE_REVISION_BYTES, 'file revision'),
    exactSize: reader.framedU64('exact file size'),
    outputBackend: decodeText(reader.frame('output backend'), 'output backend'),
    outputSessionId: decodeText(reader.frame('output session ID'), 'output session ID'),
    ownedFileIdentity: decodeText(reader.frame('owned file identity'), 'owned file identity'),
    checkpoint: decodeCheckpointReference(reader.frame('checkpoint reference')),
    finalProofId: reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'final proof ID'),
    finalProofDigest: reader.framedIdentity(FILE_CHECKPOINT_ID_BYTES, 'final proof digest'),
  }
  const pathKey = await materializationLedgerPathKey(relativePath)
  const entryId = await materializationLedgerEntryId(
    binding,
    pathKey,
    MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER,
  )
  const canonicalBytes = canonicalFileEntryBytes({
    binding,
    pathKey,
    relativePath,
    ...values,
  })
  return Object.freeze({
    schemaVersion: MATERIALIZATION_LEDGER_SCHEMA_VERSION,
    operationId: binding.operationId,
    ledgerBindingDigest: binding.ledgerBindingDigest,
    entryId,
    entryDigest: await canonicalDigest(canonicalBytes),
    pathKey,
    entryOrder: MATERIALIZATION_LEDGER_PATH_CLAIM_ORDER,
    kind: MaterializationLedgerEntryKind.FileFinalized,
    relativePath,
    ...values,
    canonicalBytes,
  })
}

async function decodeDirectoryAdmittedEntry(
  reader: CanonicalRecordReader,
  binding: MaterializationLedgerBindingV1,
  relativePath: MaterializationRootRelativePath,
): Promise<MaterializationDirectoryAdmittedEntryV1> {
  const coordinates = decodeStableDirectoryCoordinates(reader, relativePath)
  return createMaterializationDirectoryAdmittedEntry(binding, coordinates)
}

async function decodeDirectoryFinalizedEntry(
  reader: CanonicalRecordReader,
  binding: MaterializationLedgerBindingV1,
  relativePath: MaterializationRootRelativePath,
): Promise<MaterializationDirectoryFinalizedEntryV1> {
  const coordinates = decodeStableDirectoryCoordinates(reader, relativePath)
  const admissionEntryId = reader.framedIdentity(
    FILE_CHECKPOINT_ID_BYTES,
    'directory admission entry ID',
  )
  const admissionEntryDigest = reader.framedIdentity(
    FILE_CHECKPOINT_ID_BYTES,
    'directory admission entry digest',
  )
  const outcome = decodeDirectoryOutcome(reader.frame('directory outcome'))
  const pathKey = await materializationLedgerPathKey(relativePath)
  const entryId = await materializationLedgerEntryId(
    binding,
    pathKey,
    MATERIALIZATION_LEDGER_DIRECTORY_FINALIZATION_ORDER,
  )
  const canonicalBytes = canonicalDirectoryEntryBytes(
    DIRECTORY_FINALIZED_DISCRIMINANT,
    binding,
    pathKey,
    coordinates,
    [
      canonicalFrame(canonicalIdentity(
        admissionEntryId,
        FILE_CHECKPOINT_ID_BYTES,
        'directory admission entry ID',
      )),
      canonicalFrame(canonicalIdentity(
        admissionEntryDigest,
        FILE_CHECKPOINT_ID_BYTES,
        'directory admission entry digest',
      )),
      canonicalFrame(canonicalDirectoryOutcome(outcome)),
    ],
  )
  return Object.freeze({
    schemaVersion: MATERIALIZATION_LEDGER_SCHEMA_VERSION,
    operationId: binding.operationId,
    ledgerBindingDigest: binding.ledgerBindingDigest,
    entryId,
    entryDigest: await canonicalDigest(canonicalBytes),
    pathKey,
    entryOrder: MATERIALIZATION_LEDGER_DIRECTORY_FINALIZATION_ORDER,
    kind: MaterializationLedgerEntryKind.DirectoryFinalized,
    ...coordinates,
    admissionEntryId,
    admissionEntryDigest,
    outcome,
    canonicalBytes,
  })
}

function canonicalFileEntryBytes(input: {
  readonly binding: MaterializationLedgerBindingV1
  readonly pathKey: string
  readonly relativePath: MaterializationRootRelativePath
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
}): CanonicalBytes {
  return canonicalRecord(LEDGER_ENTRY_DOMAIN, MATERIALIZATION_LEDGER_SCHEMA_VERSION, [
    canonicalU8(FILE_FINALIZED_DISCRIMINANT),
    canonicalFrame(canonicalIdentity(
      input.binding.ledgerBindingDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'ledger binding digest',
    )),
    canonicalFrame(canonicalIdentity(input.pathKey, FILE_CHECKPOINT_ID_BYTES, 'path key')),
    canonicalFrame(canonicalMaterializationPath(input.relativePath)),
    canonicalFrame(canonicalIdentity(input.shareInstance, FILE_ID_BYTES, 'share instance')),
    canonicalFrame(canonicalIdentity(input.fileId, FILE_ID_BYTES, 'file ID')),
    canonicalFrame(canonicalIdentity(input.fileRevision, FILE_REVISION_BYTES, 'file revision')),
    canonicalFrame(canonicalU64(input.exactSize)),
    canonicalFrame(canonicalText(input.outputBackend)),
    canonicalFrame(canonicalText(input.outputSessionId)),
    canonicalFrame(canonicalText(input.ownedFileIdentity)),
    canonicalFrame(canonicalCheckpointReference(input.checkpoint)),
    canonicalFrame(canonicalIdentity(
      input.finalProofId,
      FILE_CHECKPOINT_ID_BYTES,
      'final proof ID',
    )),
    canonicalFrame(canonicalIdentity(
      input.finalProofDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'final proof digest',
    )),
  ])
}

function canonicalDirectoryEntryBytes(
  discriminant: number,
  binding: MaterializationLedgerBindingV1,
  pathKey: string,
  coordinates: StableDirectoryCoordinates,
  suffix: readonly Uint8Array[],
): CanonicalBytes {
  return canonicalRecord(LEDGER_ENTRY_DOMAIN, MATERIALIZATION_LEDGER_SCHEMA_VERSION, [
    canonicalU8(discriminant),
    canonicalFrame(canonicalIdentity(
      binding.ledgerBindingDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'ledger binding digest',
    )),
    canonicalFrame(canonicalIdentity(pathKey, FILE_CHECKPOINT_ID_BYTES, 'path key')),
    canonicalFrame(canonicalMaterializationPath(coordinates.relativePath)),
    canonicalFrame(canonicalIdentity(coordinates.directoryId, FILE_ID_BYTES, 'directory ID')),
    canonicalFrame(canonicalIdentity(coordinates.generation, FILE_ID_BYTES, 'directory generation')),
    canonicalFrame(canonicalIdentity(
      coordinates.ownedObjectId,
      FILE_CHECKPOINT_ID_BYTES,
      'owned directory object ID',
    )),
    canonicalFrame(canonicalOptionalParent(coordinates.parent)),
    canonicalFrame(canonicalOptionalModifiedTime(coordinates.modifiedTime)),
    ...suffix,
  ])
}

async function materializationLedgerEntryId(
  binding: MaterializationLedgerBindingV1,
  pathKey: string,
  order: MaterializationLedgerEntryOrder,
): Promise<string> {
  return canonicalDigest(canonicalRecord(LEDGER_ENTRY_ID_DOMAIN, MATERIALIZATION_LEDGER_SCHEMA_VERSION, [
    canonicalFrame(canonicalIdentity(
      binding.ledgerBindingDigest,
      FILE_CHECKPOINT_ID_BYTES,
      'ledger binding digest',
    )),
    canonicalFrame(canonicalIdentity(pathKey, FILE_CHECKPOINT_ID_BYTES, 'path key')),
    canonicalU8(order),
  ]))
}
