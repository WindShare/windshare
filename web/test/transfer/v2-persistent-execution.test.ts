import { describe, expect, it, vi } from 'vitest'

import type { FinalFileCheckpointProof } from '../../src/output/persistence/journal'
import { createMaterializationLedgerBinding } from '../../src/output/materialization-ledger/codec'
import {
  createMaterializationDirectoryAdmittedEntry,
  createMaterializationDirectoryFinalizedEntry,
} from '../../src/output/materialization-ledger/journal'
import type {
  PersistentByteRange,
  PersistentDirectoryLedgerRequest,
  PersistentFileRequest,
  PersistentFileTransactionPort,
  PersistentMaterializationPort,
  PersistentOutputNamespaceClaimPort,
} from '../../src/output/persistent-tree/contracts'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { ReceiveIntent } from '../../src/transfer/intent'
import {
  V2OutputPausedError,
  type AuthenticatedLogicalSiblingMembership,
} from '../../src/transfer/job/contract'
import {
  snapshotLogicalArtifactPath,
  snapshotMaterializationRootRelativePath,
  snapshotSourceAuthenticationPath,
} from '../../src/transfer/job/coordinate/direct-tree'
import {
  OutputDirectoryMutationError,
  VerifiedFinalOutputFile,
  disabledOutputExecutionProfile,
  outputSessionIdentity,
  snapshotDirectoryMaterializationRequest,
  type DirectoryMaterializationRequest,
  type ExactPreparationEvidence,
  type OutputFileRequest,
} from '../../src/transfer/output-session'
import { isolatedDirectoryOutputFailure } from '../../src/transfer/job/failures'
import {
  EMPTY_TRANSFER_FAILURE_SUMMARY,
  transferWorkerSettlement,
} from '../../src/transfer/outcome'
import {
  createPersistentDirectTreeExecution,
  createPersistentWorkspaceExecution,
  type PersistentDirectTreeSettlementAuthority,
  type PersistentDirectTreeMaterializationEvidence,
  type PersistentWorkspaceExecutionInput,
  type PersistentWorkspaceSettlementAuthority,
  type WorkspaceMaterializationEvidence,
} from '../../src/transfer/settlement/persistent-execution'
import {
  digestIdentity,
  fileEntry,
  identity,
  identityText,
  receiveIntentFixture,
  selectOnlyFile,
} from './v2-job-fixture'

const SIGNAL = new AbortController().signal
const SUCCESS = transferWorkerSettlement('Succeeded', EMPTY_TRANSFER_FAILURE_SUMMARY)
const PAUSED = transferWorkerSettlement('Paused', EMPTY_TRANSFER_FAILURE_SUMMARY)

describe('persistent namespace claim bridge', () => {
  it('rejects membership authority from another committed generation', () => {
    expect(() => snapshotDirectoryMaterializationRequest(rootDirectoryRequest(
      identityText(2),
      identityText(90),
      {
        directoryId: identityText(2),
        generation: identityText(91),
        hasCommittedName: async () => false,
      },
    ))).toThrow('does not match the admitted directory generation')
  })

  it('binds lazy authenticated membership without querying it on an ordinary directory admission', async () => {
    const intent = asDirectTree(await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection: new V2SelectionPolicy(),
    }))
    const materialization = new PersistentMaterializationFixture(intent)
    const hasCommittedName = vi.fn(async (candidate: string) => candidate === 'occupied')
    const membership: AuthenticatedLogicalSiblingMembership = Object.freeze({
      directoryId: intent.syntheticRoot,
      generation: identityText(90),
      hasCommittedName,
    })
    let bound: Parameters<PersistentOutputNamespaceClaimPort['bindDirectoryNamespace']>[0] | undefined
    const namespaceClaims: PersistentOutputNamespaceClaimPort = {
      bindDirectoryNamespace: (claim) => {
        materialization.events.push('namespace-bound')
        bound = claim
      },
    }
    const execution = await createPersistentDirectTreeExecution({
      executionProfile: disabledOutputExecutionProfile(1),
      intent,
      materialization,
      namespaceClaims,
      outputIdentity: outputSessionIdentity({
        backend: 'persistent-test',
        outputSessionId: 'namespace-claim-session',
      }),
      settlement: {
        beginTerminal: () => undefined,
        pause: async (_request, cut) => {
          await cut.closeMaterialization()
          return partialDirectoryState(intent)
        },
        settle: async (_request, cut) => {
          await cut.closeMaterialization()
          return publishedState(intent)
        },
      },
    })

    await execution.directories.admitDirectory(
      rootDirectoryRequest(intent.syntheticRoot, identityText(90), membership),
      SIGNAL,
    )

    expect(materialization.events.slice(0, 2)).toEqual([
      'namespace-bound',
      'ensure-directory:',
    ])
    expect(hasCommittedName).not.toHaveBeenCalled()
    if (bound === undefined) throw new Error('namespace claim was not bound')
    await expect(bound.logicalSiblingMembership.hasCommittedName('occupied')).resolves.toBe(true)
    expect(hasCommittedName).toHaveBeenCalledOnce()
  })

  it('isolates a later logical directory collision without retrying its namespace mutation', async () => {
    const intent = asDirectTree(await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection: new V2SelectionPolicy(),
    }))
    const collision = new OutputDirectoryMutationError(
      'logical directory collides with an operation-owned compatible name',
      false,
    )
    const ensureDirectory = vi.fn(async () => { throw collision })
    const execution = await createPersistentDirectTreeExecution({
      executionProfile: disabledOutputExecutionProfile(1),
      intent,
      materialization: {
        beginFile: async () => { throw new Error('file materialization must not start') },
        ensureDirectory,
        materializeDirectory: async () => ensureDirectory(),
        finalizeDirectory: async () => { throw new Error('failed admission must not finalize') },
        close: async () => undefined,
      },
      outputIdentity: outputSessionIdentity({
        backend: 'persistent-test',
        outputSessionId: 'late-logical-collision',
      }),
      settlement: {
        beginTerminal: () => undefined,
        pause: async (_request, cut) => {
          await cut.closeMaterialization()
          return partialDirectoryState(intent)
        },
        settle: async (_request, cut) => {
          await cut.closeMaterialization()
          return publishedState(intent)
        },
      },
    })

    let failure: unknown
    try {
      await execution.directories.admitDirectory(
        rootDirectoryRequest(intent.syntheticRoot, identityText(90)),
        SIGNAL,
      )
    } catch (error) {
      failure = error
    }

    expect(failure).toBeInstanceOf(V2OutputPausedError)
    expect((failure as Error).cause).toBe(collision)
    expect(ensureDirectory).toHaveBeenCalledOnce()
    expect(isolatedDirectoryOutputFailure(failure, true, intent.syntheticRoot))
      .toMatchObject({
        directoryId: intent.syntheticRoot,
        classification: {
          fault: { scope: 'directory-local' },
          materializationFailureReason: 'directory-finalize-failed',
        },
      })
  })

})

describe('persistent production execution bridge', () => {
  it('keeps DirectTree revision authority ahead of file creation and settles from proofs', async () => {
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const intent = asDirectTree(await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection: new V2SelectionPolicy(),
    }))
    const materialization = new PersistentMaterializationFixture(intent, [{ start: 0n, end: 2n }])
    let settledEvidence: PersistentDirectTreeMaterializationEvidence | undefined
    const beginTerminal = vi.fn()
    const settlement: PersistentDirectTreeSettlementAuthority = {
      beginTerminal,
      pause: async (_request, cut) => {
        await cut.closeMaterialization()
        return partialDirectoryState(intent)
      },
      settle: async (request, cut) => {
        expect(request.transferJobId).toBe('transfer-job-1')
        settledEvidence = cut.evidence
        await cut.closeMaterialization()
        return publishedState(intent)
      },
    }
    const execution = await createPersistentDirectTreeExecution({
      executionProfile: disabledOutputExecutionProfile(1),
      intent,
      materialization,
      outputIdentity: outputSessionIdentity({
        backend: 'persistent-test',
        outputSessionId: 'direct-tree-session',
      }),
      settlement,
    })
    const admission = await execution.directories.admitDirectory(
      rootDirectoryRequest(intent.syntheticRoot, identityText(90)),
      SIGNAL,
    )
    const opened = await execution.output.beginFile(outputRequest({
      fileId: file.idText,
      sourcePath: [file.name],
      artifactPath: [file.name],
      expectedSize: file.expectedSize,
      parentAdmission: admission,
      events: materialization.events,
    }), SIGNAL)

    expect(opened.durableRanges.ranges).toEqual([{ start: 0n, end: 2n }])
    expect(materialization.events.indexOf('authenticated-revision'))
      .toBeLessThan(materialization.events.indexOf('owned-file-created'))
    await opened.transaction.writeRange(2n, new Uint8Array([7, 7]), SIGNAL)
    await expect(opened.transaction.commit(SIGNAL)).resolves.toMatchObject({ fileSize: 4n })
    await execution.directories.finalizeDirectory(admission, SIGNAL)
    execution.beginTerminal('settle')
    expect(beginTerminal).toHaveBeenCalledWith('settle')
    expect(execution.terminalSettlementInitiated?.()).toBe(true)

    await expect(execution.settle({
      transferJobId: 'transfer-job-1',
      worker: SUCCESS,
      materialization: {
        entryCount: 1n,
        fileCount: 1n,
        directoryCount: 0n,
        rawBytes: file.expectedSize,
      },
    }, SIGNAL)).resolves.toMatchObject({ kind: 'published' })
    expect(materialization.closeCount).toBe(1)
    expect(materialization.terminalCloseCount).toBe(1)
    expect(settledEvidence).toEqual({
      kind: 'direct-tree-ledger',
      materializationBindingDigest: intent.plan.reservation.digest,
    })
    expect(settledEvidence).not.toHaveProperty('entries')
  })

  it('types only backend directory rejections as output-wide state I/O', async () => {
    const intent = asDirectTree(await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection: new V2SelectionPolicy(),
    }))
    const raw = new DOMException('root directory write failed', 'UnknownError')
    const ensureDirectory = vi.fn(async () => { throw raw })
    const materialization: PersistentMaterializationPort = {
      beginFile: async () => { throw new Error('file materialization must not start') },
      ensureDirectory,
      materializeDirectory: async () => ensureDirectory(),
      finalizeDirectory: async () => { throw new Error('failed admission must not finalize') },
      close: async () => undefined,
    }
    const execution = await createPersistentDirectTreeExecution({
      executionProfile: disabledOutputExecutionProfile(1),
      intent,
      materialization,
      outputIdentity: outputSessionIdentity({
        backend: 'persistent-test',
        outputSessionId: 'directory-error-boundary',
      }),
      settlement: {
        beginTerminal: () => undefined,
        pause: async (_request, cut) => {
          await cut.closeMaterialization()
          return partialDirectoryState(intent)
        },
        settle: async (_request, cut) => {
          await cut.closeMaterialization()
          return publishedState(intent)
        },
      },
    })

    let backendFailure: unknown
    try {
      await execution.directories.admitDirectory(
        rootDirectoryRequest(intent.syntheticRoot, identityText(90)),
        SIGNAL,
      )
    } catch (error) {
      backendFailure = error
    }
    expect(backendFailure).toBeInstanceOf(V2OutputPausedError)
    expect((backendFailure as Error).cause).toBe(raw)

    let contractFailure: unknown
    try {
      await execution.directories.admitDirectory(
        rootDirectoryRequest(identityText(99), identityText(91)),
        SIGNAL,
      )
    } catch (error) {
      contractFailure = error
    }
    expect(contractFailure).not.toBeInstanceOf(V2OutputPausedError)
    expect(contractFailure).toMatchObject({ name: 'DirectoryAdmissionBindingError' })
    expect(ensureDirectory).toHaveBeenCalledOnce()
  })

  it('does not accept a lifecycle state while mutable materialization remains open', async () => {
    const intent = asDirectTree(await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection: new V2SelectionPolicy(),
    }))
    const materialization = new PersistentMaterializationFixture(intent)
    const execution = await createPersistentDirectTreeExecution({
      executionProfile: disabledOutputExecutionProfile(1),
      intent,
      materialization,
      outputIdentity: outputSessionIdentity({
        backend: 'persistent-test',
        outputSessionId: 'unclosed-session',
      }),
      settlement: {
        beginTerminal: () => undefined,
        pause: async (_request, cut) => {
          await cut.closeMaterialization()
          return partialDirectoryState(intent)
        },
        settle: async () => publishedState(intent),
      },
    })

    await expect(execution.settle({
      transferJobId: 'transfer-job-unclosed',
      worker: SUCCESS,
      materialization: {
        entryCount: 0n,
        fileCount: 0n,
        directoryCount: 0n,
        rawBytes: 0n,
      },
    }, SIGNAL)).rejects.toThrow('before closing materialization')
    expect(materialization.closeCount).toBe(0)
    await expect(execution.pause({
      worker: PAUSED,
      materialization: {
        entryCount: 0n,
        fileCount: 0n,
        directoryCount: 0n,
        rawBytes: 0n,
      },
      reason: new Error('settlement invariant'),
    }, SIGNAL)).resolves.toMatchObject({ kind: 'partial-directory' })
    expect(materialization.closeCount).toBe(1)
  })

  it('delegates DirectTree directory completeness to the durable-ledger settlement authority', async () => {
    const intent = asDirectTree(await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection: new V2SelectionPolicy(),
    }))
    const materialization = new PersistentMaterializationFixture(intent)
    const settle = vi.fn(async (
      _request: Parameters<PersistentDirectTreeSettlementAuthority['settle']>[0],
      cut: Parameters<PersistentDirectTreeSettlementAuthority['settle']>[1],
    ) => {
      await cut.closeMaterialization()
      expect(cut.sealEvidence()).toEqual({
        kind: 'direct-tree-ledger',
        materializationBindingDigest: intent.plan.reservation.digest,
      })
      throw new TypeError('sealed ledger requires every directory to finalize')
    })
    const execution = await createPersistentDirectTreeExecution({
      executionProfile: disabledOutputExecutionProfile(1),
      intent,
      materialization,
      outputIdentity: outputSessionIdentity({
        backend: 'persistent-test',
        outputSessionId: 'missing-directory-proof',
      }),
      settlement: {
        beginTerminal: () => undefined,
        pause: async (_request, cut) => {
          await cut.closeMaterialization()
          return partialDirectoryState(intent)
        },
        settle,
      },
    })
    await execution.directories.admitDirectory(
      rootDirectoryRequest(intent.syntheticRoot, identityText(90)),
      SIGNAL,
    )

    await expect(execution.settle({
      transferJobId: 'transfer-job-missing-directory-proof',
      worker: SUCCESS,
      materialization: {
        entryCount: 0n,
        fileCount: 0n,
        directoryCount: 0n,
        rawBytes: 0n,
      },
    }, SIGNAL)).rejects.toThrow('requires every directory to finalize')
    // The lifecycle authority owns both the quiescent cut and durable-ledger validation.
    expect(settle).toHaveBeenCalledOnce()
  })

  it('allows a lifecycle owner to record close uncertainty as NeedsAttention', async () => {
    const intent = asDirectTree(await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection: new V2SelectionPolicy(),
    }))
    const closeFailure = new Error('root lease release is unknown')
    const materialization = new PersistentMaterializationFixture(
      intent,
      [],
      undefined,
      closeFailure,
    )
    const execution = await createPersistentDirectTreeExecution({
      executionProfile: disabledOutputExecutionProfile(1),
      intent,
      materialization,
      outputIdentity: outputSessionIdentity({
        backend: 'persistent-test',
        outputSessionId: 'unknown-close-session',
      }),
      settlement: {
        beginTerminal: () => undefined,
        pause: async () => needsAttentionState(intent),
        settle: async (_request, cut) => {
          await cut.closeMaterialization().catch(() => undefined)
          return needsAttentionState(intent)
        },
      },
    })

    await expect(execution.settle({
      transferJobId: 'transfer-job-unknown-close',
      worker: SUCCESS,
      materialization: {
        entryCount: 0n,
        fileCount: 0n,
        directoryCount: 0n,
        rawBytes: 0n,
      },
    }, SIGNAL)).resolves.toMatchObject({ kind: 'needs-attention' })
    expect(materialization.closeCount).toBe(1)
  })

})

describe('persistent Workspace production execution bridge', () => {
  it('retains exact Workspace OriginalFile generation and final checkpoint evidence', async () => {
    const file = fileEntry(identity(12), 'original.bin', 3n)
    const intent = asWorkspaceOriginal(await receiveIntentFixture({
      planKind: 'workspace-then-publish',
      artifactKind: 'original-file',
      selection: selectOnlyFile(file),
      file,
    }))
    const materialization = new PersistentMaterializationFixture(intent)
    let settledEvidence: WorkspaceMaterializationEvidence | undefined
    let settlementCalls = 0
    const settlement: PersistentWorkspaceSettlementAuthority = {
      pause: async (_request, cut) => {
        await cut.closeMaterialization()
        return resumableState(intent)
      },
      settle: async (_request, cut) => {
        settlementCalls += 1
        settledEvidence = cut.evidence
        await cut.closeMaterialization()
        return waitingToSaveState(intent)
      },
    }
    const execution = await createPersistentWorkspaceExecution({
      intent,
      admission: {
        kind: 'single-file',
        evidence: {
          fileId: file.idText,
          containingDirectoryId: identityText(2),
          generation: identityText(91),
          catalogSize: file.expectedSize,
          sourcePath: Object.freeze([file.name]),
        },
      },
      materialization,
      outputIdentity: outputSessionIdentity({
        backend: 'origin-private-test',
        outputSessionId: 'workspace-original-session',
      }),
      settlement,
      signal: SIGNAL,
    })
    await expect(execution.settle({
      transferJobId: 'transfer-job-original',
      worker: SUCCESS,
      materialization: {
        entryCount: 0n,
        fileCount: 0n,
        directoryCount: 0n,
        rawBytes: 0n,
      },
    }, SIGNAL)).rejects.toThrow('exact admitted checkpoint proof')
    expect(settlementCalls).toBe(0)
    const opened = await execution.output.beginFile(outputRequest({
      fileId: file.idText,
      sourcePath: [file.name],
      artifactPath: [file.name],
      expectedSize: file.expectedSize,
      events: materialization.events,
    }), SIGNAL)
    await opened.transaction.writeRange(0n, new Uint8Array([1, 2, 3]), SIGNAL)
    await opened.transaction.commit(SIGNAL)
    await execution.settle({
      transferJobId: 'transfer-job-original',
      worker: SUCCESS,
      materialization: {
        entryCount: 1n,
        fileCount: 1n,
        directoryCount: 0n,
        rawBytes: file.expectedSize,
      },
    }, SIGNAL)

    expect(settledEvidence?.generations).toEqual([{
      directoryId: identityText(2),
      generation: identityText(91),
    }])
    expect(settledEvidence?.entries).toMatchObject([{
      kind: 'file',
      artifactPath: [file.name],
      checkpoint: { checkpointGeneration: 1n },
    }])
    expect(settlementCalls).toBe(1)
  })

  it('materializes prepared Workspace ZIP directories and rejects aggregate-only sealing', async () => {
    const file = fileEntry(identity(13), 'member.bin', 5n)
    const intent = asWorkspaceZip(await receiveIntentFixture({
      planKind: 'workspace-then-publish',
      artifactKind: 'zip-archive',
      selection: selectOnlyFile(file),
      file,
    }))
    const evidence = preparedEvidence(file)
    const materialization = new PersistentMaterializationFixture(intent)
    const settle = vi.fn(async (
      _request: Parameters<PersistentWorkspaceSettlementAuthority['settle']>[0],
      cut: Parameters<PersistentWorkspaceSettlementAuthority['settle']>[1],
    ) => {
      await cut.closeMaterialization()
      return waitingToSaveState(intent)
    })
    const execution = await createPersistentWorkspaceExecution({
      intent,
      admission: { kind: 'prepared', evidence },
      materialization,
      outputIdentity: outputSessionIdentity({
        backend: 'origin-private-test',
        outputSessionId: 'workspace-zip-session',
      }),
      settlement: {
        pause: async (_request, cut) => {
          await cut.closeMaterialization()
          return resumableState(intent)
        },
        settle,
      },
      signal: SIGNAL,
    })
    expect(materialization.directories).toEqual([['windshare']])

    const opened = await execution.output.beginFile(outputRequest({
      fileId: file.idText,
      sourcePath: [file.name],
      artifactPath: ['windshare', file.name],
      expectedSize: file.expectedSize,
      events: materialization.events,
    }), SIGNAL)
    await opened.transaction.writeRange(0n, new Uint8Array(5), SIGNAL)
    await opened.transaction.commit(SIGNAL)

    await expect(execution.settle({
      transferJobId: 'transfer-job-zip',
      worker: SUCCESS,
      materialization: {
        entryCount: 2n,
        fileCount: 1n,
        directoryCount: 1n,
        rawBytes: 4n,
      },
    }, SIGNAL)).rejects.toThrow('worker summary cannot substitute')
    expect(settle).not.toHaveBeenCalled()

    await execution.settle({
      transferJobId: 'transfer-job-zip',
      worker: SUCCESS,
      materialization: {
        entryCount: 2n,
        fileCount: 1n,
        directoryCount: 1n,
        rawBytes: file.expectedSize,
      },
    }, SIGNAL)
    expect(settle).toHaveBeenCalledOnce()
    const admitted = settle.mock.calls[0]?.[1].evidence
    expect(admitted?.generations).toEqual(evidence.generations)
    expect(admitted?.entries).toHaveLength(2)
  })

  it('closes prepared Workspace authority when directory materialization fails', async () => {
    const file = fileEntry(identity(14), 'failed-member.bin', 2n)
    const intent = asWorkspaceZip(await receiveIntentFixture({
      planKind: 'workspace-then-publish',
      artifactKind: 'zip-archive',
      selection: selectOnlyFile(file),
      file,
    }))
    const failure = new Error('directory materialization failed')
    const beginFile = vi.fn(async () => {
      throw new Error('content must not start')
    })
    const close = vi.fn(async () => undefined)
    const materialization: PersistentMaterializationPort = {
      beginFile,
      ensureDirectory: async () => { throw failure },
      close,
    }

    await expect(createPersistentWorkspaceExecution({
      intent,
      admission: { kind: 'prepared', evidence: preparedEvidence(file) },
      materialization,
      outputIdentity: outputSessionIdentity({
        backend: 'origin-private-test',
        outputSessionId: 'failed-preparation-session',
      }),
      settlement: {
        pause: async () => resumableState(intent),
        settle: async () => waitingToSaveState(intent),
      },
      signal: SIGNAL,
    })).rejects.toBe(failure)
    expect(beginFile).not.toHaveBeenCalled()
    expect(close).toHaveBeenCalledOnce()
  })

  it('rejects a final checkpoint proof from another materialization namespace', async () => {
    const file = fileEntry(identity(15), 'foreign-proof.bin', 1n)
    const intent = asWorkspaceOriginal(await receiveIntentFixture({
      planKind: 'workspace-then-publish',
      artifactKind: 'original-file',
      selection: selectOnlyFile(file),
      file,
    }))
    const materialization = new PersistentMaterializationFixture(
      intent,
      [],
      digestIdentity(99),
    )
    const execution = await createPersistentWorkspaceExecution({
      intent,
      admission: {
        kind: 'single-file',
        evidence: {
          fileId: file.idText,
          containingDirectoryId: identityText(2),
          generation: identityText(90),
          catalogSize: file.expectedSize,
          sourcePath: Object.freeze([file.name]),
        },
      },
      materialization,
      outputIdentity: outputSessionIdentity({
        backend: 'origin-private-test',
        outputSessionId: 'foreign-proof-session',
      }),
      settlement: {
        pause: async () => resumableState(intent),
        settle: async () => waitingToSaveState(intent),
      },
      signal: SIGNAL,
    })
    const opened = await execution.output.beginFile(outputRequest({
      fileId: file.idText,
      sourcePath: [file.name],
      artifactPath: [file.name],
      expectedSize: file.expectedSize,
      events: materialization.events,
    }), SIGNAL)

    await expect(opened.transaction.commit(SIGNAL))
      .rejects.toThrow('final checkpoint proof escaped')
  })
})

class PersistentMaterializationFixture implements PersistentMaterializationPort {
  readonly events: string[] = []
  readonly directories: string[][] = []
  readonly #intent: ReceiveIntent
  readonly #initialRanges: readonly PersistentByteRange[]
  readonly #proofBindingDigest: string | undefined
  readonly #closeFailure: unknown
  readonly #directoryOwnedObjects = new Map<string, string>()
  #nextDirectoryIdentity = 160
  closeCount = 0
  terminalCloseCount = 0

  constructor(
    intent: ReceiveIntent,
    initialRanges: readonly PersistentByteRange[] = [],
    proofBindingDigest?: string,
    closeFailure?: unknown,
  ) {
    this.#intent = intent
    this.#initialRanges = initialRanges
    this.#proofBindingDigest = proofBindingDigest
    this.#closeFailure = closeFailure
  }

  async beginFile(request: PersistentFileRequest): Promise<PersistentFileTransactionPort> {
    this.events.push('begin-file')
    const revision = await request.openRevision()
    this.events.push('owned-file-created')
    const ownedObjectId = `owned:${revision.fileId}`
    let checkpointRanges = Object.freeze([...this.#initialRanges])
    const pending: PersistentByteRange[] = []
    return Object.freeze({
      revision,
      ownedObjectId,
      initialDurableRanges: Object.freeze([...checkpointRanges]),
      verifiedRanges: checkpointRanges,
      writeRange: async (offset: bigint, data: Uint8Array, signal?: AbortSignal) => {
        signal?.throwIfAborted()
        pending.push(Object.freeze({ start: offset, end: offset + BigInt(data.byteLength) }))
      },
      checkpoint: async (signal?: AbortSignal) => {
        signal?.throwIfAborted()
        checkpointRanges = mergeRanges([...checkpointRanges, ...pending])
        pending.length = 0
        return checkpointRanges
      },
      automaticCheckpoint: async (
        _trigger: Parameters<PersistentFileTransactionPort['automaticCheckpoint']>[0],
        _budget: Parameters<PersistentFileTransactionPort['automaticCheckpoint']>[1],
        signal?: AbortSignal,
      ) => {
        signal?.throwIfAborted()
        checkpointRanges = mergeRanges([...checkpointRanges, ...pending])
        pending.length = 0
        return Object.freeze({
          kind: 'advanced' as const,
          durableRanges: checkpointRanges,
          cost: Object.freeze({
            prefixCopyBytes: 0n,
            cumulativeWriteAmplificationBytes: 0n,
            peakTemporaryBytes: 0n,
          }),
        })
      },
      commit: async (signal?: AbortSignal) => {
        signal?.throwIfAborted()
        await Promise.resolve()
        const checkpointProof = finalProof({
          intent: this.#intent,
          request,
          revision,
          ownedObjectId,
          ...(this.#proofBindingDigest === undefined
            ? {}
            : { materializationBindingDigest: this.#proofBindingDigest }),
        })
        const outputSession = request.outputSession ?? outputSessionIdentity({
          backend: 'persistent-fixture',
          outputSessionId: this.#intent.operationId,
        })
        return Object.freeze({
          ...checkpointProof,
          checkpointProof,
          finalOutput: new VerifiedFinalOutputFile(
            Object.freeze({
              ...outputSession,
              canonicalPath: request.materializationRelativePath,
              ownedFileIdentity: ownedObjectId,
            }),
            Object.freeze({
              shareInstance: request.shareInstance ?? revision.fileId,
              fileId: revision.fileId,
              fileRevision: revision.fileRevision,
            }),
            revision.exactSize,
          ),
        })
      },
      pause: async () => {
        checkpointRanges = mergeRanges([...checkpointRanges, ...pending])
        pending.length = 0
        return checkpointRanges
      },
      retire: async () => {
        this.events.push('transaction-retired')
      },
      close: async () => {
        this.events.push('transaction-closed')
      },
    })
  }

  async ensureDirectory(path: readonly string[]) {
    const snapshot = [...path]
    const key = JSON.stringify(snapshot)
    const existingOwnedObjectId = this.#directoryOwnedObjects.get(key)
    const ownedObjectId = existingOwnedObjectId ?? digestIdentity(this.#nextDirectoryIdentity++)
    this.#directoryOwnedObjects.set(key, ownedObjectId)
    this.directories.push(snapshot)
    this.events.push(`ensure-directory:${snapshot.join('/')}`)
    return Object.freeze({
      ownedObjectId,
      created: existingOwnedObjectId === undefined,
    })
  }

  async materializeDirectory(request: PersistentDirectoryLedgerRequest) {
    const relativePath = snapshotMaterializationRootRelativePath(request.relativePath)
    const materialized = await this.ensureDirectory(relativePath)
    const binding = await this.#directTreeLedgerBinding()
    const ledgerAdmission = await createMaterializationDirectoryAdmittedEntry(binding, {
      relativePath,
      directoryId: request.directoryId,
      generation: request.generation,
      ownedObjectId: materialized.ownedObjectId,
      ...(request.parent === undefined ? {} : { parent: request.parent }),
      ...(request.modifiedTime === undefined ? {} : { modifiedTime: request.modifiedTime }),
    })
    return Object.freeze({ ...materialized, ledgerAdmission })
  }

  async finalizeDirectory(
    admission: Parameters<NonNullable<PersistentMaterializationPort['finalizeDirectory']>>[0],
    outcome: Parameters<NonNullable<PersistentMaterializationPort['finalizeDirectory']>>[1],
  ) {
    return createMaterializationDirectoryFinalizedEntry(
      await this.#directTreeLedgerBinding(),
      admission,
      outcome,
    )
  }

  async #directTreeLedgerBinding() {
    if (this.#intent.plan.kind !== 'direct-tree') {
      throw new TypeError('test ledger binding requires DirectTree intent')
    }
    return createMaterializationLedgerBinding({
      operationId: this.#intent.operationId,
      receiveIntentDigest: this.#intent.digest,
      materializationBindingDigest: this.#intent.plan.reservation.digest,
      authorityRef: this.#intent.plan.reservation.authorityRef,
    })
  }

  async close(): Promise<void> {
    this.closeCount += 1
    this.events.push('materialization-closed')
    if (this.#closeFailure !== undefined) throw this.#closeFailure
  }

  async closeForTerminalSettlement(): Promise<void> {
    this.terminalCloseCount += 1
    return this.close()
  }
}

function rootDirectoryRequest(
  directoryId: string,
  generation: string,
  logicalSiblingMembership?: AuthenticatedLogicalSiblingMembership,
): DirectoryMaterializationRequest {
  return Object.freeze({
    directory: Object.freeze({
      directoryId,
      generation,
      path: snapshotMaterializationRootRelativePath([]),
    }),
    sourceAuthenticationPath: snapshotSourceAuthenticationPath([]),
    logicalArtifactPath: snapshotLogicalArtifactPath(['windshare']),
    ...(logicalSiblingMembership === undefined ? {} : { logicalSiblingMembership }),
  })
}

function outputRequest(input: {
  readonly fileId: string
  readonly sourcePath: readonly string[]
  readonly artifactPath: readonly string[]
  readonly materializationRelativePath?: readonly string[]
  readonly expectedSize: bigint
  readonly parentAdmission?: OutputFileRequest['parentAdmission']
  readonly events: string[]
}): OutputFileRequest {
  return {
    source: { shareInstance: identityText(1), fileId: input.fileId },
    sourceAuthenticationPath: snapshotSourceAuthenticationPath(input.sourcePath),
    logicalArtifactPath: snapshotLogicalArtifactPath(input.artifactPath),
    materializationRelativePath: snapshotMaterializationRootRelativePath(
      input.materializationRelativePath ?? input.artifactPath,
    ),
    expectedSize: input.expectedSize,
    ...(input.parentAdmission === undefined ? {} : { parentAdmission: input.parentAdmission }),
    openRevision: async (signal) => {
      signal.throwIfAborted()
      input.events.push('authenticated-revision')
      return Object.freeze({
        shareInstance: identityText(1),
        fileId: input.fileId,
        fileRevision: identityText(51),
        exactSize: input.expectedSize,
      })
    },
  }
}

function finalProof(input: {
  readonly intent: ReceiveIntent
  readonly request: PersistentFileRequest
  readonly revision: PersistentFileTransactionPort['revision']
  readonly ownedObjectId: string
  readonly materializationBindingDigest?: string
}): FinalFileCheckpointProof {
  return Object.freeze({
    operationId: input.intent.operationId,
    receiveIntentDigest: input.intent.digest,
    materializationBindingDigest: input.materializationBindingDigest ??
      materializationBindingDigest(input.intent),
    recordId: identityText(61),
    recordDigest: digestIdentity(62),
    checkpointGeneration: 1n,
    fileId: input.revision.fileId,
    fileRevision: input.revision.fileRevision,
    canonicalPath: Object.freeze([...input.request.materializationRelativePath]),
    exactSize: input.revision.exactSize,
    ownedObjectId: input.ownedObjectId,
    complete: true,
  })
}

function materializationBindingDigest(intent: ReceiveIntent): string {
  switch (intent.plan.kind) {
    case 'direct-tree':
    case 'direct-atomic': return intent.plan.reservation.digest
    case 'workspace-then-publish': return intent.plan.workspace.digest
    case 'portable-handoff': return intent.plan.portable.digest
    case 'direct-resumable-zip': return intent.plan.binding.digest
  }
}

function mergeRanges(input: readonly PersistentByteRange[]): readonly PersistentByteRange[] {
  const ordered = [...input].sort((left, right) => left.start < right.start ? -1 : 1)
  const merged: Array<{ start: bigint; end: bigint }> = []
  for (const range of ordered) {
    const previous = merged.at(-1)
    if (previous === undefined || range.start > previous.end) {
      merged.push({ start: range.start, end: range.end })
    } else if (range.end > previous.end) {
      previous.end = range.end
    }
  }
  return Object.freeze(merged.map(range => Object.freeze(range)))
}

function preparedEvidence(
  file: ReturnType<typeof fileEntry>,
): ExactPreparationEvidence {
  return Object.freeze({
    generations: Object.freeze([Object.freeze({
      directoryId: identityText(2),
      generation: identityText(90),
    })]),
    entries: Object.freeze([
      Object.freeze({
        kind: 'directory' as const,
        sourcePath: Object.freeze([]),
        artifactPath: Object.freeze(['windshare']),
        directoryId: identityText(2),
        generation: identityText(90),
        role: 'result-root' as const,
      }),
      Object.freeze({
        kind: 'file' as const,
        sourcePath: Object.freeze([file.name]),
        artifactPath: Object.freeze(['windshare', file.name]),
        fileId: file.idText,
        containingDirectoryId: identityText(2),
        generation: identityText(90),
        exactSize: file.expectedSize,
      }),
    ]),
    entryCount: 2n,
    fileCount: 1n,
    directoryCount: 1n,
    selectedRawBytes: file.expectedSize,
  })
}

function publishedState(intent: ReceiveIntent): ReceiveLifecycleState {
  return lifecycleState(intent, {
    kind: 'published',
    receiptDigest: digestIdentity(70),
    cleanupState: 'clean',
  })
}

function partialDirectoryState(intent: ReceiveIntent): ReceiveLifecycleState {
  return lifecycleState(intent, {
    kind: 'partial-directory',
    receiptDigest: digestIdentity(71),
    successCount: 0n,
    failureCount: 1n,
  })
}

function resumableState(intent: ReceiveIntent): ReceiveLifecycleState {
  return lifecycleState(intent, {
    kind: 'resumable-receive',
    activeLeaseId: identityText(72),
    checkpointDigest: digestIdentity(73),
  })
}

function waitingToSaveState(intent: ReceiveIntent): ReceiveLifecycleState {
  return lifecycleState(intent, {
    kind: 'waiting-to-save',
    packageDigest: digestIdentity(74),
    expiresAt: 1,
  })
}

function needsAttentionState(intent: ReceiveIntent): ReceiveLifecycleState {
  return lifecycleState(intent, {
    kind: 'needs-attention',
    reason: 'cleanup-unknown',
    lastVerifiedRecordDigest: digestIdentity(75),
  })
}

function lifecycleState(intent: ReceiveIntent, payload: Record<string, unknown>): ReceiveLifecycleState {
  return Object.freeze({
    operationId: intent.operationId,
    receiveIntentDigest: intent.digest,
    generation: 1n,
    ...payload,
  }) as unknown as ReceiveLifecycleState
}

function asDirectTree(intent: ReceiveIntent): Parameters<
  typeof createPersistentDirectTreeExecution
>[0]['intent'] {
  return intent as Parameters<typeof createPersistentDirectTreeExecution>[0]['intent']
}

function asWorkspaceOriginal(intent: ReceiveIntent): Parameters<
  typeof createPersistentWorkspaceExecution
>[0]['intent'] & Readonly<{ artifact: { readonly kind: 'original-file' } }> {
  return intent as Extract<
    PersistentWorkspaceExecutionInput,
    Readonly<{ admission: Readonly<{ kind: 'single-file' }> }>
  >['intent']
}

function asWorkspaceZip(intent: ReceiveIntent): Parameters<
  typeof createPersistentWorkspaceExecution
>[0]['intent'] & Readonly<{ artifact: { readonly kind: 'zip-archive' } }> {
  return intent as Extract<
    PersistentWorkspaceExecutionInput,
    Readonly<{ admission: Readonly<{ kind: 'prepared' }> }>
  >['intent']
}
