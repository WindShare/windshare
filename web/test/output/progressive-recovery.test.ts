import { describe, expect, it, vi } from 'vitest'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { NativeZipRecoveryUnavailableError, verifyProgressiveZipRecovery } from '../../src/output/resume/progressive-checkpoint'
import { ReceiveOperationResumeAuthority } from '../../src/output/resume/authority'
import { storedReceiveLifecycleState, decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'
import type { TaskCheckpoint, TaskEntry, TaskObjectRef } from '../../src/output/origin-private/task-checkpoint/model'
import type { TaskCheckpointStore } from '../../src/output/origin-private/task-checkpoint/store'

const id = (width: number, n: number) => encodeBase64Url(new Uint8Array(width).fill(n))
const object: TaskObjectRef = { operationId: id(16, 1), objectId: id(32, 2), kind: 'zip-archive', handleId: 'owned-handle' }
const lifecycle: ReceiveLifecycleState = {
  operationId: object.operationId, receiveIntentDigest: id(32, 3), generation: 4n,
  kind: 'resumable-receive', payloadKind: 'opfs-zip', objectId: object.objectId,
  checkpointGeneration: 2n, occupiedBytes: 58n, completedFileCount: 1n, completedBytes: 8n, discoveryComplete: true,
}
const entry: TaskEntry = {
  entryId: 'file', path: ['file'], kind: 'file',
  source: { shareInstance: 'share', directoryId: 'root', generation: 'generation', sourcePath: ['file'] },
  revision: { fileId: 'file', fileRevision: 'revision', exactSize: 8n },
  zipLayout: { entryId: 'file', sequence: 0n, localHeaderOffset: 0n, payloadOffset: 50n,
    exactSize: 8n, descriptorOffset: 58n, endOffset: 74n, encodingVersion: 1 },
  ranges: [{ start: 0n, end: 8n, crc32: 1 }],
}
const checkpoint: TaskCheckpoint = {
  object, generation: 3n, allocatedLength: 74n, physicalLength: 58n, entryCount: 1n,
  discoveryComplete: true, selectedPaths: [['file']], artifactState: 'receiving',
}
function store(state = checkpoint, entries = [entry]): TaskCheckpointStore {
  return {
    readCheckpoint: async () => state, readEntry: async () => entries[0],
    readEntries: async options => options.afterSequence === undefined ? entries : [],
    commit: async () => undefined, close: () => undefined,
    readDirectoryPin: async () => undefined, readPath: async () => undefined,
  }
}

describe('native ZIP retained recovery authority', () => {
  it('round-trips its tagged authority without file-set proof or retention deadline', async () => {
    const record = await storedReceiveLifecycleState(lifecycle)
    expect(decodeStoredReceiveLifecycleState(record)).toEqual(lifecycle)
    expect(record).not.toHaveProperty('expiresAt')
    expect(lifecycle).not.toHaveProperty('checkpointSetDigest')
  })
  it('requires discovery completion even when every discovered byte is durable', async () => {
    expect((await verifyProgressiveZipRecovery(store(), object, lifecycle)).requirement).toBe('local-finalization')
    expect((await verifyProgressiveZipRecovery(store({ ...checkpoint, discoveryComplete: false }), object, lifecycle)).requirement)
      .toBe('remote-content-needed')
  })
  it('does not trust optimistic lifecycle counts over missing committed ranges', async () => {
    expect((await verifyProgressiveZipRecovery(store(checkpoint, [{ ...entry, ranges: [] }]), object, lifecycle)).requirement)
      .toBe('remote-content-needed')
  })
  it('rejects foreign objects, future generations, and incomplete entry pages', async () => {
    await expect(verifyProgressiveZipRecovery(store(), { ...object, objectId: 'other' }, lifecycle)).rejects.toThrow('ownership')
    await expect(verifyProgressiveZipRecovery(store({ ...checkpoint, generation: 1n }), object, lifecycle)).rejects.toThrow('committed')
    await expect(verifyProgressiveZipRecovery(store(checkpoint, []), object, lifecycle)).rejects.toThrow('entry authority')
  })
  it('keeps unrelated tasks available when one native checkpoint is missing', async () => {
    const other = { ...lifecycle, operationId: id(16, 4) }
    const authority = new ReceiveOperationResumeAuthority({
      source: {
        listLifecycleStates: async () => [lifecycle, other],
        readProgressiveRequirement: async state => {
          if (state.operationId === lifecycle.operationId) throw new NativeZipRecoveryUnavailableError(new TypeError('missing'))
          return 'remote-content-needed'
        },
      },
      mutations: { resume: async () => undefined, cleanup: async () => undefined,
        discard: async () => ({ kind: 'already-absent' }) },
    })
    const inventory = await authority.listResumeState()
    expect(inventory.operations).toHaveLength(2)
    expect(inventory.operations.find(ref => ref.descriptor.operationId === lifecycle.operationId)?.descriptor)
      .toMatchObject({ continuation: 'needs-attention', recoveryUnavailable: 'native-checkpoint-unavailable' })
    expect(inventory.operations.find(ref => ref.descriptor.operationId === other.operationId)?.descriptor.continuation)
      .toBe('resume-receive')
  })
  it('classifies a checkpoint-proven local task for sender-independent inventory continuation', async () => {
    const resume = vi.fn(async () => 'complete')
    const authority = new ReceiveOperationResumeAuthority({
      source: { listLifecycleStates: async () => [lifecycle], readProgressiveRequirement: async () => 'local-finalization' },
      mutations: { resume, cleanup: async () => 'expired', discard: async () => ({ kind: 'already-absent' }) },
    })
    const reference = (await authority.listResumeState()).operations[0]!
    expect(reference.descriptor.continuation).toBe('resume-local-finalization')
    await expect(authority.resume(reference)).resolves.toBe('complete')
    expect(resume).toHaveBeenCalledOnce()
  })
})
