import { renderToString } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import { encodeBase64Url } from '../../src/crypto/bytes'
import { initialReceiveLifecycleState, nextReceiveLifecycleState } from '../../src/output/workspace/state'
import { storedReceiveLifecycleState, decodeStoredReceiveLifecycleState } from '../../src/output/workspace/state-codec'
import { validatePersistedReceiveRecord } from '../../src/output/workspace/records'
import { snapshotReceiveContentWarning, type ReceiveContentWarning } from '../../src/output/workspace/lifecycle/content-warning'
import { presentTask, retainedTaskFacts } from '../../src/ui/tasks'
import { TaskCard, TaskDetails } from '../../src/ui/tasks/TaskView'

const id = (width: number, value: number) => encodeBase64Url(new Uint8Array(width).fill(value))
const warning: ReceiveContentWarning = {
  kind: 'partial-zip', selectedFileCount: 3n, completedFileCount: 2n,
  missingFiles: [{ path: ['folder', 'b.txt'], reason: 'source-changed' }],
}
const actions = { perform: () => undefined, catchUp: () => undefined }

function ready(contentWarning?: ReceiveContentWarning) {
  return nextReceiveLifecycleState(initialReceiveLifecycleState({
    operationId: id(16, 1), receiveIntentDigest: id(32, 2),
  }), { kind: 'waiting-to-save', packageDigest: id(32, 3), contentWarning: contentWarning ?? null })
}

async function reload(state: ReturnType<typeof ready>) {
  return decodeStoredReceiveLifecycleState(await validatePersistedReceiveRecord(
    structuredClone(await storedReceiveLifecycleState(state)),
  ))
}

describe('retained partial ZIP results', () => {
  it('keeps the missing-file notice through handoff, reload and another export', async () => {
    let state = await reload(ready(warning))
    for (let attempt = 0; attempt < 2; attempt++) {
      state = nextReceiveLifecycleState(state, { kind: 'handing-off', attemptKind: 'workspace',
        activeLeaseId: id(16, 4), attemptId: id(16, 5 + attempt), packageDigest: id(32, 3) })
      state = await reload(nextReceiveLifecycleState(state, { kind: 'download-started', attemptKind: 'workspace',
        attemptId: id(16, 5 + attempt), packageDigest: id(32, 3) }))
      const task = presentTask(retainedTaskFacts({
        operationId: state.operationId, receiveIntentDigest: state.receiveIntentDigest,
        lifecycleGeneration: state.generation, lifecycle: state, continuation: 'retry-download',
        actions: ['redownload', 'forget'],
      }))
      expect(task.completeness).toBe('partial')
      expect(task.tone).toBe('warning')
      expect(task.primaryAction?.label).toBe('Download again')
      const card = renderToString(<TaskCard task={task} actions={actions} onDetails={() => undefined} />)
      const details = renderToString(<TaskDetails task={task} actions={actions} />)
      expect(card).toContain('Partial download')
      expect(card).toContain('Partial result')
      expect(details).toContain('2/3 files received; 1 not included in the ZIP.')
      expect(details).toContain('folder/b.txt: The source file changed or was deleted.')
      state = nextReceiveLifecycleState(state, { kind: 'waiting-to-save', packageDigest: id(32, 3) })
    }
  })

  it('keeps ordinary results unchanged and clears a warning when resumed content completes', async () => {
    const completed = nextReceiveLifecycleState(ready(warning), {
      kind: 'waiting-to-save', packageDigest: id(32, 3), contentWarning: null,
    })
    for (const state of [await reload(ready()), await reload(completed)]) {
      expect(state).not.toHaveProperty('contentWarning')
      const task = presentTask(retainedTaskFacts({
        operationId: state.operationId, receiveIntentDigest: state.receiveIntentDigest,
        lifecycleGeneration: state.generation, lifecycle: state, continuation: 'save-artifact', actions: ['save'],
      }))
      expect(task.completeness).toBe('complete')
      expect(task.details.join(' ')).not.toContain('Partial')
    }
  })

  it('bounds representative paths without understating the number of missing files', () => {
    const contentWarning = snapshotReceiveContentWarning({ ...warning, selectedFileCount: 20n })
    const task = presentTask(retainedTaskFacts({
      operationId: id(16, 1), receiveIntentDigest: id(32, 2), lifecycleGeneration: 2n,
      lifecycle: ready(contentWarning), continuation: 'save-artifact', actions: ['save'],
    }))
    expect(task.details).toContain('17 additional missing files are not listed.')
    expect(() => snapshotReceiveContentWarning({ ...warning, completedFileCount: 3n })).toThrow()
    expect(() => snapshotReceiveContentWarning({ ...warning, selectedFileCount: 20n,
      missingFiles: Array.from({ length: 9 }, () => warning.missingFiles[0]!) })).toThrow()
  })
})
