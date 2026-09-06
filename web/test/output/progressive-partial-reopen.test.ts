import { describe, expect, it, vi } from 'vitest'
import { reopenProgressiveZipContinuation } from '../../src/output/resume/reopen/progressive-continuation'
import type { WorkspaceContinuationInput, WorkspaceContinuationAuthorityOptions } from '../../src/output/resume/reopen/workspace-continuation-authority'
import type { WorkspaceOperationStages } from '../../src/output/workspace/stages'
import type { TaskCheckpointStore } from '../../src/output/origin-private/task-checkpoint/store'
import type { ReopenResources } from '../../src/output/resume/reopen/model'

const state = vi.hoisted(() => {
  const object = { operationId: 'operation', objectId: 'archive', handleId: 'handle', kind: 'zip-archive' }
  const checkpoint = { object, generation: 2n, allocatedLength: 0n, physicalLength: 0n,
    entryCount: 0n, discoveryComplete: false, selectedPaths: [], artifactState: 'receiving' }
  return { object, checkpoint, storeClose: vi.fn(), budget: vi.fn(), backend: vi.fn(),
    reader: { object, close: vi.fn(), completeEntries: vi.fn(), handle: {} },
  }
})
vi.mock('../../src/output/origin-private/progressive-backend', () => ({
  progressiveZipObjectRef: async () => state.object,
  openOriginPrivateProgressiveZipBackend: state.backend,
}))
vi.mock('../../src/output/origin-private/admission', () => ({
  OriginPrivateWorkspaceBudgetAuthority: { open: state.budget },
}))
vi.mock('../../src/output/origin-private/task-checkpoint/indexeddb-store', () => ({
  IndexedDbTaskCheckpointStore: { open: async () => ({
    readCheckpoint: async () => state.checkpoint, readEntries: async () => [],
    close: state.storeClose,
  } as unknown as TaskCheckpointStore) },
}))
vi.mock('../../src/output/resume/reopen/partial-zip-continuation', () => ({
  openRetainedZipPartialReader: async () => state.reader,
}))

describe('quota-paused partial ZIP reopen', () => {
  it('opens only the retained reader without writer, reservation, or lifecycle transition', async () => {
    state.budget.mockRejectedValue(new DOMException('No quota headroom', 'QuotaExceededError'))
    state.backend.mockRejectedValue(new Error('A native writer must never open for a partial read'))
    const resources: ReopenResources = {}
    const lifecycle = { kind: 'resumable-receive', payloadKind: 'opfs-zip', objectId: 'archive',
      checkpointGeneration: 2n, generation: 8n }
    const input = {
      repository: { readHandle: async () => ({ operationId: 'operation', ownedObjectId: 'archive' }) },
      snapshot: { lifecycle, operation: { receiveIntent: {
        operationId: 'operation', selection: { rules: { mode: 'node-id' } },
      } } }, resources,
    } as unknown as WorkspaceContinuationInput
    const admit = vi.fn()
    const result = await reopenProgressiveZipContinuation(input, {} as WorkspaceContinuationAuthorityOptions,
      { progressive: { admit } } as unknown as WorkspaceOperationStages, false, true)
    expect(result.lifecycle).toBe(lifecycle)
    expect(result.partialContinuation).toBe(state.reader)
    expect(resources.partialReader).toBe(state.reader)
    expect(state.checkpoint.generation).toBe(2n)
    expect(state.budget).not.toHaveBeenCalled()
    expect(state.backend).not.toHaveBeenCalled()
    expect(admit).not.toHaveBeenCalled()
  })
})
