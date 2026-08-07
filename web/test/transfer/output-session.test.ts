import { describe, expect, it, vi } from 'vitest'

import { byteRange } from '../../src/content/geometry'
import { V2_CATALOG_NAME_BYTES } from '../../src/catalog/path-policy'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { DirectoryAdmissionLedger } from '../../src/transfer/directory-admission-ledger'
import {
  createDirectoryAdmission,
  DirectoryAdmissionBindingError,
  OutputBudgetExceededError,
  VerifiedDurableRanges,
  MAXIMUM_OUTPUT_PATH_SEGMENTS,
  MAXIMUM_OUTPUT_SEGMENT_BYTES,
  outputCapabilities,
  outputSessionIdentity,
  snapshotOutputDirectory,
  snapshotOutputFile,
  validateOutputSessionBinding,
  validateDirectoryAdmissionBinding,
  type DirectoryAdmission,
  type OutputDirectory,
  type OutputDirectoryAdmission,
  type OutputModifiedTime,
  type OutputFile,
  type OutputSession,
} from '../../src/transfer/output-session'
import type { TransferIntent } from '../../src/transfer/intent'

const source = Object.freeze({
  shareInstance: catalogIdentityText(10),
  fileId: catalogIdentityText(11),
  fileRevision: catalogIdentityText(12),
})

const ownership = Object.freeze({
  backend: 'fake',
  outputSessionId: 'session',
  canonicalPath: Object.freeze(['folder', 'file']),
  ownedFileIdentity: 'owned-file',
})

const TEST_ADMISSION_SECRET = Uint8Array.from({ length: 32 }, (_, index) => index + 1)
const ROOT_DIRECTORY_ID = catalogIdentityText(1)
const ROOT_GENERATION = catalogIdentityText(2)
const CHILD_DIRECTORY_ID = catalogIdentityText(3)
const CHILD_GENERATION = catalogIdentityText(4)

describe('OutputSession value contracts', () => {
  it('binds normalized durable ranges to one source revision and exact file size', () => {
    const durable = new VerifiedDurableRanges(ownership, source, 100n, [byteRange(0n, 30n)])
    expect(durable.ownership).toEqual(ownership)
    expect(Object.isFrozen(durable.ownership.canonicalPath)).toBe(true)
    expect(durable.source).toEqual(source)
    expect(durable.ranges).toEqual([{ start: 0n, end: 30n }])
    expect(durable.covers(byteRange(5n, 25n))).toBe(true)
    expect(durable.covers(byteRange(5n, 31n))).toBe(false)
    expect(() => new VerifiedDurableRanges(
      ownership,
      source,
      10n,
      [byteRange(0n, 11n)],
    ))
      .toThrow(/within the file/u)
    for (const malformed of [
      [byteRange(5n, 7n), byteRange(1n, 3n)],
      [byteRange(0n, 5n), byteRange(4n, 7n)],
      [byteRange(0n, 5n), byteRange(5n, 7n)],
      [byteRange(0n, 0n)],
    ]) {
      expect(() => new VerifiedDurableRanges(ownership, source, 10n, malformed))
        .toThrow(/sorted, non-overlapping, non-adjacent/u)
    }
  })

  it('snapshots capabilities, source identity, and output paths', () => {
    const capabilities = outputCapabilities({
      durability: 'ProcessRestart',
      randomWrite: true,
      fileFailureIsolation: true,
      modificationTime: false,
    })
    expect(Object.isFrozen(capabilities)).toBe(true)
    expect(outputSessionIdentity({ backend: 'fsa', outputSessionId: 'job' })).toEqual({
      backend: 'fsa',
      outputSessionId: 'job',
    })

    const path = ['folder', 'file.bin']
    const file = snapshotOutputFile({
      source: {
        shareInstance: source.shareInstance,
        fileId: source.fileId,
        fileRevision: source.fileRevision,
      },
      path,
      exactSize: 3n,
    })
    path[0] = 'changed'
    expect(file.path).toEqual(['folder', 'file.bin'])
    expect(Object.isFrozen(file.path)).toBe(true)
    expect(Object.isFrozen(file.source)).toBe(true)
  })

  it('rejects structurally unsafe or semantically incomplete output identities', () => {
    expect(MAXIMUM_OUTPUT_SEGMENT_BYTES).toBe(V2_CATALOG_NAME_BYTES)
    expect(() => snapshotOutputDirectory(outputDirectory(
      ['a'.repeat(V2_CATALOG_NAME_BYTES)],
    ))).not.toThrow()
    expect(() => outputSessionIdentity({ backend: '', outputSessionId: 'job' }))
      .toThrow(/backend/u)
    expect(() => outputSessionIdentity({ backend: 'fsa', outputSessionId: 'x'.repeat(129) }))
      .toThrow(/at most 128 bytes/u)
    expect(() => snapshotOutputDirectory(outputDirectory([]))).toThrow(/synthetic root/u)
    expect(() => snapshotOutputDirectory(outputDirectory(['..']))).toThrow(/frozen path policy/u)
    expect(() => snapshotOutputDirectory(outputDirectory(['bad/name']))).toThrow(/frozen path policy/u)
    expect(() => snapshotOutputDirectory(outputDirectory([
      'a'.repeat(MAXIMUM_OUTPUT_SEGMENT_BYTES + 1),
    ]))).toThrow(/frozen path policy/u)
    expect(() => snapshotOutputDirectory(outputDirectory(
      Array.from({ length: MAXIMUM_OUTPUT_PATH_SEGMENTS + 1 }, () => 'a'),
    ))).toThrow(/segment limit/u)
    expect(() => snapshotOutputDirectory(outputDirectory(
      Array.from({ length: 129 }, () => 'a'.repeat(MAXIMUM_OUTPUT_SEGMENT_BYTES)),
    ))).toThrow(/frozen path policy/u)
    expect(() => snapshotOutputDirectory(outputDirectory(['\ud800']))).toThrow(/frozen path policy/u)
    expect(() => snapshotOutputDirectory(outputDirectory(['e\u0301']))).toThrow(/frozen path policy/u)
    expect(() => snapshotOutputDirectory(outputDirectory(['CON']))).toThrow(/frozen path policy/u)
    expect(() => snapshotOutputDirectory(outputDirectory(['.wsresume-state']))).toThrow(/frozen path policy/u)
    expect(() => snapshotOutputFile({
      source: { shareInstance: '', fileId: source.fileId, fileRevision: source.fileRevision },
      path: ['file'],
      exactSize: 0n,
    })).toThrow(/shareInstance/u)
    expect(() => snapshotOutputFile({
      source: { ...source, fileId: 'file' },
      path: ['file'],
      exactSize: 0n,
    })).toThrow(/canonical non-zero 16-byte identity/u)
    expect(() => snapshotOutputFile({
      source,
      path: ['file'],
      exactSize: -1n,
    })).toThrow(/negative/u)
    expect(() => new VerifiedDurableRanges(
      { ...ownership, canonicalPath: ['different'], ownedFileIdentity: '' },
      source,
      0n,
      [],
    )).toThrow(/owned output file/u)
  })

  it('rejects impossible stream capability algebra while permitting restartable ZIP staging', () => {
    expect(() => validateOutputSessionBinding(
      outputIntent('single-file'),
      outputSession('single-file', 'ProcessRestart', true, true),
    )).toThrow(/capabilities contradict/u)
    expect(() => validateOutputSessionBinding(
      outputIntent('zip'),
      outputSession('zip', 'None', true, false),
    )).toThrow(/capabilities contradict/u)
    expect(() => validateOutputSessionBinding(
      outputIntent('zip'),
      outputSession('zip', 'ProcessRestart', true, true),
    )).not.toThrow()
  })
})

describe('OutputSession directory admission boundary', () => {
  const signal = new AbortController().signal

  it('enforces backend-owned admission count and metadata-byte budgets', async () => {
    const countBound = new DirectoryAdmissionLedger({
      secret: TEST_ADMISSION_SECRET,
      maximumAdmissions: 1,
    })
    const root = await countBound.admitDirectory(rootAdmissionRequest(), signal)
    expect(await countBound.admitDirectory(rootAdmissionRequest(), signal)).toBe(root)
    await expect(countBound.admitDirectory(childAdmissionRequest(root), signal))
      .rejects.toBeInstanceOf(OutputBudgetExceededError)

    const byteBound = new DirectoryAdmissionLedger({
      secret: TEST_ADMISSION_SECRET,
      maximumMetadataBytes: 1,
    })
    await expect(byteBound.admitDirectory(rootAdmissionRequest(), signal))
      .rejects.toBeInstanceOf(OutputBudgetExceededError)
  })

  it('coalesces exact duplicate admissions and rejects metadata rebinding', async () => {
    const ledger = new DirectoryAdmissionLedger({ secret: TEST_ADMISSION_SECRET })
    const materialize = vi.fn(async () => undefined)
    const root = await ledger.admitDirectory(rootAdmissionRequest(), signal)
    const request = childAdmissionRequest(root, 123n)

    const [first, duplicate] = await Promise.all([
      ledger.admitDirectory(request, signal, materialize),
      ledger.admitDirectory(request, signal, materialize),
    ])

    expect(duplicate).toBe(first)
    expect(first.modifiedTime?.milliseconds).toBe(123n)
    expect(materialize).toHaveBeenCalledTimes(1)
    await expect(ledger.admitDirectory({
      ...request,
      modifiedTime: modifiedTimeForMilliseconds(124n),
    }, signal)).rejects.toBeInstanceOf(DirectoryAdmissionBindingError)
    await expect(ledger.admitDirectory({
      ...request,
      modifiedTime: Object.freeze({
        seconds: 0n,
        nanoseconds: 123_456_789,
        precision: 3,
        milliseconds: 123n,
      }),
    }, signal)).rejects.toBeInstanceOf(DirectoryAdmissionBindingError)
    expect(materialize).toHaveBeenCalledTimes(1)
  })

  it('releases rejected pre-materialization reservations instead of exhausting admission budgets', async () => {
    const ledger = new DirectoryAdmissionLedger({
      secret: TEST_ADMISSION_SECRET,
      maximumAdmissions: 2,
    })
    const root = await ledger.admitDirectory(rootAdmissionRequest(), signal)
    const cancelled = new DOMException('cancel admission', 'AbortError')
    for (let index = 0; index < 3; index += 1) {
      await expect(ledger.admitDirectory({
        directoryId: catalogIdentityText(20 + index),
        generation: catalogIdentityText(30 + index),
        path: [`cancelled-${index}`],
        parentAdmission: root,
      }, signal, async () => { throw cancelled })).rejects.toBe(cancelled)
    }
    await expect(ledger.admitDirectory(childAdmissionRequest(root), signal)).resolves.toBeDefined()
  })

  it('commits proof when cancellation lands after successful materialization', async () => {
    const ledger = new DirectoryAdmissionLedger({ secret: TEST_ADMISSION_SECRET })
    const root = await ledger.admitDirectory(rootAdmissionRequest(), signal)
    const controller = new AbortController()
    const materialize = vi.fn(async () => { controller.abort(new DOMException('late cancel', 'AbortError')) })
    const request = childAdmissionRequest(root)

    const admitted = await ledger.admitDirectory(request, controller.signal, materialize)
    await expect(ledger.admitDirectory(request, signal, materialize)).resolves.toBe(admitted)
    expect(materialize).toHaveBeenCalledOnce()
  })

  it('rejects missing or forged ancestry before admitting a child or opening a file', async () => {
    const ledger = new DirectoryAdmissionLedger({ secret: TEST_ADMISSION_SECRET })
    const root = await ledger.admitDirectory(rootAdmissionRequest(), signal)

    await expect(ledger.admitDirectory({
      directoryId: CHILD_DIRECTORY_ID,
      generation: CHILD_GENERATION,
      path: ['child'],
    }, signal)).rejects.toThrow(/requires its parent admission/u)

    const forgedRoot = Object.freeze({ ...root, directoryId: catalogIdentityText(5) })
    await expect(ledger.admitDirectory(childAdmissionRequest(forgedRoot), signal))
      .rejects.toThrow(/not admitted by this output session/u)

    const child = await ledger.admitDirectory(childAdmissionRequest(root), signal)
    expect(() => ledger.validateFileParent(outputFile()))
      .toThrow(/missing, forged, or mismatched/u)
    expect(() => ledger.validateFileParent(outputFile({
      ...child,
      generation: catalogIdentityText(6),
    }))).toThrow(/missing, forged, or mismatched/u)

    expect(ledger.validateFileParent(outputFile(child)).parentAdmission).toEqual(child)
  })

  it('finalizes only the exact generation admitted by this session', async () => {
    const ledger = new DirectoryAdmissionLedger({ secret: TEST_ADMISSION_SECRET })
    const root = await ledger.admitDirectory(rootAdmissionRequest(), signal)
    const request = childAdmissionRequest(root)
    await ledger.admitDirectory(request, signal)

    expect(ledger.validateDirectoryFinalization({ ...request, parentAdmission: root }))
      .toMatchObject(request)
    expect(() => ledger.validateDirectoryFinalization({
      directoryId: catalogIdentityText(5),
      generation: catalogIdentityText(6),
      path: ['unadmitted'],
      parentAdmission: root,
    })).toThrow(/unadmitted or rebound/u)
    expect(() => ledger.validateDirectoryFinalization({
      ...request,
      parentAdmission: { ...root, generation: catalogIdentityText(7) },
    })).toThrow(/not admitted by this output session/u)
  })

  it('fails closed when a proof does not echo the requested generation metadata', async () => {
    const root = await createDirectoryAdmission(rootAdmissionRequest(), TEST_ADMISSION_SECRET)
    const request = childAdmissionRequest(root, 123n)
    const mismatched = await createDirectoryAdmission({
      ...request,
      modifiedTime: modifiedTimeForMilliseconds(999n),
    }, TEST_ADMISSION_SECRET)

    expect(() => validateDirectoryAdmissionBinding(request, mismatched))
      .toThrow(/different committed generation/u)
  })
})

function rootAdmissionRequest(): OutputDirectoryAdmission {
  return Object.freeze({
    directoryId: ROOT_DIRECTORY_ID,
    generation: ROOT_GENERATION,
    path: Object.freeze([]),
  })
}

function childAdmissionRequest(
  parentAdmission: DirectoryAdmission,
  modifiedTimeMilliseconds?: bigint,
): OutputDirectoryAdmission {
  const modifiedTime = modifiedTimeMilliseconds === undefined
    ? undefined
    : modifiedTimeForMilliseconds(modifiedTimeMilliseconds)
  return Object.freeze({
    directoryId: CHILD_DIRECTORY_ID,
    generation: CHILD_GENERATION,
    path: Object.freeze(['child']),
    parentAdmission,
    ...(modifiedTime === undefined
      ? {}
      : { modifiedTime }),
  })
}

function outputDirectory(path: readonly string[]): OutputDirectory {
  return Object.freeze({
    directoryId: CHILD_DIRECTORY_ID,
    generation: CHILD_GENERATION,
    path,
    parentAdmission: Object.freeze({
      token: admissionTokenText(1),
      directoryId: ROOT_DIRECTORY_ID,
      generation: ROOT_GENERATION,
      path: Object.freeze([]),
    }),
  })
}

function catalogIdentityText(first: number): string {
  const identity = new Uint8Array(16)
  identity[0] = first
  return encodeBase64Url(identity)
}

function admissionTokenText(first: number): string {
  const identity = new Uint8Array(32)
  identity[0] = first
  return encodeBase64Url(identity)
}

function modifiedTimeForMilliseconds(milliseconds: bigint): OutputModifiedTime {
  return Object.freeze({
    seconds: milliseconds / 1_000n,
    nanoseconds: Number(milliseconds % 1_000n) * 1_000_000,
    precision: 2,
    milliseconds,
  })
}

function outputFile(parentAdmission?: DirectoryAdmission): OutputFile {
  return Object.freeze({
    source,
    path: Object.freeze(['child', 'file.bin']),
    exactSize: 0n,
    ...(parentAdmission === undefined ? {} : { parentAdmission }),
  })
}

function outputIntent(format: TransferIntent['output']['format']): TransferIntent {
  return {
    output: { backend: 'fake', format, target: admissionTokenText(9), targetKind: 2 },
  } as TransferIntent
}

function outputSession(
  format: OutputSession['format'],
  durability: OutputSession['capabilities']['durability'],
  randomWrite: boolean,
  fileFailureIsolation: boolean,
): OutputSession {
  return {
    identity: { backend: 'fake', outputSessionId: 'session' },
    format,
    capabilities: { durability, randomWrite, fileFailureIsolation, modificationTime: false },
    admitDirectory: async () => { throw new Error('unused') },
    finalizeDirectory: async () => undefined,
    beginFile: async () => { throw new Error('unused') },
    finishJob: async () => undefined,
    abortJob: async () => undefined,
  }
}
