import { describe, expect, it } from 'vitest'

import {
  FSAOperationMutationClosedError,
  FSATerminalMutationUnavailableError,
  type FSAFileMutationIdentity,
  type FSAParentMutationIdentity,
  type FSAVerifiedFileMutationTarget,
} from '../../src/output/browser/mutation-coordination/model'
import { createFSAOperationMutationScheduler } from '../../src/output/browser/mutation-coordination/scheduler'

describe('FSA operation mutation scheduler', () => {
  it('admits independent same-parent writer lifetimes up to the injected ceiling', async () => {
    const root = parent('root')
    const scheduler = createFSAOperationMutationScheduler({ rootParent: root, maximumActiveWriters: 2 })
    const first = await scheduler.acquireWriter(file(root))
    const second = await scheduler.acquireWriter(file(root))
    let thirdAcquired = false
    const thirdPromise = scheduler.acquireWriter(file(root)).then((lease) => {
      thirdAcquired = true
      return lease
    })
    await microtask()
    expect(thirdAcquired).toBe(false)
    expect(scheduler.diagnostics()).toMatchObject({ activeWriters: 2, queuedWriters: 1, peakActiveWriters: 2 })

    first.release()
    first.release()
    const third = await thirdPromise
    second.release()
    third.release()
    await scheduler.close()
    expect(scheduler.diagnostics()).toMatchObject({
      state: 'closed', acquiredWriterLeases: 3, releasedWriterLeases: 3,
    })
  })

  it('serializes writers for one verified file while sibling writers continue', async () => {
    const root = parent('root')
    const target = file(root)
    const scheduler = createFSAOperationMutationScheduler({ rootParent: root, maximumActiveWriters: 2 })
    const first = await scheduler.acquireWriter(target)
    let nextAcquired = false
    const next = scheduler.acquireWriter({ ...target }).then((lease) => {
      nextAcquired = true
      return lease
    })
    const sibling = await scheduler.acquireWriter(file(root))
    expect(nextAcquired).toBe(false)
    sibling.release()
    first.release()
    const acquired = await next
    acquired.release()
    await scheduler.close()
  })

  it('lets sibling inspection and creation overlap active writers without widening the writer budget', async () => {
    const root = parent('root')
    const scheduler = createFSAOperationMutationScheduler({ rootParent: root, maximumActiveWriters: 1 })
    const writer = await scheduler.acquireWriter(file(root))
    let queuedWriterEntered = false
    const queuedWriter = scheduler.acquireWriter(file(root)).then((lease) => {
      queuedWriterEntered = true
      return lease
    })
    const inspectionGate = deferred()
    const order: string[] = []
    const inspection = scheduler.runNamespace([root], 'inspect-entry', async () => {
      order.push('inspect')
      await inspectionGate.promise
    })
    const creation = scheduler.runNamespace([root], 'create-file', async () => { order.push('create') })
    await microtask()
    expect(order).toEqual(['inspect'])
    expect(queuedWriterEntered).toBe(false)
    inspectionGate.resolve()
    await Promise.all([inspection, creation])
    expect(order).toEqual(['inspect', 'create'])
    expect(scheduler.diagnostics().activeWriters).toBe(1)
    writer.release()
    const acquired = await queuedWriter
    acquired.release()
    await scheduler.close()
  })

  it('waits only for the removed file and stops later same-file writers overtaking removal', async () => {
    const root = parent('root')
    const target = file(root)
    const scheduler = createFSAOperationMutationScheduler({ rootParent: root, maximumActiveWriters: 2 })
    const writer = await scheduler.acquireWriter(target)
    const removalGate = deferred()
    const removalStarted = deferred()
    const order: string[] = []
    const removal = scheduler.runFileMutation(target, 'remove-file', async () => {
      order.push('remove')
      removalStarted.resolve()
      await removalGate.promise
    })
    const laterWriter = scheduler.acquireWriter(target).then((lease) => {
      order.push('writer')
      return lease
    })

    await scheduler.runNamespace([root], 'create-file', async () => { order.push('sibling-create') })
    const siblingWriter = await scheduler.acquireWriter(file(root))
    await scheduler.runNamespace([parent('independent')], 'create-directory', async () => undefined)
    expect(order).toEqual(['sibling-create'])
    writer.release()
    await removalStarted.promise
    expect(order).toEqual(['sibling-create', 'remove'])
    expect(scheduler.diagnostics().activeWriters).toBe(1)
    removalGate.resolve()
    await removal
    const admitted = await laterWriter
    expect(order).toEqual(['sibling-create', 'remove', 'writer'])
    admitted.release()
    siblingWriter.release()
    await scheduler.close()
  })

  it('preserves file order when an earlier writer is waiting for global capacity', async () => {
    const root = parent('root')
    const target = file(root)
    const scheduler = createFSAOperationMutationScheduler({ rootParent: root, maximumActiveWriters: 1 })
    const occupied = await scheduler.acquireWriter(file(root))
    const order: string[] = []
    const earlierWriter = scheduler.acquireWriter(target).then((lease) => {
      order.push('earlier-writer')
      return lease
    })
    const removal = scheduler.runFileMutation(target, 'remove-file', async () => { order.push('remove') })
    const laterWriter = scheduler.acquireWriter(target).then((lease) => {
      order.push('later-writer')
      return lease
    })
    await microtask()
    expect(order).toEqual([])
    occupied.release()
    const earlier = await earlierWriter
    expect(order).toEqual(['earlier-writer'])
    earlier.release()
    await removal
    const later = await laterWriter
    expect(order).toEqual(['earlier-writer', 'remove', 'later-writer'])
    later.release()
    await scheduler.close()
  })

  it('orders reversed multi-parent name reservations while content writers stay open', async () => {
    const root = parent('root')
    const left = parent('left')
    const right = parent('right')
    const scheduler = createFSAOperationMutationScheduler({ rootParent: root, maximumActiveWriters: 2 })
    const leftWriter = await scheduler.acquireWriter(file(left))
    const rightWriter = await scheduler.acquireWriter(file(right))
    const firstGate = deferred()
    const firstStarted = deferred()
    const order: string[] = []
    const first = scheduler.runNamespace([right, left], 'repair-compatible-name', async () => {
      order.push('first')
      firstStarted.resolve()
      await firstGate.promise
    })
    const second = scheduler.runNamespace([left, right], 'repair-compatible-name', async () => {
      order.push('second')
    })
    await firstStarted.promise
    expect(order).toEqual(['first'])
    expect(scheduler.diagnostics().activeWriters).toBe(2)
    firstGate.resolve()
    await Promise.all([first, second])
    expect(order).toEqual(['first', 'second'])
    leftWriter.release()
    rightWriter.release()
    await scheduler.close()
  })

  it('releases namespace ownership after failure without losing a waiting file mutation', async () => {
    const root = parent('root')
    const target = file(root)
    const scheduler = createFSAOperationMutationScheduler({ rootParent: root, maximumActiveWriters: 1 })
    const writer = await scheduler.acquireWriter(target)
    const expected = new Error('namespace failed')
    const removal = scheduler.runFileMutation(target, 'remove-file', async () => 'removed')
    await expect(scheduler.runNamespace([root], 'create-file', async () => { throw expected })).rejects.toBe(expected)
    writer.release()
    await expect(removal).resolves.toBe('removed')
    await scheduler.close()
    expect(scheduler.diagnostics()).toMatchObject({ failedNamespaceMutations: 1, completedNamespaceMutations: 1 })
  })

  it('rejects changing the verified parent of a retained file identity', async () => {
    const root = parent('root')
    const target = file(root)
    const scheduler = createFSAOperationMutationScheduler({ rootParent: root, maximumActiveWriters: 1 })
    const writer = await scheduler.acquireWriter(target)
    expect(() => scheduler.acquireWriter({ ...target, parent: parent('different') })).toThrow(TypeError)
    writer.release()
    await scheduler.close()
  })

  it('drains every admitted writer and namespace callback before terminal-exclusive work', async () => {
    const root = parent('root')
    const target = file(root)
    const scheduler = createFSAOperationMutationScheduler({ rootParent: root, maximumActiveWriters: 2 })
    const writer = await scheduler.acquireWriter(target)
    const mutationGate = deferred()
    const mutation = scheduler.runNamespace([root], 'create-directory', () => mutationGate.promise)
    const removal = scheduler.runFileMutation(target, 'remove-file', async () => undefined)
    const terminal = scheduler.beginTerminal('discard-operation')
    let drained = false
    const drainObservation = terminal.drained.then(() => { drained = true })
    await expect(scheduler.acquireWriter(target)).rejects.toBeInstanceOf(FSAOperationMutationClosedError)
    await expect(scheduler.runRootNamespace('create-file', async () => undefined)).rejects.toBeInstanceOf(
      FSAOperationMutationClosedError,
    )
    await expect(scheduler.runFileMutation(target, 'remove-file', async () => undefined)).rejects.toBeInstanceOf(
      FSAOperationMutationClosedError,
    )
    writer.release()
    await microtask()
    expect(drained).toBe(false)
    mutationGate.resolve()
    await Promise.all([mutation, removal, drainObservation])
    expect(drained).toBe(true)
    await expect(terminal.runExclusive(async (authority) => {
      expect(authority.kind).toBe('discard-operation')
      expect(scheduler.diagnostics()).toMatchObject({ state: 'draining', activeWriters: 0, activeNamespaceMutations: 0 })
      return 'removed-recursively'
    })).resolves.toBe('removed-recursively')
    await expect(terminal.runExclusive(async () => undefined)).rejects.toBeInstanceOf(FSATerminalMutationUnavailableError)
    expect(scheduler.diagnostics()).toMatchObject({ state: 'closed', terminalExclusiveRuns: 1 })
  })

  it('closes idempotently only after admitted writer lifetimes drain', async () => {
    const root = parent('root')
    const scheduler = createFSAOperationMutationScheduler({ rootParent: root, maximumActiveWriters: 1 })
    const writer = await scheduler.acquireWriter(file(root))
    let closed = false
    const firstClose = scheduler.close()
    expect(scheduler.close()).toBe(firstClose)
    const closeObservation = firstClose.then(() => { closed = true })
    await microtask()
    expect(closed).toBe(false)
    writer.release()
    await closeObservation
    expect(closed).toBe(true)
    expect(scheduler.diagnostics().state).toBe('closed')
  })
})

interface Deferred {
  readonly promise: Promise<void>
  readonly resolve: () => void
}

function deferred(): Deferred {
  let resolve!: () => void
  const promise = new Promise<void>((complete) => { resolve = complete })
  return { promise, resolve }
}

function parent(description: string): FSAParentMutationIdentity {
  return Symbol(description) as FSAParentMutationIdentity
}

function file(parentIdentity: FSAParentMutationIdentity): FSAVerifiedFileMutationTarget {
  return Object.freeze({ parent: parentIdentity, file: Symbol('verified-file') as FSAFileMutationIdentity })
}

async function microtask(): Promise<void> {
  await Promise.resolve()
}
