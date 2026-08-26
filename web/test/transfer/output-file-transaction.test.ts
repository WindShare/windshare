import { describe, expect, it, vi } from 'vitest'
import {
  bindOutputFileTransaction,
} from '../../src/transfer/output-file-transaction'
import {
  VerifiedDurableRanges,
  VerifiedFinalOutputFile,
  type BeginOutputFileResult,
  type OutputFile,
  type OutputFileOwnership,
  type OutputSession,
} from '../../src/transfer/output-session'
import { identityText } from './v2-job-fixture'
import {
  snapshotLogicalArtifactPath,
  snapshotMaterializationRootRelativePath,
  snapshotSourceAuthenticationPath,
} from '../../src/transfer/job/coordinate/direct-tree'

const signal = new AbortController().signal
const file: OutputFile = Object.freeze({
  source: {
    shareInstance: identityText(1),
    fileId: identityText(4),
    fileRevision: identityText(5),
  },
  sourceAuthenticationPath: snapshotSourceAuthenticationPath(['source.bin']),
  logicalArtifactPath: snapshotLogicalArtifactPath(['logical-root', 'result.bin']),
  materializationRelativePath: snapshotMaterializationRootRelativePath(['result.bin']),
  exactSize: 4n,
})
const ownership: OutputFileOwnership = Object.freeze({
  backend: 'test',
  outputSessionId: 'session',
  canonicalPath: file.materializationRelativePath,
  ownedFileIdentity: 'owned-file',
})

describe('bound output file transaction', () => {
  it('rejects a transaction returned for another authenticated revision', () => {
    const begun = result({
      ...file.source,
      fileRevision: identityText(6),
      exactSize: file.exactSize,
    })
    expect(() => bindOutputFileTransaction(begun, file, session('None', false)))
      .toThrow(/different authenticated source revision/u)
  })

  it.each([
    { label: 'transient sequential', durability: 'None' as const, positioned: false },
    { label: 'durable sequential-prefix', durability: 'ProcessRestart' as const, positioned: false },
    { label: 'transient positioned', durability: 'None' as const, positioned: true },
    { label: 'durable positioned', durability: 'ProcessRestart' as const, positioned: true },
  ])('commits complete pending coverage for $label without a prior checkpoint', async ({
    durability,
    positioned,
  }) => {
    const commit = vi.fn(async () => finalProof())
    const bound = bindOutputFileTransaction(
      result({ ...file.source, exactSize: file.exactSize }, { commit }),
      file,
      session(durability, positioned),
    ).transaction
    if (positioned) {
      await bound.writeRange(2n, new Uint8Array([3, 4]), signal)
      await bound.writeRange(0n, new Uint8Array([1, 2]), signal)
    } else {
      await bound.writeRange(0n, new Uint8Array([1, 2]), signal)
      await bound.writeRange(2n, new Uint8Array([3, 4]), signal)
    }

    await expect(bound.commit(signal)).resolves.toMatchObject({ fileSize: 4n })
    expect(commit).toHaveBeenCalledOnce()
  })

  it('accepts a durable sequential prefix and commits the pending suffix', async () => {
    const initial = durableRanges([{ start: 0n, end: 2n }])
    const commit = vi.fn(async () => finalProof())
    const bound = bindOutputFileTransaction(
      result({ ...file.source, exactSize: file.exactSize }, { commit }, initial),
      file,
      session('ProcessRestart', false),
    ).transaction

    await expect(bound.writeRange(0n, new Uint8Array([9]), signal)).rejects.toThrow(/overlaps/u)
    await bound.writeRange(2n, new Uint8Array([3, 4]), signal)
    await expect(bound.commit(signal)).resolves.toBeInstanceOf(VerifiedFinalOutputFile)
  })

  it('rejects incomplete accepted coverage without making commit terminal', async () => {
    const commit = vi.fn(async () => finalProof())
    const bound = bindOutputFileTransaction(
      result({ ...file.source, exactSize: file.exactSize }, { commit }),
      file,
      session('None', false),
    ).transaction
    await bound.writeRange(0n, new Uint8Array([1, 2]), signal)
    await expect(bound.commit(signal)).rejects.toThrow(/complete durable and pending coverage/u)
    await bound.writeRange(2n, new Uint8Array([3, 4]), signal)
    await bound.commit(signal)
    expect(commit).toHaveBeenCalledOnce()
  })

  it('preserves pending state on a retryable deferral and advances only with the exact durable union', async () => {
    let decision: 'deferred' | 'advanced' = 'deferred'
    const automaticCheckpoint = vi.fn(async () => decision === 'deferred'
      ? Object.freeze({
          kind: 'deferred' as const,
          reason: 'capacity-unavailable' as const,
        })
      : Object.freeze({
          kind: 'advanced' as const,
          durable: durableRanges([{ start: 0n, end: 2n }]),
        }))
    const bound = bindOutputFileTransaction(
      result({ ...file.source, exactSize: file.exactSize }, { automaticCheckpoint }),
      file,
      session('ProcessRestart', true),
    ).transaction
    await bound.writeRange(0n, new Uint8Array([1, 2]), signal)

    await expect(bound.automaticCheckpoint('pending-bytes', signal))
      .resolves.toMatchObject({ kind: 'deferred' })
    await expect(bound.writeRange(1n, new Uint8Array([9]), signal)).rejects.toThrow(/overlaps/u)
    decision = 'advanced'
    await expect(bound.automaticCheckpoint('pending-time', signal))
      .resolves.toMatchObject({ kind: 'advanced', durable: { ranges: [{ start: 0n, end: 2n }] } })
  })

  it('makes a terminal automatic outcome sticky without preventing final commit', async () => {
    const automaticCheckpoint = vi.fn(async () => Object.freeze({
      kind: 'finished' as const,
      reason: 'cumulative-write-amplification-budget' as const,
    }))
    const bound = bindOutputFileTransaction(
      result({ ...file.source, exactSize: file.exactSize }, { automaticCheckpoint }),
      file,
      session('ProcessRestart', true),
    ).transaction
    await bound.writeRange(0n, new Uint8Array([1, 2]), signal)

    await expect(bound.automaticCheckpoint('pending-bytes', signal))
      .resolves.toMatchObject({ kind: 'finished' })
    await expect(bound.automaticCheckpoint('pending-time', signal))
      .rejects.toThrow(/already finished/u)
    await bound.writeRange(2n, new Uint8Array([3, 4]), signal)
    await expect(bound.commit(signal)).resolves.toBeInstanceOf(VerifiedFinalOutputFile)
    expect(automaticCheckpoint).toHaveBeenCalledOnce()
  })

  it('rejects partial automatic checkpoint evidence', async () => {
    const durable = durableRanges([{ start: 0n, end: 1n }])
    const bound = bindOutputFileTransaction(result(
      { ...file.source, exactSize: file.exactSize },
      { automaticCheckpoint: async () => Object.freeze({ kind: 'advanced' as const, durable }) },
    ), file, session('ProcessRestart', true)).transaction
    await bound.writeRange(0n, new Uint8Array([1, 2]), signal)
    await expect(bound.automaticCheckpoint('pending-bytes', signal))
      .rejects.toThrow(/every accepted pending write/u)
  })

  it('waits for and validates an exact forced durable pause cut', async () => {
    let resolvePause!: (value: VerifiedDurableRanges) => void
    const pauseEvidence = new Promise<VerifiedDurableRanges>(resolve => { resolvePause = resolve })
    const pause = vi.fn(() => pauseEvidence)
    const bound = bindOutputFileTransaction(
      result({ ...file.source, exactSize: file.exactSize }, { pause }),
      file,
      session('ProcessRestart', false),
    ).transaction
    await bound.writeRange(0n, new Uint8Array([1, 2]), signal)
    const pendingPause = bound.pause('pause')
    let settled = false
    pendingPause.finally(() => { settled = true }).catch(() => undefined)
    await Promise.resolve()
    expect(settled).toBe(false)
    resolvePause(durableRanges([{ start: 0n, end: 2n }]))
    await expect(pendingPause).resolves.toMatchObject({ ranges: [{ start: 0n, end: 2n }] })
    await expect(bound.writeRange(2n, new Uint8Array([3, 4]), signal))
      .rejects.toThrow(/terminal settlement began/u)
  })

  it('rejects stale, partial, and foreign pause evidence', async () => {
    const cases = [
      durableRanges([]),
      durableRanges([{ start: 0n, end: 1n }]),
      durableRanges([{ start: 0n, end: 2n }], { ...ownership, ownedFileIdentity: 'foreign' }),
    ]
    for (const evidence of cases) {
      const bound = bindOutputFileTransaction(result(
        { ...file.source, exactSize: file.exactSize },
        { pause: async () => evidence },
      ), file, session('ProcessRestart', false)).transaction
      await bound.writeRange(0n, new Uint8Array([1, 2]), signal)
      await expect(bound.pause('pause')).rejects.toThrow()
    }
  })

  it.each(['commit', 'pause', 'retire'] as const)(
    'rejects every later mutation after %s settlement starts',
    async (terminal) => {
      const initial = durableRanges([])
      const bound = bindOutputFileTransaction(
        result({ ...file.source, exactSize: file.exactSize }, {}, initial),
        file,
        session('None', false),
      ).transaction
      if (terminal === 'commit') {
        await bound.writeRange(0n, new Uint8Array([1, 2, 3, 4]), signal)
        await bound.commit(signal)
      } else if (terminal === 'pause') {
        await bound.writeRange(0n, new Uint8Array([1]), signal)
        await bound.pause('pause')
      } else {
        await bound.retire('retire')
      }

      await expect(bound.writeRange(0n, new Uint8Array([1]), signal))
        .rejects.toThrow(/terminal settlement began/u)
      await expect(bound.automaticCheckpoint('pending-bytes', signal))
        .rejects.toThrow(/terminal settlement began/u)
      await expect(bound.pause('again')).rejects.toThrow(/terminal settlement began/u)
      await expect(bound.retire('again')).rejects.toThrow(/terminal settlement began/u)
    },
  )

  it.each([
    { ownership: { ...ownership, backend: 'foreign' }, source: file.source, fileSize: 4n },
    { ownership: { ...ownership, outputSessionId: 'foreign' }, source: file.source, fileSize: 4n },
    { ownership: { ...ownership, canonicalPath: ['foreign.bin'] }, source: file.source, fileSize: 4n },
    { ownership: { ...ownership, ownedFileIdentity: 'foreign' }, source: file.source, fileSize: 4n },
    { ownership, source: { ...file.source, fileRevision: identityText(6) }, fileSize: 4n },
    { ownership, source: file.source, fileSize: 3n },
  ])('rejects a foreign final proof %#', async (foreign) => {
    const commit = async () => new VerifiedFinalOutputFile(
      foreign.ownership,
      foreign.source,
      foreign.fileSize,
    )
    const bound = bindOutputFileTransaction(
      result({ ...file.source, exactSize: file.exactSize }, { commit }),
      file,
      session('None', false),
    ).transaction
    await bound.writeRange(0n, new Uint8Array([1, 2, 3, 4]), signal)
    await expect(bound.commit(signal)).rejects.toThrow(/different output or source revision/u)
  })

  it('never exposes pending ranges as initial recovery truth', () => {
    expect(() => bindOutputFileTransaction(
      result(
        { ...file.source, exactSize: file.exactSize },
        {},
        durableRanges([{ start: 0n, end: 1n }]),
      ),
      file,
      session('None', true),
    )).toThrow(/transient output cannot claim durable/u)
    expect(() => bindOutputFileTransaction(
      result(
        { ...file.source, exactSize: file.exactSize },
        {},
        durableRanges([{ start: 1n, end: 2n }]),
      ),
      file,
      session('ProcessRestart', false),
    )).toThrow(/canonical recovery prefix/u)
  })
})

function session(
  durability: 'None' | 'ProcessRestart',
  randomWrite: boolean,
): Pick<OutputSession, 'identity' | 'capabilities'> {
  return {
    identity: { backend: 'test', outputSessionId: 'session' },
    capabilities: {
      durability,
      randomWrite,
      fileFailureIsolation: true,
      modificationTime: false,
    },
  }
}

function finalProof(
  proofOwnership: OutputFileOwnership = ownership,
): VerifiedFinalOutputFile {
  return new VerifiedFinalOutputFile(proofOwnership, file.source, file.exactSize)
}

function durableRanges(
  ranges: readonly Readonly<{ start: bigint; end: bigint }>[],
  rangeOwnership: OutputFileOwnership = ownership,
): VerifiedDurableRanges {
  return new VerifiedDurableRanges(rangeOwnership, file.source, file.exactSize, ranges)
}

function result(
  revision: BeginOutputFileResult['revision'],
  overrides: Partial<BeginOutputFileResult['transaction']> = {},
  durable = durableRanges([]),
): BeginOutputFileResult {
  return Object.freeze({
    revision,
    durableRanges: durable,
    transaction: {
      writeRange: async () => undefined,
      automaticCheckpoint: async () => Object.freeze({
        kind: 'finished' as const,
        reason: 'cost-evidence-unavailable' as const,
      }),
      commit: async () => finalProof(),
      retire: async () => 'FileIsolated' as const,
      pause: async () => durable,
      ...overrides,
    },
  })
}
