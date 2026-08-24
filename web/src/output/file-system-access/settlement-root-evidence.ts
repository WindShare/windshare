import {
  DirectorySettlementKind,
  sameMaterializationPath,
  snapshotMaterializationPath,
  type DirectoryAdmissionScope,
} from '../../transfer/directory-admission'
import type { DirectTreeRootExpectation } from '../../transfer/job/coordinate/direct-tree'
import type { PersistentDirectorySettlementEvidence } from '../../transfer/settlement/persistent-execution'
import {
  emitOutputTrace,
  outputTraceEvent,
  type OutputDiagnosticsPorts,
} from '../diagnostics'
import type { MaterializedManifestEntry } from '../workspace/manifest'

const MAX_ROOT_EVIDENCE_DIAGNOSTIC_CANDIDATES = 8

type MaterializedDirectoryEntry = Extract<MaterializedManifestEntry, { kind: 'directory' }>

export type FSASettlementEvidenceValidationPass = 'anticipated' | 'observed'

export type FSASettlementRootMismatchReason =
  | 'unexpected-single-file-directory-evidence'
  | 'missing-root-entry'
  | 'duplicate-root-entry'
  | 'root-identity-mismatch'
  | 'root-path-mismatch'
  | 'missing-root-receipt'
  | 'duplicate-root-receipt'
  | 'root-receipt-binding-mismatch'
  | 'root-receipt-not-finalized'

export type FSASettlementRootExpectationSnapshot =
  | Readonly<{
      kind: 'none'
      anchorKind: 'single-file'
    }>
  | Readonly<{
      kind: 'materialized-directory'
      anchorKind: 'directory' | 'synthetic-root' | 'catalog-root'
      directoryId: string
      relativePath: readonly string[]
    }>

export interface FSASettlementRootCandidateSnapshot {
  readonly directoryId: string
  readonly relativePath: readonly string[]
  readonly settlementKind: typeof DirectorySettlementKind.Finalized |
    typeof DirectorySettlementKind.IsolatedFailure | 'missing'
  readonly admissionPath: readonly string[] | null
}

export interface FSASettlementRootEvidenceMismatch {
  readonly reason: FSASettlementRootMismatchReason
  readonly layout: DirectoryAdmissionScope['layout']
  readonly expected: FSASettlementRootExpectationSnapshot
  readonly actualCandidates: readonly FSASettlementRootCandidateSnapshot[]
  readonly requireComplete: boolean
}

export interface FSASettlementRootEvidenceMismatchTraceEvent {
  readonly name: 'receive.fsa.settlement.root_evidence_mismatch'
  readonly validation_pass: FSASettlementEvidenceValidationPass
  readonly operation_id: string
  readonly receive_intent_digest: string
  readonly transfer_job_id: string
  readonly layout: DirectoryAdmissionScope['layout']
  readonly anchor_kind: DirectTreeRootExpectation['anchorKind']
  readonly expected_root_kind: DirectTreeRootExpectation['kind']
  readonly expected_directory_id?: string
  readonly expected_relative_path?: readonly string[]
  readonly actual_candidates: readonly Readonly<{
    directory_id: string
    relative_path: readonly string[]
    settlement_kind: FSASettlementRootCandidateSnapshot['settlementKind']
    admission_path: readonly string[] | null
  }>[]
  readonly require_complete: boolean
  readonly reason: FSASettlementRootMismatchReason
}

export class FSASettlementRootEvidenceMismatchError extends TypeError {
  readonly mismatch: FSASettlementRootEvidenceMismatch

  constructor(mismatch: FSASettlementRootEvidenceMismatch) {
    super('FSA settlement root evidence does not match its explicit receive intent')
    this.name = 'FSASettlementRootEvidenceMismatchError'
    this.mismatch = mismatch
  }
}

export function validateFSASettlementRootEvidence(input: Readonly<{
  directoryScope: DirectoryAdmissionScope
  directories: readonly MaterializedDirectoryEntry[]
  directorySettlements: readonly PersistentDirectorySettlementEvidence[]
  requireComplete: boolean
}>): void {
  const expected = snapshotExpectation(input.directoryScope.rootExpectation)
  const directories = input.directories.map(entry => Object.freeze({
    entry,
    relativePath: snapshotMaterializationPath(entry.artifactPath),
  }))
  const settlements = input.directorySettlements.map(value => Object.freeze({
    value,
    relativePath: snapshotMaterializationPath(value.artifactPath),
    admissionPath: snapshotMaterializationPath(value.settlement.admission.path),
  }))

  if (expected.kind === 'none') {
    if (directories.length !== 0 || settlements.length !== 0) {
      mismatch(input, expected, 'unexpected-single-file-directory-evidence', directories, settlements)
    }
    return
  }

  const semanticallyRootReceipts = settlements.filter(candidate =>
    candidate.value.settlement.admission.parentToken === undefined ||
    candidate.value.settlement.admission.directoryId === expected.directoryId ||
    sameMaterializationPath(candidate.admissionPath, expected.relativePath) ||
    sameMaterializationPath(candidate.relativePath, expected.relativePath),
  )
  const receiptPaths = new Set(
    semanticallyRootReceipts.map(candidate => pathKey(candidate.relativePath)),
  )
  const rootEntries = directories.filter(candidate =>
    candidate.entry.directoryId === expected.directoryId ||
    sameMaterializationPath(candidate.relativePath, expected.relativePath) ||
    receiptPaths.has(pathKey(candidate.relativePath)),
  )
  const entryPaths = new Set(rootEntries.map(candidate => pathKey(candidate.relativePath)))
  const rootReceipts = settlements.filter(candidate =>
    semanticallyRootReceipts.includes(candidate) ||
    entryPaths.has(pathKey(candidate.relativePath)),
  )

  if (rootEntries.length === 0) {
    if (rootReceipts.length !== 0 || input.requireComplete) {
      mismatch(input, expected, 'missing-root-entry', rootEntries, rootReceipts)
    }
    return
  }
  if (rootEntries.length !== 1) {
    mismatch(input, expected, 'duplicate-root-entry', rootEntries, rootReceipts)
  }

  const root = rootEntries[0]!
  if (root.entry.directoryId !== expected.directoryId) {
    mismatch(input, expected, 'root-identity-mismatch', rootEntries, rootReceipts)
  }
  if (!sameMaterializationPath(root.relativePath, expected.relativePath)) {
    mismatch(input, expected, 'root-path-mismatch', rootEntries, rootReceipts)
  }
  if (rootReceipts.length === 0) {
    mismatch(input, expected, 'missing-root-receipt', rootEntries, rootReceipts)
  }
  if (rootReceipts.length !== 1) {
    mismatch(input, expected, 'duplicate-root-receipt', rootEntries, rootReceipts)
  }

  const receipt = rootReceipts[0]!
  const admission = receipt.value.settlement.admission
  if (!sameMaterializationPath(receipt.relativePath, root.relativePath) ||
      !sameMaterializationPath(receipt.admissionPath, root.relativePath) ||
      admission.parentToken !== undefined ||
      admission.directoryId !== root.entry.directoryId ||
      admission.generation !== root.entry.generation ||
      admission.receiveIntentDigest !== input.directoryScope.receiveIntentDigest ||
      admission.layoutVersion !== input.directoryScope.layoutVersion ||
      admission.layout !== input.directoryScope.layout) {
    mismatch(input, expected, 'root-receipt-binding-mismatch', rootEntries, rootReceipts)
  }
  if (input.requireComplete &&
      receipt.value.settlement.kind !== DirectorySettlementKind.Finalized) {
    mismatch(input, expected, 'root-receipt-not-finalized', rootEntries, rootReceipts)
  }
}

export async function observeFSASettlementRootEvidenceValidation<Value>(input: Readonly<{
  validationPass: FSASettlementEvidenceValidationPass
  operationId: string
  receiveIntentDigest: string
  transferJobId: string
  diagnostics?: OutputDiagnosticsPorts
  trace?: (event: FSASettlementRootEvidenceMismatchTraceEvent) => void
  validate: () => Value | Promise<Value>
}>): Promise<Value> {
  try {
    return await input.validate()
  } catch (error) {
    if (error instanceof FSASettlementRootEvidenceMismatchError) {
      emitRootEvidenceMismatch(input, error.mismatch)
    }
    throw error
  }
}

function emitRootEvidenceMismatch(
  input: Readonly<{
    validationPass: FSASettlementEvidenceValidationPass
    operationId: string
    receiveIntentDigest: string
    transferJobId: string
    diagnostics?: OutputDiagnosticsPorts
    trace?: (event: FSASettlementRootEvidenceMismatchTraceEvent) => void
  }>,
  mismatch: FSASettlementRootEvidenceMismatch,
): void {
  const actualCandidates = Object.freeze(mismatch.actualCandidates.map(candidate => Object.freeze({
    directory_id: candidate.directoryId,
    relative_path: candidate.relativePath,
    settlement_kind: candidate.settlementKind,
    admission_path: candidate.admissionPath,
  })))
  const facts = Object.freeze({
    validation_pass: input.validationPass,
    operation_id: input.operationId,
    receive_intent_digest: input.receiveIntentDigest,
    transfer_job_id: input.transferJobId,
    layout: mismatch.layout,
    anchor_kind: mismatch.expected.anchorKind,
    expected_root_kind: mismatch.expected.kind,
    ...(mismatch.expected.kind === 'none'
      ? {}
      : {
          expected_directory_id: mismatch.expected.directoryId,
          expected_relative_path: mismatch.expected.relativePath,
        }),
    actual_candidates: actualCandidates,
    require_complete: mismatch.requireComplete,
    reason: mismatch.reason,
  })
  emitOutputTrace(input.diagnostics?.trace, () =>
    outputTraceEvent('settlement_root_evidence_mismatch', facts))
  try {
    input.trace?.(Object.freeze({
      name: 'receive.fsa.settlement.root_evidence_mismatch',
      ...facts,
    }))
  } catch {
    // Settlement validation remains authoritative when a diagnostic observer fails.
  }
}

function mismatch(
  input: Readonly<{
    directoryScope: DirectoryAdmissionScope
    requireComplete: boolean
  }>,
  expected: FSASettlementRootExpectationSnapshot,
  reason: FSASettlementRootMismatchReason,
  directories: readonly Readonly<{
    entry: MaterializedDirectoryEntry
    relativePath: readonly string[]
  }>[],
  settlements: readonly Readonly<{
    value: PersistentDirectorySettlementEvidence
    relativePath: readonly string[]
    admissionPath: readonly string[]
  }>[],
): never {
  throw new FSASettlementRootEvidenceMismatchError(Object.freeze({
    reason,
    layout: input.directoryScope.layout,
    expected,
    actualCandidates: snapshotCandidates(directories, settlements),
    requireComplete: input.requireComplete,
  }))
}

function snapshotCandidates(
  directories: readonly Readonly<{
    entry: MaterializedDirectoryEntry
    relativePath: readonly string[]
  }>[],
  settlements: readonly Readonly<{
    value: PersistentDirectorySettlementEvidence
    relativePath: readonly string[]
    admissionPath: readonly string[]
  }>[],
): readonly FSASettlementRootCandidateSnapshot[] {
  const candidates: FSASettlementRootCandidateSnapshot[] = []
  const matchedSettlements = new Set<number>()
  for (const directory of directories) {
    const matches = settlements
      .map((settlement, index) => Object.freeze({ settlement, index }))
      .filter(({ settlement }) =>
        sameMaterializationPath(settlement.relativePath, directory.relativePath))
    if (matches.length === 0) {
      candidates.push(candidateSnapshot(
        directory.entry.directoryId,
        directory.relativePath,
        'missing',
        null,
      ))
    } else {
      for (const { settlement, index } of matches) {
        matchedSettlements.add(index)
        candidates.push(candidateSnapshot(
          directory.entry.directoryId,
          directory.relativePath,
          settlement.value.settlement.kind,
          settlement.admissionPath,
        ))
      }
    }
  }
  settlements.forEach((settlement, index) => {
    if (matchedSettlements.has(index)) return
    candidates.push(candidateSnapshot(
      settlement.value.settlement.admission.directoryId,
      settlement.relativePath,
      settlement.value.settlement.kind,
      settlement.admissionPath,
    ))
  })
  return Object.freeze(candidates.slice(0, MAX_ROOT_EVIDENCE_DIAGNOSTIC_CANDIDATES))
}

function candidateSnapshot(
  directoryId: string,
  relativePath: readonly string[],
  settlementKind: FSASettlementRootCandidateSnapshot['settlementKind'],
  admissionPath: readonly string[] | null,
): FSASettlementRootCandidateSnapshot {
  return Object.freeze({
    directoryId,
    relativePath: Object.freeze([...relativePath]),
    settlementKind,
    admissionPath: admissionPath === null ? null : Object.freeze([...admissionPath]),
  })
}

function snapshotExpectation(
  input: DirectTreeRootExpectation,
): FSASettlementRootExpectationSnapshot {
  if (input.kind === 'none') {
    return Object.freeze({ kind: input.kind, anchorKind: input.anchorKind })
  }
  return Object.freeze({
    kind: input.kind,
    anchorKind: input.anchorKind,
    directoryId: input.directoryId,
    relativePath: snapshotMaterializationPath(input.relativePath),
  })
}

function pathKey(path: readonly string[]): string {
  return JSON.stringify(path)
}
