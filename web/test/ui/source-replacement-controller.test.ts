import { afterEach, describe, expect, it, vi } from 'vitest'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { EMPTY_TRANSFER_FAILURE_SUMMARY } from '../../src/transfer/outcome'
import type { V2RetainedReceiveOperation } from '../../src/ui/v2-receive-runtime'
import {
  FakeJoinedShare, FakeReceiveComposition, WORKSPACE_ENVIRONMENT, controllerFor,
  FILE_ID, deferred, identityText, next, resetOrchestrationTestEnvironment, startTransfer, turns, waitFor,
} from './v2-receiver-orchestration-fixture'

class ReplacementJoinedShare extends FakeJoinedShare {
  override selection = new V2SelectionPolicy(true)
  selectOnlyFile(entry: V2CatalogEntry, ancestry: readonly string[]): void {
    this.selection = new V2SelectionPolicy(false)
    this.selection.toggle(entry, ancestry)
  }
}

function retained(joined: FakeJoinedShare): V2RetainedReceiveOperation {
  return {
    operationId: identityText(90), receiveIntentDigest: identityText(91, 32), lifecycleGeneration: 2n,
    lifecycle: { kind: 'resumable-receive', payloadKind: 'opfs-zip',
      operationId: identityText(90), receiveIntentDigest: identityText(91, 32), generation: 2n,
      objectId: identityText(92, 32), checkpointGeneration: 3n, occupiedBytes: 128n,
      completedFileCount: 1n, completedBytes: 64n, discoveryComplete: true },
    continuation: 'resume-receive', actions: ['continue', 'discard'],
    sourceRevisionFailures: { shareInstance: joined.descriptor.shareInstanceId, count: 1n,
      files: [{ entryId: 'failed', path: ['folder', 'report.txt'], sourcePath: ['report.txt'] }] },
  }
}

afterEach(resetOrchestrationTestEnvironment)

describe('source revision replacement controller flow', () => {
  it('creates a separate selected-file task only after its download choice, preserving the original task', async () => {
    const joined = new ReplacementJoinedShare(true)
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const original = retained(joined)
    const before = structuredClone(original)
    receive.retainedOperations = [original]
    const controller = controllerFor(joined, receive)
    await waitFor(() => controller.getSnapshot().retained.operations.length === 1 &&
      controller.getSnapshot().output.offerPresentation?.kind === 'choices')
    controller.prepareReplacementDownload(original, original.sourceRevisionFailures!.files[0]!)
    await waitFor(() => joined.selection.defaultSelected === false &&
      controller.getSnapshot().output.offerPresentation?.kind === 'choices')
    await turns()
    expect(receive.startedAuthorities).toHaveLength(0)
    expect(receive.retainedActionCalls).toEqual([])
    expect(original).toEqual(before)
    const offers = controller.getSnapshot().output.offers
    if (offers?.kind !== 'artifact-actions') throw new Error('Expected replacement download choice')
    controller.chooseArtifact(offers.primary.choice.choiceId)
    await waitFor(() => joined.transferRuns.length === 1)
    expect(joined.transferRuns[0]?.intent.operationId).not.toBe(original.operationId)
    expect(joined.transferRuns[0]?.intent.selection.rules).toMatchObject({
      mode: 'node-id', defaultSelected: false,
    })
    expect(receive.retainedOperations).toEqual([before])
    expect(receive.retainedActionCalls).toEqual([])
    await controller.dispose()
  })

  it('retains a failed ZIP and admits another download of the same selection without navigation', async () => {
    const confirmation = deferred<void>()
    const joined = new FakeJoinedShare(true, [undefined, confirmation.promise], 'tree')
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]!.runtime!
    const loads = receive.retainedSignals.length
    const original = settleFailedZip(joined, receive)
    await waitFor(() => runtime.detachments.length === 1 && receive.retainedSignals.length > loads &&
      controller.getSnapshot().retained.operations.length === 1)
    expect(controller.getSnapshot().retained.operations[0]?.sourceRevisionFailures?.files[0]?.path)
      .toEqual(['folder', 'report.txt'])
    expect(controller.getSnapshot().output.receiveIntent).toBeNull()
    expect(controller.getSnapshot().output.offers?.kind).not.toBe('artifact-actions')
    controller.chooseArtifact(receive.startedChoices[0]!.choice.choiceId)
    expect(receive.startedAuthorities).toHaveLength(1)
    confirmation.resolve()
    await waitFor(() => controller.getSnapshot().startAdmission.allowed &&
      controller.getSnapshot().output.offers?.kind === 'artifact-actions')
    expect(receive.startedAuthorities).toHaveLength(1)
    const offers = controller.getSnapshot().output.offers!
    if (offers.kind !== 'artifact-actions') throw new Error('Expected another download choice')
    controller.chooseArtifact(offers.primary.choice.choiceId)
    await waitFor(() => joined.transferRuns.length === 2)
    expect(joined.transferRuns[1]!.intent.selection.digest).toBe(runtime.intent.selection.digest)
    expect(receive.startedAuthorities[1]!.runtime).not.toBe(runtime)
    expect(receive.retainedOperations).toEqual([original])
    expect(receive.retainedActionCalls).toEqual([])
    await controller.dispose()
  })

  it('waits for failed output cleanup before admitting the latest selection', async () => {
    const joined = new FakeJoinedShare(true, [], 'tree')
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const cleanup = deferred<void>()
    const detach = vi.spyOn(receive.startedAuthorities[0]!.runtime!, 'detach')
      .mockImplementation(() => cleanup.promise)
    settleFailedZip(joined, receive)
    await waitFor(() => detach.mock.calls.length === 1)
    controller.toggleSelection(FILE_ID)
    await waitFor(() => controller.getSnapshot().output.offers?.kind === 'artifact-actions')
    const offers = controller.getSnapshot().output.offers!
    if (offers.kind !== 'artifact-actions') throw new Error('Expected a selected-file choice')
    expect(controller.getSnapshot().startAdmission.allowed).toBe(false)
    controller.chooseArtifact(offers.primary.choice.choiceId)
    await turns()
    expect(receive.startedAuthorities).toHaveLength(1)
    cleanup.resolve()
    await waitFor(() => controller.getSnapshot().startAdmission.allowed &&
      controller.getSnapshot().output.offers?.kind === 'artifact-actions')
    const current = controller.getSnapshot().output.offers!
    if (current.kind !== 'artifact-actions') throw new Error('Expected the latest download choice')
    controller.chooseArtifact(current.primary.choice.choiceId)
    await waitFor(() => joined.transferRuns.length === 2)
    expect(joined.transferRuns[1]!.intent.selection.rules).toMatchObject({
      mode: 'node-id', defaultSelected: false,
    })
    expect(receive.retainedOperations).toHaveLength(1)
    await controller.dispose()
  })

  it('does not restart confirmation or reload inventory after disposal during failed output cleanup', async () => {
    const joined = new FakeJoinedShare(true, [], 'tree')
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const cleanup = deferred<void>()
    const detach = vi.spyOn(receive.startedAuthorities[0]!.runtime!, 'detach')
      .mockImplementation(() => cleanup.promise)
    settleFailedZip(joined, receive)
    await waitFor(() => detach.mock.calls.length === 1)
    await controller.dispose()
    const environmentCalls = receive.environmentCalls
    const inventoryLoads = receive.retainedSignals.length
    const snapshot = controller.getSnapshot()
    cleanup.resolve()
    await turns()
    expect(receive.environmentCalls).toBe(environmentCalls)
    expect(receive.retainedSignals).toHaveLength(inventoryLoads)
    expect(controller.getSnapshot()).toBe(snapshot)
  })
})

function settleFailedZip(joined: FakeJoinedShare, receive: FakeReceiveComposition): V2RetainedReceiveOperation {
  const runtime = receive.startedAuthorities[0]!.runtime!
  const lifecycle = next(runtime.lifecycle, {
    kind: 'resumable-receive', payloadKind: 'opfs-zip', objectId: identityText(92, 32),
    checkpointGeneration: 3n, occupiedBytes: 128n, completedFileCount: 1n,
    completedBytes: 64n, discoveryComplete: true,
  })
  const original = { ...retained(joined), operationId: runtime.intent.operationId,
    receiveIntentDigest: runtime.intent.digest, lifecycleGeneration: lifecycle.generation, lifecycle }
  receive.retainedOperations = [original]
  joined.transferRuns[0]!.resolve(lifecycle, undefined, {
    ...EMPTY_TRANSFER_FAILURE_SUMMARY, status: 'Paused', failureCount: 1, fileFailureCount: 1,
    omittedFailureCount: 1, fileOutcomes: { ...EMPTY_TRANSFER_FAILURE_SUMMARY.fileOutcomes, failedFiles: 1 },
  })
  return original
}
