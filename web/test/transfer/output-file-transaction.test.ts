import { describe, expect, it, vi } from 'vitest'
import { byteRange } from '../../src/content/geometry'
import {
  OutputCheckpointContractError,
  bindOutputFileTransaction,
} from '../../src/transfer/output-file-transaction'
import {
  VerifiedDurableRanges,
  type BeginOutputFileResult,
  type OutputFile,
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
const ownership = Object.freeze({
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
    expect(() => bindOutputFileTransaction(begun, file, session('None')))
      .toThrow(/different authenticated source revision/)
  })

  it('requires transient writes to remain contiguous and complete before commit', async () => {
    const commit = vi.fn(async () => undefined)
    const begun = result({ ...file.source, exactSize: file.exactSize }, { commit })
    const bound = bindOutputFileTransaction(begun, file, session('None')).transaction
    await expect(bound.writeRange(1n, new Uint8Array([1]), signal))
      .rejects.toThrow(OutputCheckpointContractError)
    await bound.writeRange(0n, new Uint8Array([1, 2]), signal)
    await expect(bound.commit(signal)).rejects.toThrow(/whole-file durability/)
    await bound.writeRange(2n, new Uint8Array([3, 4]), signal)
    await bound.commit(signal)
    expect(commit).toHaveBeenCalledOnce()
  })

  it('accepts only the exact durable checkpoint union and prevents overlap', async () => {
    let checkpoint = new VerifiedDurableRanges(
      ownership,
      file.source,
      file.exactSize,
      [byteRange(0n, 2n)],
    )
    const begun = result({ ...file.source, exactSize: file.exactSize }, {
      checkpoint: async () => checkpoint,
    }, checkpoint)
    const bound = bindOutputFileTransaction(begun, file, session('ProcessRestart')).transaction
    await expect(bound.writeRange(1n, new Uint8Array([9]), signal))
      .rejects.toThrow(/overlaps bytes/)
    await bound.writeRange(2n, new Uint8Array([3, 4]), signal)
    await expect(bound.checkpoint(signal)).rejects.toThrow(/prior durability plus every completed write/)
    checkpoint = new VerifiedDurableRanges(
      ownership,
      file.source,
      file.exactSize,
      [byteRange(0n, 4n)],
    )
    await bound.checkpoint(signal)
    await bound.commit(signal)
  })
})

function session(durability: 'None' | 'ProcessRestart'): Pick<OutputSession, 'identity' | 'capabilities'> {
  return {
    identity: { backend: 'test', outputSessionId: 'session' },
    capabilities: {
      durability,
      randomWrite: durability !== 'None',
      fileFailureIsolation: true,
      modificationTime: false,
    },
  }
}

function result(
  revision: BeginOutputFileResult['revision'],
  overrides: Partial<BeginOutputFileResult['transaction']> = {},
  durableRanges = new VerifiedDurableRanges(ownership, file.source, file.exactSize, []),
): BeginOutputFileResult {
  return Object.freeze({
    revision,
    durableRanges,
    transaction: {
      writeRange: async () => undefined,
      checkpoint: async () => durableRanges,
      commit: async () => undefined,
      retire: async () => 'FileIsolated' as const,
      pause: async () => undefined,
      ...overrides,
    },
  })
}
