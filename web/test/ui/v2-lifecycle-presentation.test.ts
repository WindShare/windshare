import { describe, expect, it } from 'vitest'

import type { CompatibleNameRepairSummary } from '../../src/output/file-system-access/compatible-name/model'
import type {
  ReceiveLifecycleState,
  ReceiveLifecycleStatePayload,
} from '../../src/output/workspace'
import type { ArtifactSpec, MaterializationPlan } from '../../src/transfer/intent'
import {
  presentNewReceiveOperation,
  presentReceiveLifecycle,
} from '../../src/ui/v2-lifecycle-presentation'
import type { V2DirectZipProgressSnapshot } from '../../src/ui/v2-receive-runtime'

const NOW = 1_000_000
const DEADLINE = NOW + 86_400_000
const ORIGINAL = { kind: 'original-file', suggestedName: 'report.txt' } as ArtifactSpec
const ZIP = { kind: 'zip-archive', suggestedName: 'photos.zip' } as ArtifactSpec
const TREE = {
  kind: 'directory-tree',
  layout: { kind: 'result-root', root: { name: 'photos' } },
} as ArtifactSpec

describe('receive lifecycle product phases', () => {
  it.each([
    [lifecycle({ kind: 'preparing', preparationId: 'preparation' }), ZIP, 'Checking selected content'],
    [lifecycle({ kind: 'receiving', activeLeaseId: 'lease' }), ZIP, 'Receiving files'],
    [lifecycle({ kind: 'packaging', activeLeaseId: 'lease', sealedMaterializationDigest: 'seal', packageTempObjectId: 'temporary' }), ZIP, 'Generating ZIP'],
    [lifecycle({ kind: 'waiting-to-save', packageDigest: 'package', expiresAt: DEADLINE }), ZIP, 'Ready to save'],
    [lifecycle({ kind: 'publishing-managed', activeLeaseId: 'lease', packageDigest: 'package', publicationAttemptId: 'attempt' }), ZIP, 'Saving photos.zip'],
    [lifecycle({ kind: 'handing-off', activeLeaseId: 'lease', attemptKind: 'workspace', attemptId: 'attempt', packageDigest: 'package', retainedDeadline: DEADLINE }), ZIP, 'Starting browser download'],
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
      lifecycle({ kind: 'waiting-to-save', packageDigest: 'package', expiresAt: DEADLINE }),
      lifecycle({ kind: 'publishing-managed', activeLeaseId: 'lease', packageDigest: 'package', publicationAttemptId: 'attempt' }),
      lifecycle({ kind: 'handing-off', activeLeaseId: 'lease', attemptKind: 'workspace', attemptId: 'attempt', packageDigest: 'package', retainedDeadline: DEADLINE }),
    ]
    const titles = states.map((state) => present(state, ZIP).title)

    expect(new Set(titles).size).toBe(states.length)
    expect(present(states[1]!, ZIP).description).toContain('without compression')
  })
})

describe('receive lifecycle terminal presentation', () => {
  it.each([
    [lifecycle({ kind: 'published', receiptDigest: 'receipt', cleanupState: 'clean' }), 'Saved', 'terminal'],
    [lifecycle({ kind: 'download-started', attemptKind: 'portable', attemptId: 'attempt' }), 'Download started', 'terminal'],
    [lifecycle({ kind: 'partial-directory', reason: 'failures', successCount: 2n, failureCount: 1n, receiptDigest: 'receipt' }), 'Some files were saved', 'terminal'],
    [lifecycle({ kind: 'restart-required', reason: 'portable-aborted', receiptDigest: 'receipt' }), 'Start again required', 'terminal'],
    [lifecycle({ kind: 'discarded', cleanupReceiptDigest: 'cleanup' }), 'Task discarded', 'terminal'],
    [lifecycle({ kind: 'expired', priorStableState: 'waiting-to-save', expiresAt: DEADLINE, cleanupState: 'clean', expiryReceiptDigest: 'expiry' }), 'Task expired', 'terminal'],
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
    expect(presentation.retention).toBeNull()
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

    expect(presentation.description).toContain('3 file(s) remain saved')
    expect(presentation.actions).toEqual([])
  })
})

describe('direct ZIP lifecycle presentation', () => {
  it('uses the bound filename and keeps the target explicitly incomplete through close and verification', () => {
    const receiving = present(
      lifecycle({ kind: 'receiving', activeLeaseId: 'lease' }),
      ZIP,
      'direct-resumable-zip',
    )
    const closing = present(
      lifecycle({ kind: 'receiving', activeLeaseId: 'lease' }),
      ZIP,
      'direct-resumable-zip',
      NOW,
      undefined,
      undefined,
      directZipProgress('closing'),
    )
    const verifying = present(
      lifecycle({ kind: 'receiving', activeLeaseId: 'lease' }),
      ZIP,
      'direct-resumable-zip',
      NOW,
      undefined,
      undefined,
      directZipProgress('verifying'),
    )

    expect(receiving.description).toContain('photos.windshare-abc234.zip')
    expect(receiving.description).toContain('incomplete target')
    expect(closing).toMatchObject({ title: 'Closing photos.windshare-abc234.zip' })
    expect(closing.description).toContain('still incomplete')
    expect(verifying.description).toContain('photos.windshare-abc234.zip')
  })

  it('separates resumable, authorization, verification, space, restart, and attention states', () => {
    const resumable = present(lifecycle({
      kind: 'resumable-receive',
      payloadKind: 'direct-zip',
      directZipCheckpointDigest: 'checkpoint',
      safeSelectedPayloadBytes: 1_024n,
      committedArchiveLength: 2_048n,
      checkpointPhase: 'between-members',
      expiresAt: DEADLINE,
    }), ZIP, 'direct-resumable-zip')
    const gate = (kind: 'authorization-required' | 'target-verification-required' |
      'destination-space-required') => present(lifecycle({
        kind,
        recoveryGateDigest: `${kind}-digest`,
        expiresAt: DEADLINE,
      }), ZIP, 'direct-resumable-zip')
    const restart = present(lifecycle({
      kind: 'restart-required',
      reason: 'target-deleted',
      receiptDigest: 'receipt',
    }), ZIP, 'direct-resumable-zip')
    const attention = present(lifecycle({
      kind: 'needs-attention',
      reason: 'target-ownership-unknown',
      lastVerifiedRecordDigest: 'record',
    }), ZIP, 'direct-resumable-zip')

    expect(resumable.description).toContain('1.0 KiB can be resumed safely')
    expect(resumable.actions.map(action => action.label)).toEqual([
      'Continue ZIP',
      'Delete only after ownership is verified',
    ])
    expect(gate('authorization-required')).toMatchObject({ title: 'Save authorization required' })
    expect(gate('target-verification-required')).toMatchObject({ title: 'Saved target must be verified' })
    expect(gate('destination-space-required')).toMatchObject({ title: 'More destination space is required' })
    expect(restart.description).toContain('will not recreate it under the old operation')
    expect(attention.description).toContain('No automatic change was made')
  })

  it('offers a new-operation route only when the retained ZIP target was deleted', () => {
    const deleted = presentNewReceiveOperation({
      lifecycle: lifecycle({
        kind: 'restart-required',
        reason: 'target-deleted',
        receiptDigest: 'receipt',
      }),
      plan: planForTest('direct-resumable-zip'),
    })
    const changed = presentNewReceiveOperation({
      lifecycle: lifecycle({
        kind: 'restart-required',
        reason: 'source-revision-changed',
        receiptDigest: 'receipt',
      }),
      plan: planForTest('direct-resumable-zip'),
    })

    expect(deleted).toMatchObject({
      kind: 'deleted-direct-zip-target',
      title: 'Choose a new save route',
      actionLabel: 'Choose a route for a new operation',
    })
    expect(deleted?.description).toContain('will not be recreated')
    expect(changed).toBeNull()
  })
})

describe('compatible-name repair presentation', () => {
  it('keeps the first replacement notice non-blocking and labels active or paused use as abnormal-stop recovery', () => {
    const summary = repairSummary('active', true, 3)
    const active = present(
      lifecycle({ kind: 'receiving', activeLeaseId: 'lease' }),
      TREE,
      'direct-tree',
      NOW,
      undefined,
      summary,
    )
    const paused = present(lifecycle({
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: 'checkpoints',
      completedFileCount: 1n,
      completedBytes: 128n,
      expiresAt: DEADLINE,
    }), TREE, 'direct-tree', NOW, undefined, summary)

    expect(active.compatibleNameRepair).toMatchObject({
      noticeTitle: 'Compatible names are in use',
      replacementCount: 3,
      replacementCountLabel: '3 verified/committed name replacements',
      logicalPathSample: ['folder/pyvenv.cfg', 'folder/nested'],
      omittedLogicalPathCount: 1,
      scriptName: 'restore-names.windshare-abc234.ps1',
      sidecarName: 'restore-names.windshare-abc234.tsv',
      runCommand: 'powershell.exe -NoProfile -ExecutionPolicy Bypass -File ".\\restore-names.windshare-abc234.ps1"',
      actionMode: 'abnormal-stop-recovery',
    })
    expect(paused.compatibleNameRepair?.actionMode).toBe('abnormal-stop-recovery')
    expect(paused.compatibleNameRepair?.actionDescription).toMatch(/do not run.*resumable/iu)
    expect(active.compatibleNameRepair?.noticeDescription).toContain('remain compatible')

    const createdTarget = present(
      lifecycle({ kind: 'receiving', activeLeaseId: 'lease' }),
      TREE,
      'direct-tree',
      NOW,
      undefined,
      repairSummary('active', false, 0),
    )
    expect(createdTarget.compatibleNameRepair).toMatchObject({
      replacementCount: 0,
      replacementCountLabel: '0 verified/committed name replacements',
      logicalPathSample: [],
      actionMode: 'abnormal-stop-recovery',
    })
  })

  it.each([
    ['published', lifecycle({ kind: 'published', receiptDigest: 'receipt', cleanupState: 'clean' }),
      'completed', 'Completed with compatible names'],
    ['partial-directory', lifecycle({
      kind: 'partial-directory',
      reason: 'stopped',
      successCount: 2n,
      failureCount: 1n,
      receiptDigest: 'receipt',
    }), 'stopped', 'Partial with compatible names'],
  ] as const)('qualifies a terminal %s only after its complete terminal footer', (
    _kind,
    state,
    footerState,
    title,
  ) => {
    const presentation = present(
      state,
      TREE,
      'direct-tree',
      NOW,
      undefined,
      repairSummary(footerState, false, 2),
    )

    expect(presentation).toMatchObject({
      stateKind: state.kind,
      category: 'terminal',
      title,
      compatibleNameRepair: {
        actionMode: 'routine-restoration',
        actionTitle: 'Restore the original names',
      },
    })
    expect(presentation.description).toContain('compatible names')
    expect(presentation.description).not.toMatch(/names (?:are|were) restored/iu)
  })

  it('withholds qualified terminal success while sidecar catch-up is pending', () => {
    const presentation = present(
      lifecycle({ kind: 'published', receiptDigest: 'receipt', cleanupState: 'clean' }),
      TREE,
      'direct-tree',
      NOW,
      undefined,
      repairSummary('completed', true, 2),
    )

    expect(presentation.title).toBe('Restoration tool catch-up required')
    expect(presentation.tone).toBe('warning')
    expect(presentation.compatibleNameRepair?.actionMode).toBe('catch-up-required')
    expect(presentation.compatibleNameRepair?.runCommand).toBeNull()
    expect(presentation.description).not.toMatch(/complete result was saved/iu)
  })
})

describe('retention, usage, and lifecycle-valid actions', () => {
  it('shows the exact stable deadline and workspace usage only for retained workspace truth', () => {
    const state = lifecycle({ kind: 'waiting-to-save', packageDigest: 'package', expiresAt: DEADLINE })
    const presentation = present(state, ZIP, 'workspace-then-publish', NOW, {
      ownedBytes: 1_024n,
      maximumBytes: 4_096n,
    })

    expect(presentation.retention).toEqual({
      expiresAt: DEADLINE,
      remainingMilliseconds: 86_400_000,
      elapsed: false,
    })
    expect(presentation.usage).toMatchObject({
      ownedBytes: 1_024n,
      maximumBytes: 4_096n,
      label: '1.0 KiB of 4.0 KiB used by this task',
    })
    expect(presentation.actions.map((action) => action.kind)).toEqual(['save', 'delete'])

    expect(present(state, ZIP, 'direct-atomic', NOW, { ownedBytes: 1_024n }).usage).toBeNull()
  })

  it('suppresses continuation immediately when a stable deadline has elapsed', () => {
    const state = lifecycle({
      kind: 'resumable-receive',
      payloadKind: 'file-set',
      checkpointSetDigest: 'checkpoints',
      completedFileCount: 2n,
      completedBytes: 512n,
      expiresAt: DEADLINE,
    })
    const before = present(state, TREE, 'workspace-then-publish', DEADLINE - 1)
    const elapsed = present(state, TREE, 'workspace-then-publish', DEADLINE)

    expect(before.actions.map((action) => action.kind)).toEqual(['continue', 'discard'])
    expect(elapsed.retention?.elapsed).toBe(true)
    expect(elapsed.actions).toEqual([])
    expect(elapsed.description).toContain('can no longer continue')
  })

  it('preserves the original workspace deadline without inventing a managed location action', () => {
    const state = lifecycle({
      kind: 'download-started',
      attemptKind: 'workspace',
      attemptId: 'attempt',
      packageDigest: 'package',
      retryableUntil: DEADLINE,
    })
    const presentation = present(state, ZIP, 'workspace-then-publish', NOW, {
      ownedBytes: 2_048n,
    })

    expect(presentation.category).toBe('retained')
    expect(presentation.retention?.expiresAt).toBe(DEADLINE)
    expect(presentation.actions.map((action) => action.kind)).toEqual([
      'redownload',
      'delete',
    ])
    expect(presentation.usage?.ownedBytes).toBe(2_048n)
  })

  it('hides reclaimed usage and continuation after expiry cleanup', () => {
    const clean = present(lifecycle({
      kind: 'expired',
      priorStableState: 'resumable-package',
      expiresAt: DEADLINE,
      cleanupState: 'clean',
      expiryReceiptDigest: 'expiry',
    }), ZIP, 'workspace-then-publish', DEADLINE, { ownedBytes: 0n })
    const pending = present(lifecycle({
      kind: 'expired',
      priorStableState: 'resumable-package',
      expiresAt: DEADLINE,
      cleanupState: 'cleanup-pending',
      expiryReceiptDigest: 'expiry',
    }), ZIP, 'workspace-then-publish', DEADLINE, { ownedBytes: 2_048n })

    expect(clean.usage).toBeNull()
    expect(clean.actions).toEqual([])
    expect(pending.usage?.ownedBytes).toBe(2_048n)
    expect(pending.actions.map((action) => action.kind)).toEqual(['delete'])
  })
})

function present(
  state: ReceiveLifecycleState,
  artifact: ArtifactSpec,
  planKind: MaterializationPlan['kind'] = 'workspace-then-publish',
  nowMilliseconds = NOW,
  workspaceUsage?: { readonly ownedBytes: bigint; readonly maximumBytes?: bigint },
  repairSummary?: CompatibleNameRepairSummary,
  directZipProgress?: V2DirectZipProgressSnapshot,
) {
  return presentReceiveLifecycle({
    state,
    artifact,
    plan: planForTest(planKind),
    nowMilliseconds,
    ...(workspaceUsage === undefined ? {} : { workspaceUsage }),
    ...(repairSummary === undefined ? {} : { repairSummary }),
    ...(directZipProgress === undefined ? {} : { directZipProgress }),
  })
}

function directZipProgress(
  phase: V2DirectZipProgressSnapshot['phase'],
): V2DirectZipProgressSnapshot {
  return Object.freeze({
    kind: 'direct-zip',
    operationId: 'operation',
    generation: 1n,
    phase,
    receivedSelectedBytes: 1_024n,
    safeResumeBytes: 768n,
  })
}

function planForTest(kind: MaterializationPlan['kind']): MaterializationPlan {
  if (kind === 'direct-resumable-zip') {
    return {
      kind,
      binding: { stableName: 'photos.windshare-abc234.zip' },
    } as MaterializationPlan
  }
  return { kind } as MaterializationPlan
}

function lifecycle(
  payload: ReceiveLifecycleStatePayload,
): ReceiveLifecycleState {
  return Object.freeze({
    ...payload,
    operationId: 'operation',
    receiveIntentDigest: 'intent',
    generation: 1n,
  }) as ReceiveLifecycleState
}

function repairSummary(
  footerState: NonNullable<CompatibleNameRepairSummary['latestObservedFooter']>['state'],
  pendingCatchUp: boolean,
  committedCount: number,
): CompatibleNameRepairSummary {
  return Object.freeze({
    committedCount,
    logicalPathSample: Object.freeze([
      Object.freeze(['folder', 'pyvenv.cfg']),
      Object.freeze(['folder', 'nested']),
    ].slice(0, committedCount)),
    pairDisplayNames: Object.freeze({
      script: 'restore-names.windshare-abc234.ps1',
      sidecar: 'restore-names.windshare-abc234.tsv',
    }),
    placement: 'inside-logical-root',
    runCommand: 'powershell.exe -NoProfile -ExecutionPolicy Bypass -File ".\\restore-names.windshare-abc234.ps1"',
    latestObservedFooter: Object.freeze({ committedCount, state: footerState }),
    pendingCatchUp,
  })
}
