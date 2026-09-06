import { afterEach, describe, expect, it } from 'vitest'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { V2CatalogEntry } from '../../src/catalog/v2-records'
import { EMPTY_TRANSFER_FAILURE_SUMMARY } from '../../src/transfer/outcome'
import type { V2RetainedReceiveOperation } from '../../src/ui/v2-receive-runtime'
import {
  FakeJoinedShare, FakeReceiveComposition, WORKSPACE_ENVIRONMENT, controllerFor,
  identityText, next, resetOrchestrationTestEnvironment, startTransfer, turns, waitFor,
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

  it('releases a settled failed native ZIP before reloading its affected paths in the same page', async () => {
    const joined = new FakeJoinedShare(true, [], 'tree')
    const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
    const controller = controllerFor(joined, receive)
    await startTransfer(controller, joined)
    const runtime = receive.startedAuthorities[0]!.runtime!
    const original = retained(joined)
    const lifecycle = next(runtime.lifecycle, {
      kind: 'resumable-receive', payloadKind: 'opfs-zip', objectId: identityText(92, 32),
      checkpointGeneration: 3n, occupiedBytes: 128n, completedFileCount: 1n,
      completedBytes: 64n, discoveryComplete: true,
    })
    receive.retainedOperations = [{ ...original, operationId: runtime.intent.operationId,
      receiveIntentDigest: runtime.intent.digest, lifecycleGeneration: lifecycle.generation, lifecycle }]
    const loads = receive.retainedSignals.length
    joined.transferRuns[0]!.resolve(lifecycle, undefined, {
      ...EMPTY_TRANSFER_FAILURE_SUMMARY, status: 'Paused', failureCount: 1, fileFailureCount: 1,
      omittedFailureCount: 1, fileOutcomes: { ...EMPTY_TRANSFER_FAILURE_SUMMARY.fileOutcomes, failedFiles: 1 },
    })
    await waitFor(() => runtime.detachments.length === 1 && receive.retainedSignals.length > loads &&
      controller.getSnapshot().retained.operations.length === 1)
    expect(controller.getSnapshot().retained.operations[0]?.sourceRevisionFailures?.files[0]?.path)
      .toEqual(['folder', 'report.txt'])
    expect(controller.getSnapshot().output.receiveIntent).toBeNull()
    expect(receive.retainedActionCalls).toEqual([])
    await controller.dispose()
  })
})
