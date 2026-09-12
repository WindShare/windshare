import { describe, expect, it } from 'vitest'
import { wireFixture } from './lease-retirement-wire-fixture'
import { V2_MESSAGE_KIND } from '../../src/session/v2-message'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import { deferred } from '../session/v2-send-fixture'
import { catalogFixture, identity, planAuthorityFixture, receiveIntentFixture, testOutput, transferJobFixture } from './v2-job-fixture'
import { disabledOutputExecutionProfile } from '../../src/transfer/output-session'
import type { V2BlockRangeReader } from '../../src/content/v2-broker'

type Scenario = 'healthy' | 'relay-loss' | 'relay-loss-before-release' | 'invalid-release-completion'
const FILE_CONCURRENCY = 2

async function runDownload(scenario: Scenario) {
  const beforeRelease = deferred<void>()
  const allowRelease = deferred<void>()
  const wire = await wireFixture({
    ...(scenario === 'relay-loss-before-release' ? {
      beforeLeaseRelease: () => { beforeRelease.resolve(); return allowRelease.promise },
    } : {}),
    ...(scenario === 'invalid-release-completion' ? { releaseCompletion: 'invalid' } : {}),
  })
  const siblingReading = deferred<void>()
  const siblingContinue = deferred<void>()
  const progress: unknown[] = []
  const transitions: unknown[] = []
  const output = testOutput([], {
    durability: 'ProcessRestart', executionProfile: disabledOutputExecutionProfile(FILE_CONCURRENCY),
  })
  const plans = planAuthorityFixture({ output })
  let siblingAborted = false
  const broker: V2BlockRangeReader = {
    readRange: async function* (descriptor, _lease, range, request) {
      if (descriptor.fileIdText === wire.files[1]!.idText) {
        siblingReading.resolve()
        const signal = request?.signal
        const onAbort = () => { siblingAborted = true; siblingContinue.reject(signal?.reason) }
        signal?.addEventListener('abort', onAbort, { once: true })
        try { signal?.throwIfAborted(); await siblingContinue.promise }
        finally { signal?.removeEventListener('abort', onAbort) }
      }
      yield { offset: range.start, data: new Uint8Array(Number(range.end - range.start)).fill(7), authenticatedRoute: 'direct' }
    },
  }
  let running: Promise<unknown> | undefined
  try {
    const selection = new V2SelectionPolicy(true)
    const catalog = catalogFixture([{ id: identity(2), entries: wire.files }])
    const intent = await receiveIntentFixture({ planKind: 'workspace-then-publish', artifactKind: 'zip-archive', selection })
    const job = transferJobFixture({
      catalog: catalog.catalog, selection, intent, plans, revisions: wire.scoped.revisions, broker,
      chunkSize: wire.share.chunkSize, maximumConcurrentFiles: FILE_CONCURRENCY,
      onProgress: value => { progress.push(value) }, trace: { current: event => { transitions.push(event) } },
    })
    const task = job.run()
    running = task
    const trigger = scenario === 'relay-loss-before-release' ? beforeRelease.promise : wire.releaseReached.promise
    await Promise.race([Promise.all([trigger, siblingReading.promise]),
      task.then(() => { throw new Error('Job ended before reproduction trigger') })])
    expect(output.commits).toEqual([wire.files[0]!.idText])
    expect(wire.runtime.laneIds()).toEqual([1, 2])
    expect([...wire.relay.errors, ...wire.direct.errors]).toEqual([])

    if (scenario === 'relay-loss-before-release') {
      const detached = deferred<void>()
      const unsubscribe = wire.runtime.subscribeLaneChanges(change => {
        if (change.type === 'detached' && change.laneId === 1) detached.resolve()
      })
      try { await wire.relay.close(); await detached.promise } finally { unsubscribe() }
      allowRelease.resolve()
      await wire.releaseReached.promise
      expect(wire.relay.requests.some(message => message.kind === V2_MESSAGE_KIND.releaseLease)).toBe(false)
      expect(wire.direct.requests.some(message => message.kind === V2_MESSAGE_KIND.releaseLease)).toBe(true)
      await wire.completeRelease()
      siblingContinue.resolve()
    } else {
      expect(wire.relay.requests.some(message => message.kind === V2_MESSAGE_KIND.releaseLease)).toBe(true)
      if (scenario === 'relay-loss') {
        await wire.relay.close()
        // A real authenticated exchange proves the remaining route before the
        // independent read finishes; the original bug canceled this read first.
        const directProbe = await wire.revisions.open(wire.files[0]!.id, {
          active: true, allows: route => route === 'direct', assertActive: () => undefined, subscribe: () => () => undefined,
        })
        await directProbe.release()
        siblingContinue.resolve()
      } else { await wire.completeRelease(); siblingContinue.resolve() }
    }
    const result = await task
    expect(wire.runtime.isClosed).toBe(false)

    // A successful authenticated exchange after job settlement rules out total
    // connectivity loss as the explanation for a paused download.
    const probe = await wire.revisions.open(wire.files[0]!.id, {
      active: true, allows: route => route === 'direct', assertActive: () => undefined, subscribe: () => () => undefined,
    })
    await probe.release()
    expect(wire.direct.requests.some(message => message.kind === V2_MESSAGE_KIND.openRevisions)).toBe(true)
    expect(wire.runtime.isClosed).toBe(false)
    expect([...wire.relay.errors, ...wire.direct.errors]).toEqual([])

    const evidence = {
      scenario, worker: result.worker, lifecycle: result.lifecycle, failureTrigger: result.failureTrigger,
      liveProtocolLanes: wire.runtime.laneIds(), siblingAborted, directProbeSucceeded: true,
      committedFiles: output.commits, bytesWritten: output.writes.reduce((sum, write) => sum + write.bytes, 0),
      progress, transitions, protocolEvents: wire.events, retirements: wire.retirements,
    }
    return evidence
  } finally {
    allowRelease.resolve()
    siblingContinue.resolve()
    await wire.close()
    await running?.catch(() => undefined)
  }
}

describe('lease retirement through the protocol runtime and TransferJob', () => {
  it.each<Scenario>(['healthy', 'relay-loss-before-release'])('%s control commits both files', async scenario => {
    const evidence = await runDownload(scenario)
    expect(evidence.worker.status).toBe('Succeeded')
    expect(evidence.committedFiles).toHaveLength(2)
    expect(evidence.siblingAborted).toBe(false)
  })

  it('replays an uncertain retirement over direct without canceling another file', async () => {
    const evidence = await runDownload('relay-loss')
    expect(evidence.liveProtocolLanes).toEqual([2])
    expect(evidence.worker.status).toBe('Succeeded')
    expect(evidence.committedFiles).toHaveLength(2)
    expect(evidence.siblingAborted).toBe(false)
    const retry = evidence.retirements.find(event => event.transition === 'retrying')
    expect(retry).toMatchObject({ attempt: 1 })
    expect(evidence.retirements).toContainEqual({ leaseId: retry!.leaseId, attempt: 2, transition: 'released' })
  })

  it('keeps an unconfirmed remote cleanup separate from completed file output', async () => {
    const evidence = await runDownload('invalid-release-completion')
    expect(evidence.worker.status).toBe('Succeeded')
    expect(evidence.committedFiles).toHaveLength(2)
    expect(evidence.siblingAborted).toBe(false)
    expect(evidence.retirements.filter(event => event.transition === 'abandoned')).toEqual(
      expect.arrayContaining([expect.objectContaining({ reason: 'remote_failure', attempt: 1 })]))
    expect(evidence.retirements.some(event => event.transition === 'retrying')).toBe(false)
  })

  it('keeps generation disposal during shared-read cleanup out of the file result', async () => {
    const entered = deferred<void>()
    const leave = deferred<void>()
    const wire = await wireFixture({ beforeLeaseRelease: () => { entered.resolve(); return leave.promise } })
    try {
      const opened = await wire.scoped.revisions.open(wire.files[0]!.id)
      const releasing = opened.release()
      await entered.promise
      wire.revisions.close()
      await expect(releasing).resolves.toBeUndefined()
      expect(wire.retirements.at(-1)).toMatchObject({ transition: 'abandoned', reason: 'service_closed' })
    } finally { leave.resolve(); await wire.close() }
  })
})
