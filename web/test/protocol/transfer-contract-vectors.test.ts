import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import { sha256 } from '../../src/crypto/digest'
import {
  canonicalDirectoryAdmissionBytes,
  createDirectoryAdmission,
  deriveDirectoryAdmissionToken,
  type DirectoryAdmission,
  type OutputDirectoryAdmission,
  type OutputModifiedTime,
} from '../../src/transfer/output-session'
import {
  createTransferIntentDraft,
  freezeTransferIntent,
  type TransferSelectionRules,
} from '../../src/transfer/intent'
import {
  FILE_CHECKPOINT_NAMESPACE,
  FILE_CHECKPOINT_OWNERSHIP_MARKER,
  canonicalFileCheckpointBytes,
  decodeFileCheckpointV1,
  decodeFileCheckpointOwnership,
  encodeFileCheckpointOwnership,
  encodeFileCheckpointV1,
  newFileCheckpointV1,
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
        // The fixed-width run identity remains excluded from canonical bytes and digest.
        transferJobId: urlIdentity(requiredString(vector.transferJobIdB64, 'transfer job ID')),
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
      expect(intent.transferJobId).toBe(urlIdentity(requiredString(vector.transferJobIdB64, 'transfer job ID')))
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
      const secret = b64ToBytes(requiredString(vector.secretB64, 'admission secret'))
      const canonical = canonicalDirectoryAdmissionBytes(secret, input)
      expect(bytesEqual(canonical, b64ToBytes(requiredString(vector.preimageB64, 'admission preimage')))).toBe(true)
      const token = await deriveDirectoryAdmissionToken(secret, input)
      expect(token).toBe(urlIdentity(requiredString(vector.tokenB64, 'admission token')))
      const admission = await createDirectoryAdmission(input, secret)
      expect(admission.token).toBe(token)
      expect(admission.parentToken).toBe(
        vector.parentTokenB64 === null ? undefined : urlIdentity(requiredString(vector.parentTokenB64, 'parent token')),
      )
    })
  }
})

describe('Go↔TypeScript FileCheckpointV1 vectors', () => {
  it('replays the candidate payload and storage envelope', () => {
    const vector = checkpointVectors.cases.find((candidate) => candidate.name === 'candidate') as VectorCase
    const record = newFileCheckpointV1({
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
        start: BigInt(range.start), end: BigInt(range.end),
      })),
      phase: Number(vector.phase),
      commitState: Number(vector.commitState),
    })
    expect(record.recordId).toBe(urlIdentity(requiredString(vector.recordIdB64, 'record ID')))
    expect(record.checksum).toBe(urlIdentity(requiredString(vector.checksumB64, 'checksum')))
    expect(bytesEqual(canonicalFileCheckpointBytes(record), b64ToBytes(requiredString(vector.canonicalBytesB64, 'checkpoint canonical bytes')))).toBe(true)
    const encoded = encodeFileCheckpointV1(record)
    expect(bytesEqual(encoded, b64ToBytes(requiredString(vector.encodedB64, 'checkpoint envelope')))).toBe(true)
    expect(decodeFileCheckpointV1(encoded)).toEqual(record)
  })

  it('replays the ownership marker envelope', () => {
    const vector = checkpointVectors.cases.find((candidate) => candidate.name === 'ownership') as VectorCase
    expect(requiredString(vector.marker, 'ownership marker')).toBe(FILE_CHECKPOINT_OWNERSHIP_MARKER)
    expect(requiredString(vector.namespace, 'ownership namespace')).toBe(FILE_CHECKPOINT_NAMESPACE)
    const ownership = {
      marker: FILE_CHECKPOINT_OWNERSHIP_MARKER,
      namespace: FILE_CHECKPOINT_NAMESPACE,
      backend: requiredString(vector.backend, 'ownership backend'),
      rootIdentity: urlIdentity(requiredString(vector.rootIdentityB64, 'ownership root')),
    } as const
    expect(bytesEqual(encodeFileCheckpointOwnership(ownership), b64ToBytes(requiredString(vector.encodedB64, 'ownership envelope')))).toBe(true)
    expect(decodeFileCheckpointOwnership(encodeFileCheckpointOwnership(ownership))).toEqual(ownership)
  })
})

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
  return createDirectoryAdmission(parentInput, b64ToBytes(requiredString(parentVector.secretB64, 'parent secret')))
}
