import { describe, expect, it } from 'vitest'
import { SelectionPolicy } from '../../src/catalog/selection'
import { REVISION_CODE_BUSY } from '../../src/content/flow'
import { RemoteRevisionError } from '../../src/content/session-services'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { createReceivedProtocolError } from '../../src/session/diagnostics'
import { createProtocolOperationIdentity, createProtocolSessionIdentity } from '../../src/session/identities'
import { TransferJob } from '../../src/transfer/job'
import { id, makeShareFixture, outputFile } from './share-fixture'

const REVISION_CODE_STALE = 0x3001
const REVISION_CODE_UNREADABLE = 0x3003
const SOURCE_FAILURE_SAMPLE_LIMIT = 20
const FILE_PAYLOAD = new Uint8Array([1, 2, 3, 4])
const selection = new SelectionPolicy(true).snapshot()

function remoteRevisionError(code: number): RemoteRevisionError {
  return new RemoteRevisionError(createReceivedProtocolError({
    requestKind: 'open_revisions',
    correlation: Object.freeze({
      protocolSessionId: createProtocolSessionIdentity(id(40)),
      protocolOperationId: createProtocolOperationIdentity(41n),
    }),
    content: {
      scope: 'revision',
      code,
      retryable: false,
    },
  }))
}

describe('TransferJob source revision failure summary', () => {
  it('counts both source-changed and source-unavailable failures when mixed', async () => {
    const changed = { seed: 10, name: 'changed.txt', bytes: FILE_PAYLOAD }
    const unavailable = { seed: 11, name: 'unreadable.txt', bytes: FILE_PAYLOAD }
    const changedId = encodeBase64Url(id(changed.seed))
    const unavailableId = encodeBase64Url(id(unavailable.seed))
    const fixture = makeShareFixture([changed, unavailable])

    const result = await new TransferJob({
      ...fixture,
      selection,
      revisions: {
        open: async fileId => {
          const key = encodeBase64Url(fileId)
          if (key === changedId) throw remoteRevisionError(REVISION_CODE_STALE)
          if (key === unavailableId) throw remoteRevisionError(REVISION_CODE_UNREADABLE)
          return fixture.revisions.open(fileId)
        },
      },
      output: {
        sessionId: 'sess-mixed-source-failures',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async () => outputFile({}),
      },
    }).run()

    expect(result.status).toBe('source-changed')
    expect(result.failedFiles).toBe(2n)
    expect(result.sourceRevisionFailures?.count).toBe(2n)
    expect(result.sourceRevisionFailures?.files.map(file => ({
      path: file.path.join('/'),
      reason: file.reason,
    }))).toEqual([
      { path: 'changed.txt', reason: 'source-changed' },
      { path: 'unreadable.txt', reason: 'source-unavailable' },
    ])
  })

  it('tracks total count across mixed failures beyond the display sample limit', async () => {
    const unavailableCount = 25
    const healthy = { seed: 10, name: 'ok.txt', bytes: FILE_PAYLOAD }
    const changed = { seed: 11, name: 'changed.txt', bytes: FILE_PAYLOAD }
    const unavailableFiles = Array.from({ length: unavailableCount }, (_, index) => ({
      seed: 12 + index,
      name: `unreadable-${index + 1}.txt`,
      bytes: FILE_PAYLOAD,
    }))
    const healthyId = encodeBase64Url(id(healthy.seed))
    const changedId = encodeBase64Url(id(changed.seed))
    const fixture = makeShareFixture([healthy, changed, ...unavailableFiles])

    const result = await new TransferJob({
      ...fixture,
      selection,
      revisions: {
        open: async fileId => {
          const key = encodeBase64Url(fileId)
          if (key === healthyId) return fixture.revisions.open(fileId)
          if (key === changedId) throw remoteRevisionError(REVISION_CODE_STALE)
          throw remoteRevisionError(REVISION_CODE_UNREADABLE)
        },
      },
      output: {
        sessionId: 'sess-mixed-capped-source-failures',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async () => outputFile({}),
      },
    }).run()

    const expectedFailures = BigInt(1 + unavailableCount)
    expect(result.status).toBe('partial')
    expect(result.completedFiles).toBe(1n)
    expect(result.failedFiles).toBe(expectedFailures)
    expect(result.sourceRevisionFailures?.count).toBe(expectedFailures)
    expect(result.sourceRevisionFailures?.files).toHaveLength(SOURCE_FAILURE_SAMPLE_LIMIT)
    expect(result.sourceRevisionFailures?.files[0]?.reason).toBe('source-changed')
    expect(result.contentWarning?.selectedFileCount).toBe(expectedFailures + 1n)
    expect(result.contentWarning?.completedFileCount).toBe(1n)
    expect(result.contentWarning?.missingFiles).toHaveLength(SOURCE_FAILURE_SAMPLE_LIMIT)
  })

  it('preserves failed status and exact total when source-unavailable exceeds the display sample limit', async () => {
    const totalUnavailable = 25
    const files = Array.from({ length: totalUnavailable }, (_, index) => ({
      seed: 10 + index,
      name: `locked-${index + 1}.txt`,
      bytes: FILE_PAYLOAD,
    }))
    const fixture = makeShareFixture(files)

    const result = await new TransferJob({
      ...fixture,
      selection,
      revisions: {
        open: async () => { throw remoteRevisionError(REVISION_CODE_UNREADABLE) },
      },
      output: {
        sessionId: 'sess-unavailable-capped',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async () => outputFile({}),
      },
    }).run()

    expect(result.status).toBe('failed')
    expect(result.error).toBeInstanceOf(RemoteRevisionError)
    expect(result.completedFiles).toBe(0n)
    expect(result.failedFiles).toBe(BigInt(totalUnavailable))
    expect(result.sourceRevisionFailures?.count).toBe(BigInt(totalUnavailable))
    expect(result.sourceRevisionFailures?.files).toHaveLength(SOURCE_FAILURE_SAMPLE_LIMIT)
    expect(result.sourceRevisionFailures?.files.every(file => file.reason === 'source-unavailable')).toBe(true)
    expect(result.contentWarning?.selectedFileCount).toBe(BigInt(totalUnavailable))
    expect(result.contentWarning?.completedFileCount).toBe(0n)
    expect(result.contentWarning?.missingFiles).toHaveLength(SOURCE_FAILURE_SAMPLE_LIMIT)
  })

  it('isolates source-busy files while completing healthy siblings', async () => {
    const healthy = { seed: 20, name: 'healthy.txt', bytes: FILE_PAYLOAD }
    const busy = { seed: 21, name: 'budget.xlsx', bytes: FILE_PAYLOAD }
    const busyId = encodeBase64Url(id(busy.seed))
    const fixture = makeShareFixture([healthy, busy])

    const result = await new TransferJob({
      ...fixture,
      selection,
      revisions: {
        open: async fileId => {
          if (encodeBase64Url(fileId) === busyId) throw remoteRevisionError(REVISION_CODE_BUSY)
          return fixture.revisions.open(fileId)
        },
      },
      output: {
        sessionId: 'sess-source-busy',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async () => outputFile({}),
      },
    }).run()

    expect(result.status).toBe('partial')
    expect(result.completedFiles).toBe(1n)
    expect(result.failedFiles).toBe(1n)
    expect(result.sourceRevisionFailures?.count).toBe(1n)
    expect(result.sourceRevisionFailures?.files).toEqual([
      expect.objectContaining({ path: ['budget.xlsx'], reason: 'source-busy' }),
    ])
    expect(result.contentWarning?.missingFiles).toEqual([
      { path: ['budget.xlsx'], reason: 'source-busy' },
    ])
  })

  it('settles all-busy failures as source-busy while keeping source-changed and mixed failures distinct', async () => {
    const busy = { seed: 30, name: 'budget.xlsx', bytes: FILE_PAYLOAD }
    const busyFixture = makeShareFixture([busy])
    const singleBusy = await new TransferJob({
      ...busyFixture,
      selection,
      revisions: { open: async () => { throw remoteRevisionError(REVISION_CODE_BUSY) } },
      output: {
        sessionId: 'sess-single-busy',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async () => outputFile({}),
      },
    }).run()

    expect(singleBusy.status).toBe('source-busy')
    expect(singleBusy.completedFiles).toBe(0n)
    expect(singleBusy.failedFiles).toBe(1n)
    expect(singleBusy.sourceRevisionFailures?.files).toEqual([
      expect.objectContaining({ path: ['budget.xlsx'], reason: 'source-busy' }),
    ])
    expect(singleBusy.contentWarning?.missingFiles).toEqual([
      { path: ['budget.xlsx'], reason: 'source-busy' },
    ])

    const changed = { seed: 31, name: 'changed.txt', bytes: FILE_PAYLOAD }
    const changedId = encodeBase64Url(id(changed.seed))
    const changedMixed = await new TransferJob({
      ...makeShareFixture([busy, changed]),
      selection,
      revisions: {
        open: async fileId => {
          throw remoteRevisionError(encodeBase64Url(fileId) === changedId ? REVISION_CODE_STALE : REVISION_CODE_BUSY)
        },
      },
      output: {
        sessionId: 'sess-changed-busy-mixed',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async () => outputFile({}),
      },
    }).run()
    expect(changedMixed.status).toBe('source-changed')

    const unreadable = { seed: 32, name: 'locked.txt', bytes: FILE_PAYLOAD }
    const unreadableId = encodeBase64Url(id(unreadable.seed))
    const unavailableMixed = await new TransferJob({
      ...makeShareFixture([busy, unreadable]),
      selection,
      revisions: {
        open: async fileId => {
          throw remoteRevisionError(encodeBase64Url(fileId) === unreadableId ? REVISION_CODE_UNREADABLE : REVISION_CODE_BUSY)
        },
      },
      output: {
        sessionId: 'sess-unavailable-busy-mixed',
        capabilities: { maxConcurrentFiles: 1, resumable: true, partialOutput: true },
        openFile: async () => outputFile({}),
      },
    }).run()
    expect(unavailableMixed.status).toBe('failed')
    expect(unavailableMixed.contentWarning?.missingFiles).toEqual([
      { path: ['budget.xlsx'], reason: 'source-busy' },
      { path: ['locked.txt'], reason: 'source-unavailable' },
    ])
  })
})
