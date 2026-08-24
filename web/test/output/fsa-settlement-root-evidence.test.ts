import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import type { OutputTraceEvent } from '../../src/output/diagnostics'
import {
  FSASettlementRootEvidenceMismatchError,
  observeFSASettlementRootEvidenceValidation,
  validateFSASettlementRootEvidence,
  type FSASettlementRootEvidenceMismatchTraceEvent,
} from '../../src/output/file-system-access/settlement-root-evidence'
import type { MaterializedManifestEntry } from '../../src/output/workspace/manifest'
import {
  DIRECTORY_ADMISSION_LAYOUT_VERSION,
  DIRECTORY_ADMISSION_SCHEMA_VERSION,
  DirectorySettlementKind,
  snapshotMaterializationPath,
  type DirectoryAdmissionScope,
} from '../../src/transfer/directory-admission'
import {
  FaultScope,
  OutputFaultCode,
  outputFault,
} from '../../src/transfer/fault'
import type { PersistentDirectorySettlementEvidence } from '../../src/transfer/settlement/persistent-execution'

type DirectoryEntry = Extract<MaterializedManifestEntry, { kind: 'directory' }>

const EXPECTED_DIRECTORY_ID = identity(1)
const EXPECTED_GENERATION = identity(2)
const EXPECTED_PATH = snapshotMaterializationPath([])
const RECEIVE_INTENT_DIGEST = identity(3, 32)
const OPERATION_ID = identity(4)
const TRANSFER_JOB_ID = identity(5)

const DIRECTORY_SCOPE: DirectoryAdmissionScope = Object.freeze({
  receiveIntentDigest: RECEIVE_INTENT_DIGEST,
  layoutVersion: DIRECTORY_ADMISSION_LAYOUT_VERSION,
  layout: 'directory-tree-result-root',
  rootExpectation: Object.freeze({
    kind: 'materialized-directory',
    anchorKind: 'directory',
    directoryId: EXPECTED_DIRECTORY_ID,
    relativePath: EXPECTED_PATH,
  }),
})

const SINGLE_FILE_SCOPE: DirectoryAdmissionScope = Object.freeze({
  receiveIntentDigest: RECEIVE_INTENT_DIGEST,
  layoutVersion: DIRECTORY_ADMISSION_LAYOUT_VERSION,
  layout: 'directory-tree-single-file',
  rootExpectation: Object.freeze({
    kind: 'none',
    anchorKind: 'single-file',
  }),
})

describe('FSA settlement root evidence', () => {
  it('accepts the explicit directory identity, relative path, and finalized receipt', () => {
    expect(() => validate({
      entries: [directoryEntry()],
      settlements: [directorySettlement()],
      requireComplete: true,
    })).not.toThrow()
  })

  it('rejects wrong root identity without reinterpreting the expected empty path', () => {
    expectMismatch('root-identity-mismatch', {
      entries: [directoryEntry({ directoryId: identity(10) })],
      settlements: [directorySettlement({ directoryId: identity(10) })],
    })
  })

  it('rejects a logical root name passed as the reserved-root-relative path', () => {
    expectMismatch('root-path-mismatch', {
      entries: [directoryEntry({ path: ['photos'] })],
      settlements: [directorySettlement({ path: ['photos'], admissionPath: ['photos'] })],
    })
  })

  it('rejects duplicate root entries and duplicate root receipts before generic proof sorting', () => {
    expectMismatch('duplicate-root-entry', {
      entries: [directoryEntry(), directoryEntry({ generation: identity(11) })],
      settlements: [directorySettlement()],
    })
    expectMismatch('duplicate-root-receipt', {
      entries: [directoryEntry()],
      settlements: [directorySettlement(), directorySettlement({ tokenSeed: 12 })],
    })
  })

  it('requires complete result-root output to contain its root and a finalized receipt', () => {
    expectMismatch('missing-root-entry', {
      entries: [],
      settlements: [],
    })
    expectMismatch('root-receipt-not-finalized', {
      entries: [directoryEntry()],
      settlements: [directorySettlement({ kind: DirectorySettlementKind.IsolatedFailure })],
    })
  })

  it('requires admission and entry paths to bind the same root coordinate', () => {
    expectMismatch('root-receipt-binding-mismatch', {
      entries: [directoryEntry()],
      settlements: [directorySettlement({ admissionPath: ['other'] })],
    })
  })

  it('allows a partial result to omit root evidence but rejects incomplete evidence that is present', () => {
    expect(() => validate({
      entries: [],
      settlements: [],
      requireComplete: false,
    })).not.toThrow()
    expectMismatch('missing-root-receipt', {
      entries: [directoryEntry()],
      settlements: [],
      requireComplete: false,
    })
  })

  it('requires single-file settlement to contain no directory-root evidence', () => {
    expect(() => validate({
      scope: SINGLE_FILE_SCOPE,
      entries: [],
      settlements: [],
      requireComplete: true,
    })).not.toThrow()
    expectMismatch('unexpected-single-file-directory-evidence', {
      scope: SINGLE_FILE_SCOPE,
      entries: [directoryEntry()],
      settlements: [directorySettlement()],
    })
  })

  it('emits identical bounded semantic facts for anticipated and observed failures', async () => {
    const error = captureMismatch({
      entries: [directoryEntry({ directoryId: identity(20) })],
      settlements: [directorySettlement({ directoryId: identity(20) })],
    })
    const detailed: FSASettlementRootEvidenceMismatchTraceEvent[] = []
    const generic: OutputTraceEvent[] = []

    for (const validationPass of ['anticipated', 'observed'] as const) {
      await expect(observeFSASettlementRootEvidenceValidation({
        validationPass,
        operationId: OPERATION_ID,
        receiveIntentDigest: RECEIVE_INTENT_DIGEST,
        transferJobId: TRANSFER_JOB_ID,
        diagnostics: {
          backend: 'file_system_access',
          trace: { current: event => generic.push(event) },
        },
        trace: event => detailed.push(event),
        validate: () => { throw error },
      })).rejects.toBe(error)
    }

    expect(detailed).toHaveLength(2)
    expect(detailed.map(event => event.validation_pass)).toEqual(['anticipated', 'observed'])
    for (const event of detailed) {
      expect(event).toMatchObject({
        name: 'receive.fsa.settlement.root_evidence_mismatch',
        operation_id: OPERATION_ID,
        receive_intent_digest: RECEIVE_INTENT_DIGEST,
        transfer_job_id: TRANSFER_JOB_ID,
        layout: 'directory-tree-result-root',
        anchor_kind: 'directory',
        expected_root_kind: 'materialized-directory',
        expected_directory_id: EXPECTED_DIRECTORY_ID,
        expected_relative_path: [],
        require_complete: true,
        reason: 'root-identity-mismatch',
        actual_candidates: [{
          directory_id: identity(20),
          relative_path: [],
          settlement_kind: 'finalized',
          admission_path: [],
        }],
      })
    }
    expect(generic.map(event => event.eventName)).toEqual([
      'settlement_root_evidence_mismatch',
      'settlement_root_evidence_mismatch',
    ])
  })

  it('preserves the exact validation error when every diagnostic observer throws', async () => {
    const error = captureMismatch({ entries: [], settlements: [] })
    await expect(observeFSASettlementRootEvidenceValidation({
      validationPass: 'anticipated',
      operationId: OPERATION_ID,
      receiveIntentDigest: RECEIVE_INTENT_DIGEST,
      transferJobId: TRANSFER_JOB_ID,
      diagnostics: {
        backend: 'file_system_access',
        trace: { current: () => { throw new Error('generic trace failed') } },
      },
      trace: () => { throw new Error('detailed trace failed') },
      validate: () => { throw error },
    })).rejects.toBe(error)
  })
})

function expectMismatch(
  reason: FSASettlementRootEvidenceMismatchError['mismatch']['reason'],
  input: ValidationFixture,
): void {
  const error = captureMismatch(input)
  expect(error.mismatch.reason).toBe(reason)
}

function captureMismatch(input: ValidationFixture): FSASettlementRootEvidenceMismatchError {
  try {
    validate(input)
  } catch (error) {
    if (error instanceof FSASettlementRootEvidenceMismatchError) return error
    throw error
  }
  throw new Error('fixture did not produce a root-evidence mismatch')
}

function validate(input: ValidationFixture): void {
  validateFSASettlementRootEvidence({
    directoryScope: input.scope ?? DIRECTORY_SCOPE,
    directories: input.entries,
    directorySettlements: input.settlements,
    requireComplete: input.requireComplete ?? true,
  })
}

interface ValidationFixture {
  readonly scope?: DirectoryAdmissionScope
  readonly entries: readonly DirectoryEntry[]
  readonly settlements: readonly PersistentDirectorySettlementEvidence[]
  readonly requireComplete?: boolean
}

function directoryEntry(input: Readonly<{
  directoryId?: string
  generation?: string
  path?: readonly string[]
}> = {}): DirectoryEntry {
  return Object.freeze({
    kind: 'directory',
    artifactPath: snapshotMaterializationPath(input.path ?? []),
    directoryId: input.directoryId ?? EXPECTED_DIRECTORY_ID,
    generation: input.generation ?? EXPECTED_GENERATION,
    ownedObjectId: identity(30, 32),
  })
}

function directorySettlement(input: Readonly<{
  directoryId?: string
  generation?: string
  path?: readonly string[]
  admissionPath?: readonly string[]
  kind?: typeof DirectorySettlementKind.Finalized | typeof DirectorySettlementKind.IsolatedFailure
  tokenSeed?: number
}> = {}): PersistentDirectorySettlementEvidence {
  const kind = input.kind ?? DirectorySettlementKind.Finalized
  const admission = Object.freeze({
    schemaVersion: DIRECTORY_ADMISSION_SCHEMA_VERSION,
    receiveIntentDigest: RECEIVE_INTENT_DIGEST,
    layoutVersion: DIRECTORY_ADMISSION_LAYOUT_VERSION,
    layout: 'directory-tree-result-root' as const,
    token: identity(input.tokenSeed ?? 40, 32),
    directoryId: input.directoryId ?? EXPECTED_DIRECTORY_ID,
    generation: input.generation ?? EXPECTED_GENERATION,
    path: snapshotMaterializationPath(input.admissionPath ?? input.path ?? []),
  })
  return Object.freeze({
    artifactPath: snapshotMaterializationPath(input.path ?? []),
    settlement: kind === DirectorySettlementKind.Finalized
      ? Object.freeze({ kind, admission })
      : Object.freeze({
          kind,
          admission,
          fault: outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
        }),
  })
}

function identity(seed: number, width = 16): string {
  const bytes = new Uint8Array(width)
  bytes.fill(seed)
  return encodeBase64Url(bytes)
}
