import { describe, expect, it } from 'vitest'

import {
  FSAOperationMutationClosedError,
  FSATerminalMutationUnavailableError,
  type FSAParentMutationIdentity,
} from '../../src/output/browser/mutation-coordination/model'
import { createFSAOperationMutationScheduler } from '../../src/output/browser/mutation-coordination/scheduler'

describe('FSA operation mutation scheduler', () => {
  it('admits independent writer lifetimes concurrently up to the injected ceiling', async () => {
    const root = parent('root')
    const scheduler = createFSAOperationMutationScheduler({
      rootParent: root,
      maximumActiveWriters: 2,
    })

    const first = await scheduler.acquireWriter(root)
    const second = await scheduler.acquireWriter(root)
    let thirdAcquired = false
    const thirdPromise = scheduler.acquireWriter(root).then((lease) => {
      thirdAcquired = true
      return lease
    })
    await microtask()
    expect(thirdAcquired).toBe(false)
    expect(scheduler.diagnostics()).toMatchObject({
      activeWriters: 2,
      queuedWriters: 1,
      peakActiveWriters: 2,
    })

    first.release()
    first.release()
    const third = await thirdPromise
    expect(thirdAcquired).toBe(true)
    second.release()
    third.release()
    await scheduler.close()
    expect(scheduler.diagnostics()).toMatchObject({
      state: 'closed',
      acquiredWriterLeases: 3,
      releasedWriterLeases: 3,
    })
  })

  it('drains an active writer and prevents a later same-parent writer from overtaking mutation', async () => {
    const root = parent('root')
    const scheduler = createFSAOperationMutationScheduler({
      rootParent: root,
      maximumActiveWriters: 2,
    })
    const activeWriter = await scheduler.acquireWriter(root)
    const mutationGate = deferred()
    const mutationStarted = deferred()
    const order: string[] = []

    const mutation = scheduler.runNamespace([root], 'create-file', async () => {
      order.push('mutation')
      mutationStarted.resolve()
      await mutationGate.promise
    })
    let laterWriterAcquired = false
    const laterWriter = scheduler.acquireWriter(root).then((lease) => {
      laterWriterAcquired = true
      order.push('writer')
      return lease
    })

    await microtask()
    expect(order).toEqual([])
    expect(laterWriterAcquired).toBe(false)

    activeWriter.release()
    await mutationStarted.promise
    expect(order).toEqual(['mutation'])
    expect(laterWriterAcquired).toBe(false)

    mutationGate.resolve()
    await mutation
    const admitted = await laterWriter
    expect(order).toEqual(['mutation', 'writer'])
    admitted.release()
    await scheduler.close()
  })

  it('runs already-queued sibling namespace mutations as a no-timer batch', async () => {
    const root = parent('root')
    const scheduler = createFSAOperationMutationScheduler({
      rootParent: root,
      maximumActiveWriters: 2,
    })
    const activeWriter = await scheduler.acquireWriter(root)
    const firstGate = deferred()
    const firstStarted = deferred()
    const order: string[] = []

    const firstMutation = scheduler.runNamespace([root], 'create-file', async () => {
      order.push('first-mutation')
      firstStarted.resolve()
      await firstGate.promise
    })
    const secondMutation = scheduler.runNamespace([root], 'create-directory', async () => {
      order.push('second-mutation')
    })
    const writer = scheduler.acquireWriter(root).then((lease) => {
      order.push('writer')
      return lease
    })

    activeWriter.release()
    await firstStarted.promise
    expect(order).toEqual(['first-mutation'])
    firstGate.resolve()
    await Promise.all([firstMutation, secondMutation])
    const admitted = await writer
    expect(order).toEqual(['first-mutation', 'second-mutation', 'writer'])
    admitted.release()
    await scheduler.close()
  })

  it('allows another parent namespace to progress while one parent drains', async () => {
    const root = parent('root')
    const blockedParent = parent('child')
    const independentParent = parent('child')
    const scheduler = createFSAOperationMutationScheduler({
      rootParent: root,
      maximumActiveWriters: 2,
    })
    const writer = await scheduler.acquireWriter(blockedParent)
    let blockedStarted = false

    const blocked = scheduler.runNamespace([blockedParent], 'create-file', async () => {
      blockedStarted = true
    })
    await expect(scheduler.runNamespace(
      [independentParent],
      'create-directory',
      async () => 'independent',
    )).resolves.toBe('independent')
    expect(blockedStarted).toBe(false)

    writer.release()
    await blocked
    await scheduler.close()
  })

  it('orders reversed multi-parent requests without deadlock', async () => {
    const root = parent('root')
    const left = parent('left')
    const right = parent('right')
    const scheduler = createFSAOperationMutationScheduler({
      rootParent: root,
      maximumActiveWriters: 2,
    })
    const leftWriter = await scheduler.acquireWriter(left)
    const rightWriter = await scheduler.acquireWriter(right)
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

    leftWriter.release()
    await microtask()
    expect(order).toEqual([])
    rightWriter.release()
    await firstStarted.promise
    expect(order).toEqual(['first'])
    firstGate.resolve()
    await Promise.all([first, second])
    expect(order).toEqual(['first', 'second'])
    await scheduler.close()
  })

  it('releases global writer capacity after a rejected native close attempt', async () => {
    const root = parent('root')
    const scheduler = createFSAOperationMutationScheduler({
      rootParent: root,
      maximumActiveWriters: 1,
    })
    const first = await scheduler.acquireWriter(root)
    const expected = new Error('native close rejected')
    let secondAcquired = false
    const second = scheduler.acquireWriter(root).then((lease) => {
      secondAcquired = true
      return lease
    })

    const closeAttempt = (async () => {
      try {
        await Promise.reject(expected)
      } finally {
        first.release()
      }
    })()
    await expect(closeAttempt).rejects.toBe(expected)
    const admitted = await second
    expect(secondAcquired).toBe(true)
    admitted.release()
    await scheduler.close()
  })

  it('drains admitted work before one terminal-exclusive callback and rejects late admission', async () => {
    const root = parent('root')
    const other = parent('other')
    const scheduler = createFSAOperationMutationScheduler({
      rootParent: root,
      maximumActiveWriters: 2,
    })
    const writer = await scheduler.acquireWriter(root)
    const mutationGate = deferred()
    const mutationStarted = deferred()
    const mutation = scheduler.runNamespace([other], 'create-directory', async () => {
      mutationStarted.resolve()
      await mutationGate.promise
    })
    await mutationStarted.promise

    const terminal = scheduler.beginTerminal('discard-operation')
    let drained = false
    const drainObservation = terminal.drained.then(() => { drained = true })
    await expect(scheduler.acquireWriter(root)).rejects.toBeInstanceOf(
      FSAOperationMutationClosedError,
    )
    await expect(scheduler.runRootNamespace(
      'remove-entry',
      async () => undefined,
    )).rejects.toBeInstanceOf(FSAOperationMutationClosedError)
    await microtask()
    expect(drained).toBe(false)

    writer.release()
    mutationGate.resolve()
    await mutation
    await terminal.drained
    await drainObservation
    expect(drained).toBe(true)

    let exclusiveRuns = 0
    await expect(terminal.runExclusive(async (authority) => {
      exclusiveRuns += 1
      expect(authority.kind).toBe('discard-operation')
      expect(scheduler.diagnostics()).toMatchObject({
        state: 'draining',
        activeWriters: 0,
        activeNamespaceMutations: 0,
      })
      await expect(scheduler.runRootNamespace(
        'remove-entry',
        async () => undefined,
      )).rejects.toBeInstanceOf(FSAOperationMutationClosedError)
      return 'removed-recursively'
    })).resolves.toBe('removed-recursively')
    expect(exclusiveRuns).toBe(1)
    await expect(terminal.runExclusive(async () => undefined)).rejects.toBeInstanceOf(
      FSATerminalMutationUnavailableError,
    )
    expect(scheduler.diagnostics()).toMatchObject({
      state: 'closed',
      terminalExclusiveRuns: 1,
      failedTerminalExclusiveRuns: 0,
    })
  })

  it('closes idempotently only after admitted writer lifetimes drain', async () => {
    const root = parent('root')
    const scheduler = createFSAOperationMutationScheduler({
      rootParent: root,
      maximumActiveWriters: 1,
    })
    const writer = await scheduler.acquireWriter(root)
    let closed = false

    const firstClose = scheduler.close()
    const secondClose = scheduler.close()
    expect(secondClose).toBe(firstClose)
    const closeObservation = firstClose.then(() => { closed = true })
    await expect(scheduler.acquireWriter(root)).rejects.toBeInstanceOf(
      FSAOperationMutationClosedError,
    )
    await microtask()
    expect(closed).toBe(false)

    writer.release()
    await Promise.all([firstClose, secondClose, closeObservation])
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

async function microtask(): Promise<void> {
  await Promise.resolve()
}
