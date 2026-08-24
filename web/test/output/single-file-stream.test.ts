import { describe, expect, it, vi } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import { SingleFileStreamOutputSession } from '../../src/output/streams/single-file'
import type {
  OpenedOutputRevision,
  OutputFileRequest,
} from '../../src/transfer/output-session'
import {
  snapshotLogicalArtifactPath,
  snapshotMaterializationRootRelativePath,
  snapshotSourceAuthenticationPath,
} from '../../src/transfer/job/coordinate/direct-tree'

const ACTIVE_SIGNAL = new AbortController().signal
const SHARE_INSTANCE = identityText(1)
const FILE_ID = identityText(2)
const FILE_REVISION = identityText(3)

describe('single-file stream output session', () => {
  it('does not acquire output authority when revision authentication fails or retry it', async () => {
    const output = recordingStream()
    const failure = new Error('revision unavailable')
    const openRevision = vi.fn(async () => { throw failure })
    const session = new SingleFileStreamOutputSession('failed-open', output.stream)

    await expect(session.beginFile(request(3n, openRevision), ACTIVE_SIGNAL)).rejects.toBe(failure)

    expect(openRevision).toHaveBeenCalledOnce()
    expect(output.chunks).toHaveLength(0)
    expect(output.closed).toBe(false)
    expect(output.aborted).toBe(false)
    expect(output.stream.locked).toBe(false)

    await expect(session.beginFile(request(3n, openRevision), ACTIVE_SIGNAL)).rejects
      .toThrow(/exactly one authenticated revision open/u)
    expect(openRevision).toHaveBeenCalledOnce()
  })

  it.each([
    {
      mismatch: 'catalog identity',
      revision: openedRevision(3n, { fileId: identityText(4) }),
      message: /requested catalog file/u,
    },
    {
      mismatch: 'exact size',
      revision: openedRevision(4n),
      message: /requested output size/u,
    },
  ])('rejects a mismatched $mismatch before creating output', async ({ revision, message }) => {
    const output = recordingStream()
    const session = new SingleFileStreamOutputSession('mismatch', output.stream)

    await expect(session.beginFile(
      request(3n, async () => revision),
      ACTIVE_SIGNAL,
    )).rejects.toThrow(message)

    expect(output.chunks).toHaveLength(0)
    expect(output.closed).toBe(false)
    expect(output.aborted).toBe(false)
    expect(output.stream.locked).toBe(false)
  })

  it('binds the authenticated revision and transient range proof to the artifact path', async () => {
    const output = recordingStream()
    const session = new SingleFileStreamOutputSession('sequential', output.stream)
    const sourcePath = ['catalog', 'source.bin']
    const artifactPath = ['chosen', 'result.bin']
    const revision = openedRevision(3n)
    const input = request(3n, async () => revision, sourcePath, artifactPath)

    const opening = session.beginFile(input, ACTIVE_SIGNAL)
    sourcePath[0] = 'mutated-source'
    artifactPath[0] = 'mutated-artifact'
    const begun = await opening

    expect(begun.revision).toEqual(revision)
    expect(begun.durableRanges.source).toEqual({
      shareInstance: SHARE_INSTANCE,
      fileId: FILE_ID,
      fileRevision: FILE_REVISION,
    })
    expect(begun.durableRanges.ownership.canonicalPath).toEqual(['chosen', 'result.bin'])
    expect(begun.durableRanges.ownership.canonicalPath).not.toEqual(['catalog', 'source.bin'])
    expect(begun.durableRanges.ranges).toEqual([])

    await expect(begun.transaction.writeRange(1n, Uint8Array.of(9), ACTIVE_SIGNAL)).rejects
      .toThrow(/contiguous ascending ranges/u)
    expect(output.chunks).toHaveLength(0)

    const callerBytes = Uint8Array.of(1, 2)
    const firstWrite = begun.transaction.writeRange(0n, callerBytes, ACTIVE_SIGNAL)
    callerBytes.fill(9)
    await firstWrite
    await begun.transaction.writeRange(2n, Uint8Array.of(3), ACTIVE_SIGNAL)
    await expect(begun.transaction.checkpoint(ACTIVE_SIGNAL)).resolves.toMatchObject({ ranges: [] })
    await begun.transaction.commit(ACTIVE_SIGNAL)

    expect(output.bytes()).toEqual(Uint8Array.of(1, 2, 3))
    expect(output.closed).toBe(true)
    expect(output.aborted).toBe(false)
    expect(output.stream.locked).toBe(false)
  })

  it('commits an authenticated zero-byte revision through the ordinary transaction', async () => {
    const output = recordingStream()
    const session = new SingleFileStreamOutputSession('empty', output.stream)
    const begun = await session.beginFile(
      request(0n, async () => openedRevision(0n)),
      ACTIVE_SIGNAL,
    )

    await begun.transaction.commit(ACTIVE_SIGNAL)

    expect(output.chunks).toHaveLength(0)
    expect(output.closed).toBe(true)
    expect(output.aborted).toBe(false)
  })

  it('isolates retirement before any byte is attempted', async () => {
    const output = recordingStream()
    const session = new SingleFileStreamOutputSession('retire-empty', output.stream)
    const begun = await session.beginFile(
      request(2n, async () => openedRevision(2n)),
      ACTIVE_SIGNAL,
    )

    await expect(begun.transaction.retire(new Error('member failed'))).resolves.toBe('FileIsolated')

    expect(output.chunks).toHaveLength(0)
    expect(output.closed).toBe(false)
    expect(output.aborted).toBe(true)
    expect(output.stream.locked).toBe(false)
  })

  it('marks retirement after an emitted prefix as job-output compromise', async () => {
    const output = recordingStream()
    const session = new SingleFileStreamOutputSession('retire-prefix', output.stream)
    const begun = await session.beginFile(
      request(2n, async () => openedRevision(2n)),
      ACTIVE_SIGNAL,
    )
    await begun.transaction.writeRange(0n, Uint8Array.of(1), ACTIVE_SIGNAL)

    await expect(begun.transaction.retire(new Error('member failed'))).resolves
      .toBe('JobOutputCompromised')

    expect(output.bytes()).toEqual(Uint8Array.of(1))
    expect(output.closed).toBe(false)
    expect(output.aborted).toBe(true)
  })
})

function request(
  expectedSize: bigint,
  openRevision: OutputFileRequest['openRevision'],
  sourcePath: string[] = ['catalog', 'source.bin'],
  artifactPath: string[] = ['chosen', 'result.bin'],
): OutputFileRequest {
  return {
    source: { shareInstance: SHARE_INSTANCE, fileId: FILE_ID },
    sourceAuthenticationPath: snapshotSourceAuthenticationPath(sourcePath),
    logicalArtifactPath: snapshotLogicalArtifactPath(artifactPath),
    materializationRelativePath: snapshotMaterializationRootRelativePath(artifactPath),
    expectedSize,
    openRevision,
  }
}

function openedRevision(
  exactSize: bigint,
  override: Partial<OpenedOutputRevision> = {},
): OpenedOutputRevision {
  return Object.freeze({
    shareInstance: SHARE_INSTANCE,
    fileId: FILE_ID,
    fileRevision: FILE_REVISION,
    exactSize,
    ...override,
  })
}

interface RecordedStream {
  stream: WritableStream<Uint8Array>
  readonly chunks: Uint8Array[]
  closed: boolean
  aborted: boolean
  bytes(): Uint8Array
}

function recordingStream(): RecordedStream {
  const result: RecordedStream = {
    chunks: [],
    closed: false,
    aborted: false,
    stream: undefined as unknown as WritableStream<Uint8Array>,
    bytes: () => concatenate(result.chunks),
  }
  result.stream = new WritableStream<Uint8Array>({
    write: (chunk) => { result.chunks.push(chunk.slice()) },
    close: () => { result.closed = true },
    abort: () => { result.aborted = true },
  })
  return result
}

function concatenate(chunks: readonly Uint8Array[]): Uint8Array {
  const output = new Uint8Array(chunks.reduce((total, chunk) => total + chunk.byteLength, 0))
  let offset = 0
  for (const chunk of chunks) {
    output.set(chunk, offset)
    offset += chunk.byteLength
  }
  return output
}

function identityText(first: number): string {
  const identity = new Uint8Array(16)
  identity[0] = first
  return encodeBase64Url(identity)
}
