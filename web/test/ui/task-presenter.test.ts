import { describe, expect, it } from 'vitest'
import { activeTaskFacts, compareTaskTransition, presentTask, retainedTaskFacts } from '../../src/ui/tasks'
import { taskFixture, TASK_FIXTURES } from '../../src/ui/tasks/fixtures'
import { EMPTY_V2_OUTPUT_PRESENTATION } from '../../src/ui/v2-output'
import { EMPTY_V2_PROGRESS } from '../../src/ui/v2-model'
import type { V2RetainedReceiveOperation } from '../../src/ui/v2-receive-runtime'
import type { ReceiveLifecycleState } from '../../src/output/workspace'
import type { TaskAction } from '../../src/ui/tasks'

function retained(actions: V2RetainedReceiveOperation['actions'] = ['continue', 'save-partial', 'discard']): V2RetainedReceiveOperation {
  const state: ReceiveLifecycleState = {
    operationId: '1'.repeat(32), receiveIntentDigest: '2'.repeat(64), generation: 4n,
    kind: 'resumable-receive', payloadKind: 'opfs-zip',
    objectId: '3'.repeat(32), checkpointGeneration: 3n, occupiedBytes: 4096n,
    completedFileCount: 2n, completedBytes: 1024n, discoveryComplete: false,
  }
  return Object.freeze({
    operationId: state.operationId, receiveIntentDigest: state.receiveIntentDigest,
    lifecycleGeneration: state.generation, lifecycle: state,
    continuation: 'resume-receive', actions,
    display: Object.freeze({ objectLabel: 'Original selection', createdAtMilliseconds: 1000 }),
  })
}

describe('shared task presenter', () => {
  it('keeps open or failed discovery indeterminate and reports completed files separately', () => {
    const open = presentTask(TASK_FIXTURES['open-discovery']!)
    expect(open.progress).toMatchObject({ mode: 'indeterminate', percentage: null })
    expect(open.progress?.label).toContain('3 files completed')
    expect(open.progress?.status).toContain('Calculating total')
    expect(open.progress?.remainingBytes).toBeNull()
    expect(open.progress?.details.join(' ')).toContain('final total unknown')
    expect(presentTask(taskFixture({
      progress: { ...EMPTY_V2_PROGRESS, discovery: 'failed', discoveredBytes: 100n, writtenBytes: 50n },
    })).progress?.percentage).toBeNull()
  })

  it('requires exact closed discovery and never presents receipt as publication', () => {
    const result = presentTask(taskFixture({
      progress: { ...EMPTY_V2_PROGRESS, discovery: 'complete', discoveredBytes: 100n, writtenBytes: 100n, materializedBytes: 100n },
    }))
    expect(result.progress).toMatchObject({ mode: 'determinate', percentage: 99 })
    expect(result.stage).toBe('downloading')
    expect(result.publication).toBe('unpublished')
  })

  it('labels nonoverlapping local materialization separately from this attempt receipt', () => {
    const result = presentTask(taskFixture({
      progress: { ...EMPTY_V2_PROGRESS, discovery: 'complete', discoveredBytes: 100n,
        materializedBytes: 91n, writtenBytes: 1n, completedBytes: 80n },
    }))
    expect(result.progress?.percentage).toBe(91)
    expect(result.progress?.label).toContain('91 B / 100 B written or reused · 91%')
    expect(result.progress?.details).toContain('1 B newly received during this attempt.')
  })

  it('keeps direct ZIP verification ahead of disconnected sender and generic finishing', () => {
    const result = presentTask(taskFixture({
      ...TASK_FIXTURES.verifying!, blocking: { kind: 'share-ended' },
      progress: { ...EMPTY_V2_PROGRESS, phase: 'finishing' },
    }))
    expect(result.headline).toBe('Verifying ZIP')
  })

  it('keeps direct ZIP receipt and restart-safe bytes independent', () => {
    const result = presentTask(TASK_FIXTURES.verifying!)
    expect(result.stage).toBe('finishing')
    expect(result.headline).toBe('Verifying ZIP')
    expect(result.progress?.label).toContain('1.9 MiB received')
    expect(result.progress?.details.join(' ')).toContain('1.4 MiB safe to resume')
  })

  it('does not let connection interruption or cleanup replace a local saved result', () => {
    const result = presentTask(taskFixture({
      ...TASK_FIXTURES['saved-cleanup']!,
      blocking: { kind: 'share-ended' },
    }))
    expect(result.headline).toBe('Saved')
    expect(result.details.join(' ')).toContain('Cleanup')
    expect(presentTask(TASK_FIXTURES['partial-ready']!).headline).toBe('Ready to save')
    expect(presentTask(TASK_FIXTURES['partial-ready']!).completeness).toBe('partial')
    expect(presentTask(TASK_FIXTURES['browser-handoff']!).headline).toBe('Download started — check browser downloads')
  })

  it('never synthesizes partial export or destructive primary actions', () => {
    const completeOnly = retained(['continue', 'discard'])
    const result = presentTask(retainedTaskFacts(completeOnly, 'matching-share'))
    expect(result.primaryAction?.id).toBe('continue')
    expect(result.secondaryActions).toEqual([])
    const partial = presentTask(retainedTaskFacts(retained(), 'matching-share'))
    expect(partial.secondaryActions.find(action => action.id === 'save-partial')?.consequence).toContain('retained task remains available')
    const destructiveOnly = presentTask(retainedTaskFacts(retained(['discard']), 'local'))
    expect(destructiveOnly.primaryAction).toBeNull()
  })

  it('preserves inventory token identity and disables remote continuation without original credentials', () => {
    const operation = retained()
    const result = presentTask(retainedTaskFacts(operation, 'original-link-required'))
    expect(result.stage).toBe('needs-action')
    const continuation = [...result.secondaryActions, result.primaryAction!].find(action => action.id === 'continue')!
    expect(continuation.disabledReason).toContain('original share link')
    expect(continuation.target).toMatchObject({ kind: 'retained', action: 'continue' })
    if (continuation.target.kind === 'retained') expect(continuation.target.operation).toBe(operation)
    expect(result.primaryAction?.id).toBe('save-partial')
    expect(result.objectLabel).toBe('Original selection')
  })

  it('keeps history removal separate from unfinished output deletion', () => {
    const history = retained(['forget'])
    const result = presentTask(retainedTaskFacts(history, 'local'))
    expect(result.primaryAction).toBeNull()
    const action = result.secondaryActions[0]!
    expect(action.label).toBe('Remove from Downloads')
    expect(action.destructive).toBe(false)
    expect(action.consequence).toContain('remain untouched')
    const deletion = presentTask(retainedTaskFacts(retained(['delete']), 'local')).destructiveActions[0]!
    expect(deletion.consequence).toContain('deleting retained output')
  })

  it('suppresses an unfinished progress indicator after save or handoff', () => {
    expect(presentTask(TASK_FIXTURES['saved-cleanup']!).progress).toBeNull()
    expect(presentTask(TASK_FIXTURES['browser-handoff']!).progress).toBeNull()
    expect(presentTask(TASK_FIXTURES['partial-ready']!).details.join(' ')).toContain('received')
  })

  it('does not describe an active checkpoint as final ZIP completion', () => {
    const result = presentTask(taskFixture({
      directZipProgress: { ...TASK_FIXTURES.verifying!.directZipProgress!, phase: 'saving-resume-position' },
    }))
    expect(result.stage).toBe('downloading')
    expect(result.headline).toBe('Saving resume progress')
  })

  it('keeps local packaging actionable without a joined share', () => {
    const operation: V2RetainedReceiveOperation = {
      ...retained(['continue']),
      continuation: 'resume-package',
      lifecycle: {
        ...retained().lifecycle, kind: 'resumable-package',
        sealedMaterializationDigest: '4'.repeat(64), tempCleanupProofDigest: '5'.repeat(64),
      },
    }
    const result = presentTask(retainedTaskFacts(operation))
    expect(result.stage).toBe('paused')
    expect(result.primaryAction).toMatchObject({ label: 'Finish and save', disabledReason: null })
    expect(result.description).toContain('without the sender')
    expect(result.completeness).toBe('complete')
  })

  it('shows source-authorized pending local work as Finishing without implying a remote transfer', () => {
    const operation: V2RetainedReceiveOperation = { ...retained(['continue']), continuation: 'resume-local-finalization' }
    const idle = presentTask(retainedTaskFacts(operation, 'local'))
    const running = presentTask(retainedTaskFacts(operation, 'local', {
      pending: { operationId: operation.operationId, action: 'continue' },
    }))
    expect(idle.stage).toBe('paused')
    expect(running.headline).toBe('Finishing locally')
    expect(running.primaryAction).toBeNull()
    expect(running.description).toContain('without reconnecting')
    expect(presentTask(retainedTaskFacts(operation, 'local', {
      pending: { operationId: 'another-operation', action: 'continue' },
    })).stage).toBe('paused')
    expect(presentTask(retainedTaskFacts(retained(['continue']), 'matching-share', {
      pending: { operationId: operation.operationId, action: 'continue' },
    })).headline).not.toBe('Finishing locally')
  })

  it('uses runtime admission per retained action without blocking independent local export', () => {
    const result = presentTask(retainedTaskFacts(retained(), 'matching-share', {
      admission: action => action === 'continue'
        ? { allowed: false, reason: 'Pause the current download before continuing this one.' }
        : { allowed: true, reason: null },
    }))
    expect(result.primaryAction?.id).toBe('save-partial')
    expect(result.secondaryActions.find(action => action.id === 'continue')?.disabledReason).toContain('Pause the current download')
  })

  it('treats crash inventory as inactive while preserving its durable receive and packaging phases', () => {
    const receiving: V2RetainedReceiveOperation = { ...retained(['continue']), lifecycle: taskFixture().lifecycle }
    const remote = presentTask(retainedTaskFacts(receiving, 'original-link-required'))
    expect(remote.stage).toBe('needs-action')
    expect(remote.headline).toContain('original link')
    expect(receiving.lifecycle.kind).toBe('receiving')
    expect(presentTask(retainedTaskFacts(receiving, 'matching-share')).stage).toBe('paused')
    const packaging: V2RetainedReceiveOperation = { ...receiving, continuation: 'resume-package' }
    expect(presentTask(retainedTaskFacts(packaging, 'local')).headline).toBe('Ready to finish locally')
    const handoff: V2RetainedReceiveOperation = { ...receiving, continuation: 'retry-download' }
    expect(presentTask(retainedTaskFacts(handoff, 'local')).headline).toBe('Ready to save')
    const sealed: V2RetainedReceiveOperation = { ...receiving, continuation: 'save-artifact', actions: ['save'] }
    expect(presentTask(retainedTaskFacts(sealed, 'local')).primaryAction?.id).toBe('save')
  })

  it('keeps failed reconnect distinct from a confirmed ended share', () => {
    const result = presentTask(taskFixture({ blocking: { kind: 'unavailable' } }))
    expect(result.headline).toBe('Connection unavailable')
    expect(result.description).not.toContain('share has ended')
  })

  it('blocks ended-share remote continuation while keeping local finalization and Save available', () => {
    const operation = retained(['continue', 'save-partial'])
    const result = presentTask(retainedTaskFacts(operation, 'matching-share', {
      blocking: { kind: 'share-ended' },
      admission: action => action === 'continue'
        ? { allowed: false, reason: 'The share ended. Open a new link to continue.' }
        : { allowed: true, reason: null },
    }))
    expect(result.stage).toBe('needs-action')
    expect(result.headline).toBe('The share has ended')
    expect(result.primaryAction?.id).toBe('save-partial')
    expect(result.secondaryActions.find(action => action.id === 'continue')?.disabledReason).toContain('new link')
    const local = presentTask(retainedTaskFacts({ ...operation, continuation: 'resume-package' }, 'local', {
      blocking: { kind: 'share-ended' },
    }))
    expect(local.headline).toBe('Ready to finish locally')
    expect(local.primaryAction?.disabledReason).toBeNull()
  })

  it('does not create a task before lifecycle commitment', () => {
    expect(activeTaskFacts({ output: EMPTY_V2_OUTPUT_PRESENTATION, progress: EMPTY_V2_PROGRESS })).toBeNull()
  })

  it('uses immutable display facts and identical lifecycle presentation for current and retained adapters', () => {
    const operation = retained(['continue'])
    const display = operation.display!
    const active = activeTaskFacts({
      output: { ...EMPTY_V2_OUTPUT_PRESENTATION, lifecycle: operation.lifecycle },
      progress: EMPTY_V2_PROGRESS, display,
    })!
    const restored = retainedTaskFacts(operation, 'matching-share')
    expect(presentTask(active).headline).toBe(presentTask(restored).headline)
    expect(presentTask(active).objectLabel).toBe('Original selection')
    expect(active.display).toBe(display)
  })

  it('deduplicates byte and checkpoint updates but emits blocking and publication transitions', () => {
    const initial = presentTask(taskFixture()).transition
    const bytes = presentTask(taskFixture({
      lifecycle: { ...taskFixture().lifecycle, generation: 2n },
      progress: { ...EMPTY_V2_PROGRESS, writtenBytes: 999n },
    })).transition
    expect(compareTaskTransition(initial, bytes)).toBeNull()
    expect(compareTaskTransition(initial, presentTask(TASK_FIXTURES.reconnecting!).transition)).not.toBeNull()
    expect(compareTaskTransition(initial, presentTask(TASK_FIXTURES['browser-handoff']!).transition)).not.toBeNull()
  })

  it('keeps filename restoration pending prominent without losing a saved headline', () => {
    const action: TaskAction = {
      id: 'catch-up', label: 'Finish setup', destructive: false, disabledReason: null, consequence: null,
      target: { kind: 'retained', operation: retained(), action: 'catch-up' },
    }
    const result = presentTask(taskFixture({
      ...TASK_FIXTURES['saved-cleanup']!,
      actions: [action],
      fidelity: {
        noticeTitle: '', noticeDescription: '', replacementCount: 1, replacementCountLabel: '',
        logicalPathSample: [], omittedLogicalPathCount: 0, scriptName: 'restore.ps1', sidecarName: 'names.json',
        placementLabel: '', runCommand: null, shortCommand: null, visibility: 'primary',
        actionMode: 'catch-up-required', actionTitle: '', actionDescription: '',
      },
    }))
    expect(result.headline).toBe('Saved')
    expect(result.attention).toBe(true)
    expect(result.primaryAction?.id).toBe('catch-up')
    expect(result.details.join(' ')).toContain('1 filename was adjusted')
  })
})
