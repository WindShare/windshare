import type { DirectoryAdmissionScope } from '../../transfer/directory-admission'
import type { DirectTreeRootExpectation } from '../../transfer/job/coordinate/direct-tree'
import type { MaterializationSummary } from '../../transfer/output/session'
import type {
  PersistentDirectTreeDirectoryEvidence,
  PersistentDirectTreeMaterializationEvidence,
} from '../execution/persistent-execution'
import { TargetOwnershipUnknownError } from '../persistent-tree/errors'
import type { FSACheckpointSettlementSnapshot } from './settlement-ledger'
import type { DirectTreeIntent } from './settlement-proof'

const U64_MAX = 0xffff_ffff_ffff_ffffn

export type FSASettlementEvidenceOutcome = 'published' | 'partial' | 'resumable'

export interface ValidatedFSASettlementEvidence {
  readonly fileCount: bigint
  readonly visibleDirectoryCount: bigint
  readonly materializedDirectoryCount: bigint
  readonly finalizedDirectoryCount: bigint
  readonly isolatedDirectoryCount: bigint
  readonly completedBytes: bigint
}

export interface ValidateFSASettlementEvidenceOptions {
  readonly intent: DirectTreeIntent
  readonly directoryScope: DirectoryAdmissionScope
  readonly evidence: PersistentDirectTreeMaterializationEvidence
  readonly snapshot: FSACheckpointSettlementSnapshot
  readonly summary: MaterializationSummary
  readonly outcome: FSASettlementEvidenceOutcome
}

/**
 * Joins durable committed file checkpoints with the execution's directory admissions
 * and worker summary without maintaining a separate ledger or Merkle tree.
 */
export function validateFSASettlementEvidence(
  options: ValidateFSASettlementEvidenceOptions,
): ValidatedFSASettlementEvidence {
  if (options.evidence.kind !== 'direct-tree-checkpoints' ||
      options.evidence.materializationBindingDigest !== options.intent.plan.reservation.digest) {
    throw new TargetOwnershipUnknownError('settlement', options.intent.operationId)
  }
  if (options.directoryScope.receiveIntentDigest !== options.intent.digest) {
    throw new TypeError('FSA settlement root expectation belongs to another receive intent')
  }
  const { snapshot, summary, outcome } = options
  const directories = options.evidence.directories
  const expectedEntryCount = checkedAdd(
    snapshot.fileCount,
    directories.visibleCount,
    'visible materialization entry count',
  )
  if (summary.entryCount !== expectedEntryCount ||
      summary.fileCount !== snapshot.fileCount ||
      summary.directoryCount !== directories.visibleCount ||
      summary.rawBytes !== snapshot.completedBytes) {
    throw new TypeError('worker summary disagrees with durable checkpoint evidence')
  }
  validateRootExpectation(
    snapshot,
    directories,
    options.directoryScope.rootExpectation,
    outcome === 'published',
  )
  if (outcome === 'published') {
    if (directories.finalizedCount !== directories.materializedCount ||
        directories.isolatedCount !== 0n) {
      throw new TypeError('published materialization requires every directory to finalize')
    }
  } else if (outcome === 'partial') {
    if (checkedAdd(
      directories.finalizedCount,
      directories.isolatedCount,
      'typed directory outcome count',
    ) !== directories.materializedCount) {
      throw new TypeError('partial terminal materialization has an untyped directory outcome')
    }
  }
  return Object.freeze({
    fileCount: snapshot.fileCount,
    visibleDirectoryCount: directories.visibleCount,
    materializedDirectoryCount: directories.materializedCount,
    finalizedDirectoryCount: directories.finalizedCount,
    isolatedDirectoryCount: directories.isolatedCount,
    completedBytes: snapshot.completedBytes,
  })
}

function validateRootExpectation(
  snapshot: FSACheckpointSettlementSnapshot,
  directories: PersistentDirectTreeDirectoryEvidence,
  expectation: DirectTreeRootExpectation,
  requireComplete: boolean,
): void {
  if (expectation.kind === 'none') {
    if (directories.rootDirectory !== undefined || directories.materializedCount !== 0n) {
      throw new TypeError('single-file materialization has unexpected directory-root evidence')
    }
    if (requireComplete && (!snapshot.rootFileFinalized || snapshot.fileCount !== 1n)) {
      throw new TypeError('published single-file materialization lacks its [] file checkpoint')
    }
    return
  }
  if (directories.rootDirectory === undefined) {
    if (requireComplete || directories.materializedCount !== 0n) {
      throw new TypeError('materialized directory root evidence is missing')
    }
    return
  }
  if (snapshot.rootFileFinalized) {
    throw new TypeError('directory materialization root was claimed by a file')
  }
  if (directories.rootDirectory.directoryId !== expectation.directoryId ||
      !samePath(directories.rootDirectory.relativePath, expectation.relativePath)) {
    throw new TypeError('materialized directory root identity or path disagrees with expectation')
  }
  if (requireComplete && !directories.rootDirectory.finalized) {
    throw new TypeError('published materialization root directory is not finalized')
  }
}

function checkedAdd(left: bigint, right: bigint, label: string): bigint {
  if (typeof left !== 'bigint' || typeof right !== 'bigint' ||
      left < 0n || right < 0n || left > U64_MAX - right) {
    throw new RangeError(`${label} exceeds u64`)
  }
  return left + right
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((segment, index) => segment === right[index])
}
