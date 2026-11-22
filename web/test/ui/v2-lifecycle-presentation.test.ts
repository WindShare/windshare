import { describe, expect, it } from 'vitest'

import type {
  RecoverySummary,
} from '../../src/transfer/lifecycle/recovery-summary'
import type {
  ReceiveLifecycleState,
  ReceiveLifecycleStatePayload,
} from '../../src/output/workspace'
import type { ArtifactSpec, MaterializationPlan } from '../../src/transfer/intent'
import {
  presentReceiveLifecycle,
} from '../../src/ui/lifecycle-presentation'

const ORIGINAL = { kind: 'original-file', suggestedName: 'report.txt' } as ArtifactSpec
const ZIP = { kind: 'zip-archive', suggestedName: 'photos.zip' } as ArtifactSpec
const TREE = {
  kind: 'directory-tree',
  layout: { kind: 'result-root', root: { name: 'photos' } },
} as ArtifactSpec

type ResumableFileSetPayload = Extract<ReceiveLifecycleStatePayload, {
  kind: 'resumable-receive'
  payloadKind: 'file-set'
}>
type TestLifecyclePayload = Exclude<ReceiveLifecycleStatePayload, ResumableFileSetPayload> |
  (Omit<ResumableFileSetPayload, 'selectionFacts'> &
    Readonly<{ selectionFacts?: ResumableFileSetPayload['selectionFacts'] }>)

describe('receive lifecycle product phases', () => {
  it('explains how to resume a native archive after storage pressure', () => {
    const state = lifecycle({
      kind: 'resumable-receive', payloadKind: 'workspace-archive', objectId: 'object',
      checkpointGeneration: 2n, occupiedBytes: 2048n, completedFileCount: 1n,
      completedBytes: 1024n, discoveryComplete: false, pauseReason: 'storage-pressure',
    })
    expect(present(state, ZIP)).toMatchObject({
      title: 'Browser storage is full',
      description: expect.stringContaining('Free browser storage, then choose Continue.'),
    })
  })


  it.each([
    [lifecycle({ kind: 'preparing', preparationId: 'preparation' }), ZIP, 'Checking selected content'],
    [lifecycle({ kind: 'receiving', activeLeaseId: 'lease' }), ZIP, 'Receiving files'],
    [lifecycle({ kind: 'packaging', activeLeaseId: 'lease', sealedMaterializationDigest: 'seal', packageTempObjectId: 'temporary' }), ZIP, 'Finalizing ZIP'],
    [lifecycle({ kind: 'waiting-to-save', packageDigest: 'package' }), ZIP, 'Ready to save'],
    [lifecycle({ kind: 'publishing-managed', activeLeaseId: 'lease', packageDigest: 'package', publicationAttemptId: 'attempt' }), ZIP, 'Saving photos.zip'],
    [lifecycle({ kind: 'handing-off', activeLeaseId: 'lease', attemptKind: 'workspace', attemptId: 'attempt', packageDigest: 'package' }), ZIP, 'Starting browser download'],
  ] as const)('presents %s without exposing storage implementation language', (state, artifact, title) => {
    const presentation = present(state, artifact)

    expect(presentation.title).toBe(title)
    expect(`${presentation.title} ${presentation.description}`)
      .not.toMatch(/backend|OPFS|stream|admission|partial.?ZIP/iu)
  })

  it('keeps receive, ZIP generation, save wait, publication, and browser handoff distinct', () => {
    const states = [
      lifecycle({ kind: 'receiving', activeLeaseId: 'lease' }),
      lifecycle({ kind: 'packaging', activeLeaseId: 'lease', sealedMaterializationDigest: 'seal', packageTempObjectId: 'temporary' }),
      lifecycle({ kind: 'waiting-to-save', packageDigest: 'package' }),
      lifecycle({ kind: 'publishing-managed', activeLeaseId: 'lease', packageDigest: 'package', publicationAttemptId: 'attempt' }),
      lifecycle({ kind: 'handing-off', activeLeaseId: 'lease', attemptKind: 'workspace', attemptId: 'attempt', packageDigest: 'package' }),
    ]
    const titles = states.map((state) => present(state, ZIP).title)

    expect(new Set(titles).size).toBe(states.length)
    expect(present(states[1]!, ZIP).description).toContain('sender can go offline')
  })
})

describe('receive lifecycle terminal presentation', () => {
  it.each([
    [lifecycle({ kind: 'published', receiptDigest: 'receipt', cleanupState: 'clean' }), 'Saved', 'terminal'],
    [lifecycle({ kind: 'download-started', attemptKind: 'portable', attemptId: 'attempt' }), 'Download started', 'terminal'],
    [lifecycle({ kind: 'partial-directory', reason: 'failures', successCount: 2n, failureCount: 1n, receiptDigest: 'receipt' }), 'Some files were saved', 'terminal'],
    [lifecycle({ kind: 'restart-required', reason: 'portable-aborted', receiptDigest: 'receipt' }), 'Start again required', 'terminal'],
    [lifecycle({ kind: 'discarded', cleanupReceiptDigest: 'cleanup' }), 'Task discarded', 'terminal'],
    [lifecycle({ kind: 'needs-attention', reason: 'publication-unknown', lastVerifiedRecordDigest: 'record' }), 'Needs attention', 'terminal'],
  ] as const)('distinguishes %s', (state, title, category) => {
    const presentation = present(state, state.kind === 'partial-directory' ? TREE : ORIGINAL)

    expect(presentation).toMatchObject({ stateKind: state.kind, title, category })
  })

  it('does not claim that browser handoff means the file was saved', () => {
    const presentation = present(lifecycle({
      kind: 'download-started',
      attemptKind: 'portable',
      attemptId: 'attempt',
    }), ORIGINAL, 'portable-handoff')

    expect(presentation.title).toBe('Download started')
    expect(presentation.description).toContain('cannot confirm')
    expect(presentation.description).not.toMatch(/saved successfully/iu)
    expect(presentation.actions).toEqual([])
  })

  it('keeps partial directory success visible without inventing a resumable terminal action', () => {
    const presentation = present(lifecycle({
      kind: 'partial-directory',
      reason: 'stopped',
      successCount: 3n,
      failureCount: 0n,
      receiptDigest: 'receipt',
    }), TREE, 'direct-tree')

    expect(presentation.description).toContain('3 files remain saved')
    expect(presentation.actions).toEqual([])
  })
})

describe('direct tree recovery presentation', () => {
  it('offers preserve and explicit owned-file restart choices for DirectTree recovery', () => {
    const state = lifecycle({
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: 'checkpoint-set',
      completedFileCount: 1n,
      retainedBytes: 1_024n,
      completedBytes: 1_024n,
    })
    const resumable = present(
      state,
      TREE,
      'direct-tree',
      undefined,
      recoverySummary({ discovery: 'complete' }),
    )

    expect(resumable.description).toBe(
      'Selection complete. Completed: 1 file, 1.0 KiB. Verified partial data: 2 files, 512 B. ' +
      'Preserve partial files: 2.0 KiB remaining; up to 384 B of temporary destination space. ' +
      'Restart incomplete files: 2.5 KiB remaining, including 512 B of verified data to redownload.',
    )
    expect(resumable.actions).toEqual([
      { kind: 'continue', label: 'Continue and preserve partial files', destructive: false },
      { kind: 'redownload', label: 'Restart incomplete files', destructive: true },
    ])

    const incomplete = present(
      state,
      TREE,
      'direct-tree',
      undefined,
      recoverySummary({ discovery: 'known-so-far' }),
    )
    expect(incomplete.description).toContain('Known so far (discovery incomplete)')
    expect(incomplete.description).toContain('These totals can grow')
  })

  it('withholds DirectTree recovery actions until exact checkpoint costs are validated', () => {
    const resumable = present(lifecycle({
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: 'checkpoint-set',
      completedFileCount: 1n,
      retainedBytes: 1_024n,
      completedBytes: 1_024n,
    }), TREE, 'direct-tree')

    expect(resumable.description).toContain('Recovery costs are being validated')
    expect(resumable.actions).toEqual([])
  })
})

describe('retained usage and lifecycle-valid actions', () => {
  it('shows retained workspace usage without a deletion deadline', () => {
    const state = lifecycle({ kind: 'waiting-to-save', packageDigest: 'package' })
    const presentation = present(state, ZIP, 'workspace-then-publish', {
      ownedBytes: 1_024n,
      maximumBytes: 4_096n,
    })

    expect(presentation.usage).toMatchObject({
      ownedBytes: 1_024n,
      maximumBytes: 4_096n,
      label: '1.0 KiB of 4.0 KiB used by this task',
    })
    expect(presentation.actions.map((action) => action.kind)).toEqual(['save', 'delete'])

    expect(present(state, ZIP, 'direct-tree', { ownedBytes: 1_024n }).usage).toBeNull()
  })

  it('offers continuation and explicit discard for retained progress', () => {
    const state = lifecycle({
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: 'checkpoints',
      completedFileCount: 2n,
      retainedBytes: 512n,
      completedBytes: 512n,
    })
    const presentation = present(state, TREE, 'workspace-then-publish')
    expect(presentation.actions.map((action) => action.kind)).toEqual(['continue', 'discard'])
  })

  it('keeps browser handoff repeatable without inventing a managed location action', () => {
    const state = lifecycle({
      kind: 'download-started',
      attemptKind: 'workspace',
      attemptId: 'attempt',
      packageDigest: 'package',
    })
    const presentation = present(state, ZIP, 'workspace-then-publish', {
      ownedBytes: 2_048n,
    })

    expect(presentation.category).toBe('retained')
    expect(presentation.actions.map((action) => action.kind)).toEqual([
      'redownload',
      'delete',
    ])
    expect(presentation.usage?.ownedBytes).toBe(2_048n)
  })


})

function present(
  state: ReceiveLifecycleState,
  artifact: ArtifactSpec,
  planKind: MaterializationPlan['kind'] = 'workspace-then-publish',
  workspaceUsage?: { readonly ownedBytes: bigint; readonly maximumBytes?: bigint },
  recoverySummaryInput?: RecoverySummary,
) {
  return presentReceiveLifecycle({
    state,
    artifact,
    plan: planForTest(planKind),
    ...(workspaceUsage === undefined ? {} : { workspaceUsage }),
    ...(recoverySummaryInput === undefined ? {} : { recoverySummary: recoverySummaryInput }),
  })
}

function planForTest(kind: MaterializationPlan['kind']): MaterializationPlan {
  return { kind } as MaterializationPlan
}

function lifecycle(
  payload: TestLifecyclePayload,
): ReceiveLifecycleState {
  const selectionFacts = payload.kind === 'resumable-receive' && payload.payloadKind === 'file-set'
    ? payload.selectionFacts ?? Object.freeze({
        discoveredFileCount: payload.completedFileCount,
        discoveredBytes: payload.completedBytes,
        discovery: 'failed' as const,
      })
    : undefined
  return Object.freeze({
    ...payload,
    ...(selectionFacts === undefined ? {} : { selectionFacts }),
    operationId: 'operation',
    receiveIntentDigest: 'intent',
    generation: 1n,
  }) as ReceiveLifecycleState
}

function recoverySummary(
  override: Pick<RecoverySummary, 'discovery'>,
): RecoverySummary {
  return Object.freeze({
    lifecycleGeneration: 1n,
    checkpointSetDigest: 'checkpoint-set',
    discoveredFileCount: 5n,
    discoveredBytes: 3_584n,
    discovery: override.discovery,
    completedFileCount: 1n,
    retainedBytes: 1_024n,
    completedBytes: 1_024n,
    incompleteFileCount: 2n,
    verifiedPartialFileCount: 2n,
    verifiedPartialBytes: 512n,
    unstartedFileCount: 2n,
    unstartedBytes: 1_024n,
    preservingRemainingBytes: 2_048n,
    restartRemainingBytes: 2_560n,
    restartRedownloadBytes: 512n,
    maximumPreservingTemporaryBytes: 384n,
  })
}
