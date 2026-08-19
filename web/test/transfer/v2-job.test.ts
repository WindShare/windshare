import { describe, expect, it } from 'vitest'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import type { TransferTraceEvent } from '../../src/transfer/v2-job'
import {
  catalogFixture,
  directoryEntry,
  fileEntry,
  identity,
  identityText,
  planAuthorityFixture,
  readerFixture,
  receiveIntentFixture,
  selectOnlyFile,
  transferJobFixture,
  type TestArtifactKind,
  type TestPlanKind,
} from './v2-job-fixture'

describe('plan-specific transfer routing', () => {
  it.each<readonly [TestPlanKind, TestArtifactKind, string]>([
    ['direct-tree', 'directory-tree', 'published'],
    ['direct-atomic', 'original-file', 'published'],
    ['workspace-then-publish', 'original-file', 'materialization-sealed'],
    ['portable-handoff', 'original-file', 'download-started'],
  ])('routes %s without deriving a representation from the output session', async (
    planKind,
    artifactKind,
    lifecycleKind,
  ) => {
    const file = fileEntry(identity(4), 'file.bin', 4n)
    const selection = artifactKind === 'directory-tree'
      ? new V2SelectionPolicy()
      : selectOnlyFile(file)
    const intent = await receiveIntentFixture({ planKind, artifactKind, selection, file })
    const catalog = catalogFixture([{ id: identity(2), entries: [file] }])
    const readers = readerFixture([file])
    const plans = planAuthorityFixture()
    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(plans.routes).toEqual([planKind])
    expect(result.worker.status).toBe('Succeeded')
    expect(result.lifecycle.kind).toBe(lifecycleKind)
    expect(result.outputDurability).toBe('None')
    expect(plans.output.requests).toHaveLength(1)
    expect('format' in plans.output).toBe(false)
    if (planKind === 'portable-handoff' ||
        (planKind === 'workspace-then-publish' && artifactKind === 'zip-archive')) {
      expect(plans.preparations).toHaveLength(1)
      expect(result.preparation?.fileCount).toBe(1n)
    } else {
      expect(plans.preparations).toHaveLength(0)
    }
  })

  it('emits only stable plan/operation facts and no catalog identity or path', async () => {
    const file = fileEntry(identity(9), 'secret-name.bin', 2n)
    const selection = new V2SelectionPolicy()
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })
    const catalog = catalogFixture([{ id: identity(2), entries: [file] }])
    const readers = readerFixture([file])
    const traces: TransferTraceEvent[] = []
    await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans: planAuthorityFixture(),
      revisions: readers.revisions,
      broker: readers.broker,
      trace: { current: event => { traces.push(event) } },
    }).run()

    expect(traces
      .filter(event => event.name === 'receive_transition')
      .map(event => event.transition)).toEqual(expect.arrayContaining([
        'intent_frozen',
        'materialization_started',
        'materialization_completed',
        'tree_finalized',
      ]))
    const serialized = JSON.stringify(traces, (_key, value) =>
      typeof value === 'bigint' ? value.toString() : value,
    )
    expect(serialized).not.toContain('secret-name.bin')
    expect(serialized).not.toContain(file.idText)
    expect(serialized).not.toContain('file_id')
    expect(serialized).not.toContain('directory_id')
  })

  it('does not give workspace OriginalFile a whole-tree preparation barrier', async () => {
    const root = identity(2)
    const child = identity(3)
    const file = fileEntry(identity(11), 'a.bin', 2n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([
      { id: root, entries: [file, directoryEntry(child, 'z-child')] },
      { id: child, entries: [], loadFailure: new Error('unrelated branch unavailable') },
    ])
    const readers = readerFixture([file])
    const plans = planAuthorityFixture({
      onWorkspaceOriginalAdmission: (evidence) => {
        expect(evidence).toMatchObject({
          fileId: file.idText,
          containingDirectoryId: identityText(2),
          generation: identityText(90),
          catalogSize: 2n,
          sourcePath: ['a.bin'],
        })
        expect(readers.revisionRequests).toEqual([])
        expect(readers.blockRequests).toEqual([])
      },
    })
    const intent = await receiveIntentFixture({
      planKind: 'workspace-then-publish',
      artifactKind: 'original-file',
      selection,
      file,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('Succeeded')
    expect(result.lifecycle.kind).toBe('materialization-sealed')
    expect(catalog.loads).toEqual([expect.any(String)])
    expect(plans.routes).toEqual(['workspace-then-publish'])
    expect(plans.preparations).toEqual([])
    expect(plans.singleFileAdmissions).toHaveLength(1)
    expect(result.preparation).toBeUndefined()
  })

  it('starts DirectTree file content while a later directory is still discovering', async () => {
    const root = identity(2)
    const child = identity(3)
    const file = fileEntry(identity(11), 'a.bin', 2n)
    const selection = new V2SelectionPolicy(true)
    const childReplayStarted = deferred()
    const releaseChildReplay = deferred()
    const blockStarted = deferred()
    const releaseBlock = deferred()
    const catalog = catalogFixture([
      { id: root, entries: [file, directoryEntry(child, 'z-child')] },
      {
        id: child,
        entries: [],
        beforePages: async () => {
          childReplayStarted.resolve()
          await releaseChildReplay.promise
        },
      },
    ])
    const readers = readerFixture([file], [], {
      beforeRead: async () => {
        blockStarted.resolve()
        await releaseBlock.promise
      },
    })
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })
    const running = transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    await withExternalBound(Promise.all([
      childReplayStarted.promise,
      blockStarted.promise,
    ]), 500)
    expect(plans.output.commits).toEqual([])
    releaseBlock.resolve()
    releaseChildReplay.resolve()
    const result = await running

    expect(result.worker.status).toBe('Succeeded')
    expect(result.lifecycle.kind).toBe('published')
    expect(readers.blockRequests).toEqual([file.idText])
  })

  it('never exceeds the configured file-worker bound', async () => {
    const root = identity(2)
    const files = Array.from({ length: 6 }, (_, index) =>
      fileEntry(identity(20 + index), `file-${index}.bin`, 2n))
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: root, entries: files }])
    const twoReadersStarted = deferred()
    const releaseReaders = deferred()
    let activeReaders = 0
    let maximumActiveReaders = 0
    let startedReaders = 0
    const readers = readerFixture(files, [], {
      beforeRead: async () => {
        activeReaders += 1
        startedReaders += 1
        maximumActiveReaders = Math.max(maximumActiveReaders, activeReaders)
        if (startedReaders === 2) twoReadersStarted.resolve()
        try {
          await releaseReaders.promise
        } finally {
          activeReaders -= 1
        }
      },
    })
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })
    const running = transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
      maximumConcurrentFiles: 2,
    }).run()

    await withExternalBound(twoReadersStarted.promise, 500)
    expect(readers.blockRequests).toHaveLength(2)
    expect(maximumActiveReaders).toBe(2)
    releaseReaders.resolve()
    const result = await running

    expect(result.worker.status).toBe('Succeeded')
    expect(startedReaders).toBe(files.length)
    expect(maximumActiveReaders).toBe(2)
  })
})

function deferred(): {
  readonly promise: Promise<void>
  readonly resolve: () => void
} {
  let resolve!: () => void
  const promise = new Promise<void>((complete) => {
    resolve = complete
  })
  return { promise, resolve }
}

async function withExternalBound<T>(operation: Promise<T>, milliseconds: number): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined
  const timeout = new Promise<never>((_resolve, reject) => {
    timer = setTimeout(() => reject(new Error('operation exceeded external test bound')), milliseconds)
  })
  try {
    return await Promise.race([operation, timeout])
  } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
}
