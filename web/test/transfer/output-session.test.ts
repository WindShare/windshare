import { describe, expect, it, vi } from 'vitest'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import {
  OutputSessionBindingError,
  VerifiedDurableRanges,
  outputCapabilities,
  snapshotOpenedOutputRevision,
  snapshotOutputFileRequest,
  validatePlanExecutionBinding,
  type DirectTreeExecution,
  type OutputSession,
} from '../../src/transfer/output-session'
import {
  fileEntry,
  identity,
  identityText,
  planAuthorityFixture,
  receiveIntentFixture,
} from './v2-job-fixture'

describe('plan-specific output boundary', () => {
  it('has no representation format and preserves source/artifact path separation', () => {
    const openRevision = vi.fn(async () => snapshotOpenedOutputRevision({
      shareInstance: identityText(1),
      fileId: identityText(4),
      fileRevision: identityText(5),
      exactSize: 3n,
    }))
    const request = snapshotOutputFileRequest({
      source: { shareInstance: identityText(1), fileId: identityText(4) },
      sourcePath: ['source', 'file.bin'],
      artifactPath: ['result', 'renamed.bin'],
      expectedSize: 3n,
      openRevision,
    })
    expect(request.sourcePath).toEqual(['source', 'file.bin'])
    expect(request.artifactPath).toEqual(['result', 'renamed.bin'])
    expect('format' in request).toBe(false)
    expect(request.openRevision).toBe(openRevision)
  })

  it('rejects malformed opened revisions and noncanonical output identities', () => {
    expect(() => snapshotOpenedOutputRevision({
      shareInstance: identityText(1),
      fileId: identityText(4),
      fileRevision: identityText(5),
      exactSize: -1n,
    })).toThrow(/size is invalid/)
    expect(() => new VerifiedDurableRanges({
      backend: 'test',
      outputSessionId: 'session',
      canonicalPath: ['file.bin'],
      ownedFileIdentity: 'owned',
    }, {
      shareInstance: identityText(1),
      fileId: identityText(4),
      fileRevision: identityText(5),
    }, 4n, [{ start: 2n, end: 4n }, { start: 1n, end: 2n }]))
      .toThrow(/sorted, non-overlapping/)
  })

  it('requires DirectTree to expose incremental directory authority', async () => {
    const selection = new V2SelectionPolicy()
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })
    const output: OutputSession = {
      identity: { backend: 'test', outputSessionId: 'session' },
      capabilities: outputCapabilities({
        durability: 'None',
        randomWrite: false,
        fileFailureIsolation: true,
        modificationTime: false,
      }),
      beginFile: async () => { throw new Error('not used') },
    }
    const malformed = {
      planKind: 'direct-tree',
      output,
      settle: async () => { throw new Error('not used') },
      pause: async () => { throw new Error('not used') },
    } as unknown as DirectTreeExecution
    expect(() => validatePlanExecutionBinding(intent, malformed))
      .toThrow(OutputSessionBindingError)
  })

  it('accepts the explicit plan adapter without consulting a session format', async () => {
    const file = fileEntry(identity(4), 'file.bin', 0n)
    const selection = new V2SelectionPolicy(false)
    selection.toggle(file, [identityText(2)])
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })
    const authority = planAuthorityFixture()
    const execution = await authority.openDirectAtomic(
      intent as Parameters<typeof authority.openDirectAtomic>[0],
      new AbortController().signal,
    )
    expect(validatePlanExecutionBinding(intent, execution).planKind).toBe('direct-atomic')
    expect('format' in execution.output).toBe(false)
  })
})
