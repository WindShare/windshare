import { describe, expect, it } from 'vitest'

import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import {
  disabledOutputExecutionProfile,
  TransferPauseRequestedError,
  TransferStopRequestedError,
} from '../../src/transfer/output-session'
import type { TransferTraceEvent } from '../../src/transfer/v2-job'
import {
  V2OutputSettlementTimeoutError,
  V2TransferFailureSettlementError,
  type V2OutputSettlementDeadline,
} from '../../src/transfer/settlement/v2-output'
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
  testOutput,
  transferJobFixture,
} from './v2-job-fixture'

describe('v2 plan settlement', () => {
  it('aborts an in-flight DirectTree receive into its typed Stop terminal cut', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    let announceRead!: () => void
    const readStarted = new Promise<void>(resolve => { announceRead = resolve })
    let releaseRead!: () => void
    const readGate = new Promise<void>(resolve => { releaseRead = resolve })
    const readers = readerFixture([file], [], {
      beforeRead: async () => {
        announceRead()
        await readGate
      },
    })
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })
    const transfer = transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    })
    const controller = new AbortController()
    const resultPromise = transfer.run(controller.signal)
    await readStarted
    const reason = new TransferStopRequestedError()
    controller.abort(reason)
    releaseRead()

    const result = await resultPromise

    expect(result.worker.status).toBe('Paused')
    expect(result.abortReason).toBe(reason)
    expect(result.lifecycle).toMatchObject({ kind: 'partial-directory', reason: 'stopped' })
    expect(plans.stops).toHaveLength(1)
    expect(plans.stops[0]?.reason).toBe(reason)
    expect(plans.stops[0]?.worker).toBe(result.worker)
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual([])
    expect(plans.stopSignals[0]?.aborted).toBe(true)
  })

  it.each([
    { control: 'pause', reason: () => new DOMException('pause transfer', 'AbortError') },
    { control: 'stop', reason: () => new TransferStopRequestedError() },
  ] as const)('closes DirectTree admission before cancellation directory finalization for $control', async ({
    control,
    reason,
  }) => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readStarted = deferred<void>()
    const releaseRead = deferred<void>()
    const finalizeStarted = deferred<void>()
    const releaseFinalize = deferred<void>()
    const order: string[] = []
    const readers = readerFixture([file], [], {
      beforeRead: async () => {
        readStarted.resolve()
        await releaseRead.promise
      },
    })
    const plans = planAuthorityFixture({
      onDirectTreeBeginTerminal: kind => { order.push(`begin:${kind}`) },
      beforeDirectoryFinalize: async () => {
        order.push('finalize-directory')
        finalizeStarted.resolve()
        await releaseFinalize.promise
      },
    })
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })
    const controller = new AbortController()
    const running = transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run(controller.signal)

    await readStarted.promise
    controller.abort(reason())
    releaseRead.resolve()
    await finalizeStarted.promise

    expect(order).toEqual([`begin:${control}`, 'finalize-directory'])
    expect(plans.terminalCuts).toEqual([control])
    expect(plans.pauses).toEqual([])
    expect(plans.stops).toEqual([])

    releaseFinalize.resolve()
    const result = await running
    expect(result.worker.status).toBe('Paused')
    expect(control === 'pause' ? plans.pauses : plans.stops).toHaveLength(1)
  })

  it('settles DirectTree progressively as a partial directory after a file-local failure', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], [], { failRevisionFor: file.idText })
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.worker.status).toBe('CompletedWithErrors')
    expect(result.worker.failureCount).toBe(1)
    expect(result.lifecycle.kind).toBe('partial-directory')
    expect(plans.settlements).toEqual(['direct-tree:CompletedWithErrors'])
    expect(plans.pauses).toEqual([])
  })

  it('keeps ordinary DirectTree settlement after directory finalization', async () => {
    const root = identity(2)
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: root, entries: [] }])
    const readers = readerFixture([])
    const order: string[] = []
    const plans = planAuthorityFixture({
      beforeDirectoryFinalize: () => { order.push('finalize-directory') },
      beforeDirectTreeSettlement: () => { order.push('settle') },
    })
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })

    const result = await transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
    }).run()

    expect(result.lifecycle.kind).toBe('published')
    expect(order).toEqual(['finalize-directory', 'settle'])
    expect(plans.terminalCuts).toEqual([])
  })

  it.each([
    {
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      workerFamily: 'discovery',
    },
    {
      planKind: 'workspace-then-publish',
      artifactKind: 'zip-archive',
      workerFamily: 'prepared-files',
    },
  ] as const)(
    'records a later $workerFamily worker consequence with stable receive context',
    async ({ planKind, artifactKind, workerFamily }) => {
      const root = identity(2)
      const first = fileEntry(identity(11), 'first.bin', 2n)
      const second = fileEntry(identity(12), 'second.bin', 2n)
      const selection = new V2SelectionPolicy(true)
      const catalog = catalogFixture([{ id: root, entries: [first, second] }])
      const firstStarted = deferred<void>()
      const secondStarted = deferred<void>()
      const releaseFirst = deferred<void>()
      const releaseSecond = deferred<void>()
      const familyAborted = deferred<void>()
      const initiatingFailure = new TransferPauseRequestedError()
      const laterFailure = new Error('later worker consequence')
      const readers = readerFixture([first, second], [], {
        observeOpenSignal: (fileId, signal) => {
          if (fileId !== second.idText) return
          signal?.addEventListener('abort', () => familyAborted.resolve(), { once: true })
        },
        beforeOpen: async fileId => {
          if (fileId === first.idText) {
            firstStarted.resolve()
            await releaseFirst.promise
            throw initiatingFailure
          }
          secondStarted.resolve()
          await releaseSecond.promise
          throw laterFailure
        },
      })
      const plans = planAuthorityFixture({
        output: testOutput([], {
          executionProfile: disabledOutputExecutionProfile(2),
        }),
      })
      const intent = await receiveIntentFixture({ planKind, artifactKind, selection })
      const traces: TransferTraceEvent[] = []
      const running = transferJobFixture({
        catalog: catalog.catalog,
        selection,
        intent,
        plans,
        revisions: readers.revisions,
        broker: readers.broker,
        maximumConcurrentFiles: 2,
        trace: { current: event => traces.push(event) },
      }).run()

      await Promise.all([firstStarted.promise, secondStarted.promise])
      releaseFirst.resolve()
      await familyAborted.promise
      releaseSecond.resolve()
      const result = await running

      expect(result.abortReason).toBe(initiatingFailure)
      const consequences = traces.filter((event): event is Extract<
        TransferTraceEvent,
        { transition: 'worker_consequence_observed' }
      > => event.name === 'receive_transition' &&
        event.transition === 'worker_consequence_observed')
      expect(consequences).toEqual(expect.arrayContaining([
        expect.objectContaining({
          workerFamily,
          operationId: intent.operationId,
          transferJobId: result.transferJobId,
          protocolSessionId: identityText(91),
          protocolGeneration: 3,
          outputSessionId: 'test-session',
          failureSource: expect.objectContaining({ kind: 'worker' }),
        }),
      ]))
    },
  )

  it('pauses a complete artifact instead of calling settlement after the same file-local failure', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], [], { failRevisionFor: file.idText })
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
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

    expect(result.worker.status).toBe('CompletedWithErrors')
    expect(result.lifecycle.kind).toBe('restart-required')
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual(['direct-atomic'])
  })

  it.each([
    {
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      lifecycle: 'partial-directory',
      settlement: 'direct-tree:CompletedWithErrors',
    },
  ] as const)(
    'isolates a child finalization failure without allowing an incomplete $planKind artifact to publish',
    async ({ planKind, artifactKind, lifecycle, settlement }) => {
      const root = identity(2)
      const child = identity(3)
      const file = fileEntry(identity(11), 'payload.bin', 2n)
      const selection = new V2SelectionPolicy(true)
      const catalog = catalogFixture([
        { id: root, entries: [directoryEntry(child, 'child'), file] },
        { id: child, entries: [] },
      ])
      const readers = readerFixture([file])
      const plans = planAuthorityFixture({ failDirectoryFinalizePath: 'child' })
      const intent = await receiveIntentFixture({ planKind, artifactKind, selection })

      const result = await transferJobFixture({
        catalog: catalog.catalog,
        selection,
        intent,
        plans,
        revisions: readers.revisions,
        broker: readers.broker,
      }).run()

      expect(result.worker.status).toBe('CompletedWithErrors')
      expect(result.lifecycle.kind).toBe(lifecycle)
      expect(plans.output.commits).toEqual([file.idText])
      expect(plans.settlements).toEqual(settlement === undefined ? [] : [settlement])
      expect(plans.pauses).toEqual(planKind === 'direct-tree' ? [] : [planKind])
    },
  )

  it('keeps successful worker evidence distinct when terminal publication settlement fails', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 2n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file])
    const plans = planAuthorityFixture({ failSettlement: true })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
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
    expect(result.lifecycle.kind).toBe('restart-required')
    expect(result.abortReason).toEqual(expect.any(Error))
    expect(plans.settlements).toEqual(['direct-atomic:Succeeded'])
    expect(plans.pauses).toEqual(['direct-atomic'])
    expect(plans.output.commits).toEqual([file.idText])
  })

})

describe('v2 terminal settlement authority', () => {
  it('does not re-enter Pause or replace an initiating DirectTree terminal-cut failure', async () => {
    const root = identity(2)
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: root, entries: [] }])
    const readers = readerFixture([])
    const plansFailure = new Error('projector terminal drain failed')
    const plans = planAuthorityFixture({ settlementFailure: plansFailure })
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })

    let failure: unknown
    try {
      await transferJobFixture({
        catalog: catalog.catalog,
        selection,
        intent,
        plans,
        revisions: readers.revisions,
        broker: readers.broker,
      }).run()
    } catch (error) {
      failure = error
    }

    expect(failure).toBe(plansFailure)
    expect(plans.settlements).toEqual(['direct-tree:Succeeded'])
    expect(plans.pauses).toEqual([])
    expect(plans.unknownSettlements).toEqual([])
  })

  it('drains DirectTree terminal authority before exposing a latched timeout', async () => {
    const root = identity(2)
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: root, entries: [] }])
    const readers = readerFixture([])
    const terminalEntered = deferred<void>()
    const terminalAborted = deferred<void>()
    const releaseTerminal = deferred<void>()
    const terminalMutations: string[] = []
    const deadline = manualSettlementDeadline()
    const plans = planAuthorityFixture({
      beforeDirectTreeSettlement: async signal => {
        signal.addEventListener('abort', () => terminalAborted.resolve(), { once: true })
        terminalEntered.resolve()
        await releaseTerminal.promise
        terminalMutations.push('terminal-finished')
      },
    })
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
      outputSettlementDeadline: deadline,
    }).run()
    let exposed = false
    const exposureObservation = running.then(
      () => { exposed = true },
      () => { exposed = true },
    )
    await terminalEntered.promise

    deadline.expire()
    await terminalAborted.promise

    expect(exposed).toBe(false)
    expect(terminalMutations).toEqual([])
    expect(plans.settlementSignals[0]?.aborted).toBe(true)

    releaseTerminal.resolve()
    await expect(running).rejects.toBeInstanceOf(V2OutputSettlementTimeoutError)
    await exposureObservation
    expect(exposed).toBe(true)
    expect(terminalMutations).toEqual(['terminal-finished'])
    expect(plans.pauses).toEqual([])
    expect(plans.unknownSettlements).toEqual([])

    const stableMutations = [...terminalMutations]
    await Promise.resolve()
    expect(terminalMutations).toEqual(stableMutations)
  })

  it('drains DirectTree Stop authority before exposing a latched timeout', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 4n)
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readStarted = deferred<void>()
    const releaseRead = deferred<void>()
    const readers = readerFixture([file], [], {
      beforeRead: async () => {
        readStarted.resolve()
        await releaseRead.promise
      },
    })
    const terminalEntered = deferred<void>()
    const terminalAborted = deferred<void>()
    const releaseTerminal = deferred<void>()
    const terminalMutations: string[] = []
    const deadline = manualSettlementDeadline()
    const plans = planAuthorityFixture({
      beforeDirectTreeStop: async signal => {
        signal.addEventListener('abort', () => terminalAborted.resolve(), { once: true })
        terminalEntered.resolve()
        await releaseTerminal.promise
        terminalMutations.push('stop-finished')
      },
    })
    const intent = await receiveIntentFixture({
      planKind: 'direct-tree',
      artifactKind: 'directory-tree',
      selection,
    })
    const controller = new AbortController()
    const running = transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
      outputSettlementDeadline: deadline,
    }).run(controller.signal)
    let exposed = false
    const exposureObservation = running.then(
      () => { exposed = true },
      () => { exposed = true },
    )
    await readStarted.promise
    controller.abort(new TransferStopRequestedError())
    releaseRead.resolve()
    await terminalEntered.promise

    deadline.expire()
    await terminalAborted.promise

    expect(exposed).toBe(false)
    expect(terminalMutations).toEqual([])
    expect(plans.stopSignals[0]?.aborted).toBe(true)

    releaseTerminal.resolve()
    await expect(running).rejects.toBeInstanceOf(V2OutputSettlementTimeoutError)
    await exposureObservation
    expect(exposed).toBe(true)
    expect(terminalMutations).toEqual(['stop-finished'])
    expect(plans.pauses).toEqual([])
    expect(plans.unknownSettlements).toEqual([])
  })

  it('bounds a plan pause and records target ownership as unknown', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 2n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], [], { failRevisionFor: file.idText })
    const plans = planAuthorityFixture({ hangPause: true })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    const result = await withExternalBound(transferJobFixture({
      catalog: catalog.catalog,
      selection,
      intent,
      plans,
      revisions: readers.revisions,
      broker: readers.broker,
      outputSettlementTimeoutMilliseconds: 10,
    }).run(), 500)

    expect(result.worker.status).toBe('CompletedWithErrors')
    expect(result.lifecycle.kind).toBe('needs-attention')
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual(['direct-atomic'])
    expect(plans.unknownSettlements).toEqual(['direct-atomic'])
    expect(plans.pauseSignals).toHaveLength(1)
    expect(plans.pauseSignals[0]?.aborted).toBe(true)
  })

  it('records ownership unknown when an adapter reports a lifecycle illegal for the plan stage', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 2n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], [], { failRevisionFor: file.idText })
    const plans = planAuthorityFixture({ invalidPauseLifecycle: true })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
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

    expect(result.worker.status).toBe('CompletedWithErrors')
    expect(result.lifecycle.kind).toBe('needs-attention')
    expect(plans.settlements).toEqual([])
    expect(plans.pauses).toEqual(['direct-atomic'])
    expect(plans.unknownSettlements).toEqual(['direct-atomic'])
  })

  it('preserves the transfer failure when both output settlement paths fail', async () => {
    const root = identity(2)
    const file = fileEntry(identity(11), 'payload.bin', 2n)
    const selection = selectOnlyFile(file)
    const catalog = catalogFixture([{ id: root, entries: [file] }])
    const readers = readerFixture([file], [], { failRevisionFor: file.idText })
    const plans = planAuthorityFixture({ failPause: true, failUnknownSettlement: true })
    const intent = await receiveIntentFixture({
      planKind: 'direct-atomic',
      artifactKind: 'original-file',
      selection,
      file,
    })

    let thrown: unknown
    try {
      await transferJobFixture({
        catalog: catalog.catalog,
        selection,
        intent,
        plans,
        revisions: readers.revisions,
        broker: readers.broker,
      }).run()
    } catch (error) {
      thrown = error
    }

    expect(thrown).toBeInstanceOf(V2TransferFailureSettlementError)
    const failure = thrown as V2TransferFailureSettlementError
    expect(failure.transferFailure).toBe(failure.trigger)
    expect(failure.message).toBe('Transfer failed before output settlement completed')
    expect(failure.cause).toBeUndefined()
    expect(failure.settlementFailures).toHaveLength(2)
    expect(failure.settlementFailures).toEqual([
      expect.objectContaining({ fact: expect.objectContaining({ stage: 'settlement' }) }),
      expect.objectContaining({ fact: expect.objectContaining({ stage: 'settlement' }) }),
    ])
    expect(JSON.stringify(failure.settlementFailures)).not.toContain('fixture')
  })
})

function manualSettlementDeadline(): V2OutputSettlementDeadline & Readonly<{
  expire(): void
}> {
  let expiration: (() => void) | undefined
  return Object.freeze({
    schedule: (_delayMilliseconds: number, expire: () => void) => {
      if (expiration !== undefined) throw new Error('test deadline was scheduled twice')
      expiration = expire
      return Object.freeze({ cancel: () => undefined })
    },
    expire: () => {
      if (expiration === undefined) throw new Error('test deadline is not scheduled')
      expiration()
    },
  })
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(complete => { resolve = complete })
  return Object.freeze({ promise, resolve })
}

async function withExternalBound<T>(operation: Promise<T>, milliseconds: number): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined
  const timeout = new Promise<never>((_resolve, reject) => {
    timer = setTimeout(() => reject(new Error('settlement exceeded external test bound')), milliseconds)
  })
  try {
    return await Promise.race([operation, timeout])
  } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
}
