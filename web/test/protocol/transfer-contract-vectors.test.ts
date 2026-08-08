import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import { sha256 } from '../../src/crypto/digest'
import { FaultDomain, FaultScope, OutputFaultCode, outputFault } from '../../src/transfer/fault'
import {
  canonicalDirectoryAdmissionMessageV1,
  createDirectoryAdmission,
  DirectorySettlementKind,
  deriveDirectoryAdmissionToken,
  finalizedDirectorySettlement,
  isolatedDirectorySettlement,
  sameDirectoryAdmission,
  validateDirectorySettlement,
  verifyDirectoryAdmissionToken,
  type DirectoryAdmission,
  type DirectoryAdmissionScope,
  type OutputDirectoryAdmission,
  type OutputModifiedTime,
} from '../../src/transfer/output-session'
import {
  createTransferIntentDraft,
  freezeTransferIntent,
  snapshotTransferRunId,
  type TransferSelectionRules,
} from '../../src/transfer/intent'
import {
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  canonicalFileCheckpointBytes,
  checkpointIdentityEqual,
  decodeFileCheckpointV1,
  decodeFileCheckpointOwnership,
  encodeFileCheckpointOwnership,
  encodeFileCheckpointV1,
  newFileCheckpointV1,
  selectVerifiedCheckpoint,
  validateFileCheckpointTransition,
} from '../../src/output/persistence/checkpoint'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'

const intentVectors = loadVectorFile(new URL('../../../core/testvectors/transfer-intent-v1.json', import.meta.url))
const admissionVectors = loadVectorFile(new URL('../../../core/testvectors/directory-admission-v1.json', import.meta.url))
const checkpointVectors = loadVectorFile(new URL('../../../core/testvectors/file-checkpoint-v1.json', import.meta.url))

function urlIdentity(standardBase64: string): string {
  return encodeBase64Url(b64ToBytes(standardBase64))
}

function bytesEqual(left: Uint8Array, right: Uint8Array): boolean {
  return Buffer.from(left).equals(Buffer.from(right))
}

function requiredString(value: unknown, label: string): string {
  if (typeof value !== 'string') throw new Error(`${label} is not a string`)
  return value
}

function transferSelection(value: unknown): TransferSelectionRules {
  const selection = value as {
    mode: 'node-id' | 'catalog-path'
    defaultSelected: boolean
    rules?: readonly { kind: 'directory' | 'file'; idB64: string; selected: boolean }[]
    paths?: readonly string[]
    inputPaths?: readonly string[]
  }
  if (selection.mode === 'catalog-path') {
    if (selection.defaultSelected !== false) throw new Error('catalog-path vector default must be false')
    return {
      mode: 'catalog-path',
      defaultSelected: false,
      paths: selection.inputPaths ?? selection.paths ?? [],
    }
  }
  return {
    mode: 'node-id',
    defaultSelected: selection.defaultSelected,
    rules: (selection.rules ?? []).map((rule) => ({
      kind: rule.kind,
      id: urlIdentity(rule.idB64),
      selected: rule.selected,
    })),
  }
}

describe('Go↔TypeScript TransferIntentV1 vectors', () => {
  for (const rawCase of intentVectors.cases) {
    const vector = rawCase as VectorCase
    it(`replays ${requiredString(vector.name, 'intent case')}`, async () => {
      const output = vector.output as {
        targetKind: number
        targetIdentityB64: string
        backend: string
        format: 'directory' | 'single-file' | 'zip'
      }
      if (output.targetKind !== 2) throw new Error('browser vector output target must be opaque')
      const draft = createTransferIntentDraft({
        shareInstance: urlIdentity(requiredString(vector.shareInstanceB64, 'share instance')),
        syntheticRoot: urlIdentity(requiredString(vector.syntheticRootB64, 'synthetic root')),
        selection: transferSelection(vector.selection),
      })
      const intent = await freezeTransferIntent(draft, {
        target: urlIdentity(output.targetIdentityB64),
        targetKind: 2,
        backend: output.backend,
        format: output.format,
      })
      expect(bytesEqual(intent.canonicalBytes, b64ToBytes(requiredString(vector.canonicalBytesB64, 'canonical bytes')))).toBe(true)
      expect(intent.digest).toBe(urlIdentity(requiredString(vector.digestB64, 'intent digest')))
      expect(intent.digest).toBe(encodeBase64Url(await sha256(intent.canonicalBytes)))
      expect(snapshotTransferRunId(urlIdentity(requiredString(vector.transferJobIdB64, 'transfer job ID'))))
        .toBe(urlIdentity(requiredString(vector.transferJobIdB64, 'transfer job ID')))
    })
  }
})

describe('Go↔TypeScript DirectoryAdmissionV1 vectors', () => {
  for (const rawCase of admissionVectors.cases) {
    const vector = rawCase as VectorCase
    it(`replays ${requiredString(vector.name, 'admission case')}`, async () => {
      const parent = await replayParentAdmission(vector, admissionVectors.cases as readonly VectorCase[])
      const modified = outputModifiedTime(vector.modifiedTime)
      const input: OutputDirectoryAdmission = {
        directoryId: urlIdentity(requiredString(vector.directoryIdB64, 'directory ID')),
        generation: urlIdentity(requiredString(vector.generationB64, 'generation')),
        path: requiredString(vector.path, 'path') === '' ? [] : requiredString(vector.path, 'path').split('/'),
        ...(parent === undefined ? {} : { parentAdmission: parent }),
        ...(modified === undefined ? {} : { modifiedTime: modified }),
      }
      const scope = admissionScope(vector)
      const secret = b64ToBytes(requiredString(vector.secretB64, 'admission secret'))
      const canonical = canonicalDirectoryAdmissionMessageV1(scope, input)
      expect(bytesEqual(canonical, b64ToBytes(requiredString(vector.messageB64, 'admission message')))).toBe(true)
      const token = await deriveDirectoryAdmissionToken(secret, scope, input)
      expect(token).toBe(urlIdentity(requiredString(vector.tokenB64, 'admission token')))
      expect(await verifyDirectoryAdmissionToken(secret, scope, input, token)).toBe(true)
      const admission = await createDirectoryAdmission(secret, scope, input)
      expect(admission.schemaVersion).toBe(Number(vector.schemaVersion))
      expect(admission.transferIntentDigest).toBe(scope.transferIntentDigest)
      expect(admission.token).toBe(token)
      expect(admission.parentToken).toBe(
        vector.parentTokenB64 === null ? undefined : urlIdentity(requiredString(vector.parentTokenB64, 'parent token')),
      )
      replayDirectorySettlement(vector, admission)
    })
  }
})

describe('Go↔TypeScript FileCheckpointV1 vectors', () => {
  it('replays canonical bindings and storage envelopes', () => {
    const names = ['candidate', 'verified', 'paused', 'next-candidate', 'next-verified', 'foreign-root'] as const
    const records = new Map(names.map((name) => {
      const vector = checkpointVectors.cases.find((candidate) => candidate.name === name) as VectorCase
      const record = checkpointRecord(vector)
      expect(record.schemaVersion).toBe(Number(vector.schemaVersion))
      expect(record.recordId).toBe(urlIdentity(requiredString(vector.recordIdB64, 'record ID')))
      expect(record.checksum).toBe(urlIdentity(requiredString(vector.checksumB64, 'checksum')))
      expect(bytesEqual(
        canonicalFileCheckpointBytes(record),
        b64ToBytes(requiredString(vector.canonicalBytesB64, 'checkpoint canonical bytes')),
      )).toBe(true)
      const encoded = encodeFileCheckpointV1(record)
      expect(bytesEqual(encoded, b64ToBytes(requiredString(vector.encodedB64, 'checkpoint envelope')))).toBe(true)
      expect(decodeFileCheckpointV1(encoded)).toEqual(record)
      return [name, record] as const
    }))

    const candidate = records.get('candidate')!
    const verified = records.get('verified')!
    const paused = records.get('paused')!
    const nextCandidate = records.get('next-candidate')!
    const nextVerified = records.get('next-verified')!
    const foreignRoot = records.get('foreign-root')!
    expect(() => validateFileCheckpointTransition(candidate, verified)).not.toThrow()
    expect(() => validateFileCheckpointTransition(verified, paused)).not.toThrow()
    expect(() => validateFileCheckpointTransition(verified, nextCandidate)).not.toThrow()
    expect(() => validateFileCheckpointTransition(nextCandidate, nextVerified)).not.toThrow()
    expect(checkpointIdentityEqual(candidate, foreignRoot)).toBe(false)

    const crashCuts = checkpointVectors.cases.find((value) => value.name === 'crash-cuts') as VectorCase
    const beforeCommit = selectVerifiedCheckpoint(candidate, verified, nextCandidate)
    expect(beforeCommit.recordId).toBe(urlIdentity(requiredString(crashCuts.beforeCommitRecordIdB64, 'before-commit record')))
    expect(beforeCommit.checkpointGeneration).toBe(BigInt(requiredString(
      crashCuts.beforeCommitCheckpointGeneration,
      'before-commit generation',
    )))
    const afterCommit = selectVerifiedCheckpoint(candidate, verified, nextCandidate, nextVerified)
    expect(afterCommit.recordId).toBe(urlIdentity(requiredString(crashCuts.afterCommitRecordIdB64, 'after-commit record')))
    expect(afterCommit.checkpointGeneration).toBe(BigInt(requiredString(
      crashCuts.afterCommitCheckpointGeneration,
      'after-commit generation',
    )))
  })

  it('requires certified ownership and root-open disposition in the envelope', () => {
    const ownershipVector = checkpointVectors.cases.find((candidate) => candidate.name === 'ownership') as VectorCase
    const mismatchVector = checkpointVectors.cases.find((candidate) => candidate.name === 'ownership-mismatch') as VectorCase
    for (const vector of [ownershipVector, mismatchVector]) {
      expect(requiredString(vector.marker, 'ownership marker')).toBe(FILE_CHECKPOINT_OWNERSHIP_MARKER)
      expect(requiredString(vector.namespace, 'ownership namespace')).toBe(FILE_CHECKPOINT_NAMESPACE)
      expect(requiredString(vector.certification, 'ownership certification')).toBe('windows/ntfs/process-restart/v1')
      expect(['caller-provided-container', 'authority-created-root']).toContain(
        requiredString(vector.rootOpenDisposition, 'root-open disposition'),
      )
      const encoded = b64ToBytes(requiredString(vector.encodedB64, 'ownership envelope'))
      const decoded = decodeFileCheckpointOwnership(encoded)
      expect(decoded).toEqual({
        marker: FILE_CHECKPOINT_OWNERSHIP_MARKER,
        namespace: FILE_CHECKPOINT_NAMESPACE,
        backend: requiredString(vector.backend, 'ownership backend'),
        certification: requiredString(vector.certification, 'ownership certification'),
        rootIdentity: urlIdentity(requiredString(vector.rootIdentityB64, 'ownership root')),
        rootOpenDisposition: requiredString(vector.rootOpenDisposition, 'root-open disposition'),
      })
      expect(bytesEqual(encodeFileCheckpointOwnership(decoded), encoded)).toBe(true)
    }
    expect(requiredString(ownershipVector.rootOpenDisposition, 'root-open disposition')).not.toBe(
      requiredString(mismatchVector.rootOpenDisposition, 'mismatched root-open disposition'),
    )
    expect(requiredString(ownershipVector.canonicalBytesB64, 'ownership canonical bytes')).not.toBe(
      requiredString(mismatchVector.canonicalBytesB64, 'mismatched ownership canonical bytes'),
    )
  })
})

function replayDirectorySettlement(vector: VectorCase, admission: DirectoryAdmission): void {
  const expected = vector.settlement as {
    kind: unknown
    admissionTokenB64: unknown
    fault?: { domain?: unknown; scope?: unknown; code?: unknown }
  }
  const expectedToken = urlIdentity(requiredString(expected.admissionTokenB64, 'settlement admission token'))
  let settlement
  switch (expected.kind) {
    case DirectorySettlementKind.Finalized:
      if (expected.fault !== undefined) throw new Error('finalized settlement vector must not carry a fault')
      settlement = finalizedDirectorySettlement(admission)
      break
    case DirectorySettlementKind.IsolatedFailure:
      if (expected.fault?.domain !== FaultDomain.Output ||
          expected.fault.scope !== FaultScope.DirectoryLocal ||
          expected.fault.code !== OutputFaultCode.DirectoryMetadata) {
        throw new Error('isolated settlement vector must carry the closed directory metadata fault')
      }
      settlement = isolatedDirectorySettlement(
        admission,
        outputFault(FaultScope.DirectoryLocal, OutputFaultCode.DirectoryMetadata),
      )
      break
    default:
      throw new Error('directory settlement vector kind is invalid')
  }

  const validated = validateDirectorySettlement(admission, settlement)
  expect(validated.kind).toBe(expected.kind)
  expect(validated.admission.token).toBe(expectedToken)
  expect(sameDirectoryAdmission(validated.admission, admission)).toBe(true)
  if (validated.kind === DirectorySettlementKind.IsolatedFailure) {
    expect(validated.fault).toEqual(expected.fault)
  } else {
    expect(validated).not.toHaveProperty('fault')
  }
}

function checkpointRecord(vector: VectorCase): ReturnType<typeof newFileCheckpointV1> {
  return newFileCheckpointV1({
    ownershipMarker: requiredString(vector.ownershipMarker, 'ownership marker'),
    namespace: requiredString(vector.namespace, 'namespace'),
    transferIntentDigest: b64ToBytes(requiredString(vector.transferIntentDigestB64, 'intent digest')),
    fileId: b64ToBytes(requiredString(vector.fileIdB64, 'file ID')),
    fileRevision: b64ToBytes(requiredString(vector.fileRevisionB64, 'file revision')),
    canonicalPath: requiredString(vector.canonicalPath, 'checkpoint path'),
    exactSize: BigInt(requiredString(vector.exactSize, 'exact size')),
    backend: requiredString(vector.backend, 'checkpoint backend'),
    rootIdentity: b64ToBytes(requiredString(vector.rootIdentityB64, 'root identity')),
    ownedOutputObject: b64ToBytes(requiredString(vector.ownedOutputObjectB64, 'output object')),
    stateGeneration: BigInt(requiredString(vector.stateGeneration, 'state generation')),
    checkpointGeneration: BigInt(requiredString(vector.checkpointGeneration, 'checkpoint generation')),
    verifiedRanges: (vector.verifiedRanges as readonly { start: string; end: string }[]).map((range) => ({
      start: BigInt(range.start),
      end: BigInt(range.end),
    })),
    phase: Number(vector.phase),
    commitState: Number(vector.commitState),
    quarantineReason: Number(vector.quarantineReason),
    quarantineOrigin: Number(vector.quarantineOrigin),
    retirementReason: Number(vector.retirementReason),
  })
}

function outputModifiedTime(value: unknown): OutputModifiedTime | undefined {
  if (value === null || value === undefined) return undefined
  const modified = value as { seconds: number; nanoseconds: number; precision: 1 | 2 | 3 }
  const seconds = BigInt(modified.seconds)
  return {
    seconds,
    nanoseconds: modified.nanoseconds,
    precision: modified.precision,
    milliseconds: seconds * 1_000n + BigInt(Math.trunc(modified.nanoseconds / 1_000_000)),
  }
}

async function replayParentAdmission(
  vector: VectorCase,
  allCases: readonly VectorCase[],
): Promise<DirectoryAdmission | undefined> {
  if (vector.parentTokenB64 === null || vector.parentTokenB64 === undefined) return undefined
  const parentVector = allCases.find((candidate) => candidate.tokenB64 === vector.parentTokenB64)
  if (parentVector === undefined) throw new Error(`missing parent admission vector for ${String(vector.name)}`)
  const parentModified = outputModifiedTime(parentVector.modifiedTime)
  const parentParent = await replayParentAdmission(parentVector, allCases)
  const parentInput: OutputDirectoryAdmission = {
    directoryId: urlIdentity(requiredString(parentVector.directoryIdB64, 'parent directory ID')),
    generation: urlIdentity(requiredString(parentVector.generationB64, 'parent generation')),
    path: requiredString(parentVector.path, 'parent path') === '' ? [] : requiredString(parentVector.path, 'parent path').split('/'),
    ...(parentParent === undefined ? {} : { parentAdmission: parentParent }),
    ...(parentModified === undefined ? {} : { modifiedTime: parentModified }),
  }
  return createDirectoryAdmission(
    b64ToBytes(requiredString(parentVector.secretB64, 'parent secret')),
    admissionScope(parentVector),
    parentInput,
  )
}

function admissionScope(vector: VectorCase): DirectoryAdmissionScope {
  return {
    transferIntentDigest: urlIdentity(requiredString(vector.intentDigestB64, 'intent digest')),
    syntheticRoot: urlIdentity(requiredString(vector.syntheticRootB64, 'synthetic root')),
  }
}
