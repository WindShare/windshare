import { afterEach, describe, expect, it, vi } from 'vitest'
import { BrowserReceiveApplication } from '../../src/application/receive/application'
import type { ReceivePlanningShare } from '../../src/application/receive/joined-share'
import {
  FakeJoinedShare, FakeReceiveComposition, WORKSPACE_ENVIRONMENT, deferred, turns, waitFor,
} from '../ui/v2-receiver-orchestration-fixture'

const applications: Array<BrowserReceiveApplication<ReceivePlanningShare>> = []
afterEach(async () => {
  await Promise.all(applications.splice(0).map(app => app.close(new DOMException('Test complete', 'AbortError'))))
})

async function fixture(onTaskCleared: () => void = () => undefined) {
  const joined = new FakeJoinedShare(true)
  const share = joined as unknown as ReceivePlanningShare
  const receive = new FakeReceiveComposition(WORKSPACE_ENVIRONMENT)
  const errors: unknown[] = []
  const app = new BrowserReceiveApplication({
    receive, currentJoinedShare: () => share, isDisposed: () => false,
    onProgress: () => undefined, onRetainedChanged: () => undefined,
    onTaskAdopted: () => undefined, onTaskCleared, onChanged: () => undefined,
    onBrowsingReady: () => undefined, onActionError: error => errors.push(error),
  })
  applications.push(app)
  app.tasks.observeConnection({ kind: 'connected' })
  app.beginProjection(share, 'observation-replacement')
  await waitFor(() => app.observations.getSnapshot().offers?.kind === 'artifact-actions')
  const offers = app.observations.getSnapshot().offers
  if (offers?.kind !== 'artifact-actions') throw new Error('Test output offer unavailable')
  const choose = () => app.chooseArtifact(offers.primary.choice.choiceId,
    { objectLabel: 'report.txt', createdAtMilliseconds: 1 }, 'automatic')
  return { app, joined, receive, share, errors, choose }
}

describe('assembled browser receive application', () => {
  it('starts the output authority in the caller stack and admits one transfer without a UI controller', async () => {
    const f = await fixture()
    const unsubscribe = f.app.observations.subscribe(() => { throw new Error('View detached') })
    let inClick = true
    f.receive.clickStack = () => inClick
    f.choose()
    inClick = false
    f.choose()
    expect(f.receive.authorityStartStacks).toEqual([true])
    await waitFor(() => f.joined.transferRuns.length === 1)
    expect(f.app.active).toBe(true)
    expect(f.app.operationId).toBe(f.receive.startedAuthorities[0]!.runtime!.intent.operationId)
    expect(f.errors).toEqual([])
    unsubscribe()
  })

  it('fences reentrant close immediately and waits for output detachment before settling', async () => {
    let reentrant: Promise<void> | undefined
    const reason = new DOMException('Application closed', 'AbortError')
    const f = await fixture(() => { reentrant = f.app.close(reason) })
    f.choose()
    await waitFor(() => f.joined.transferRuns.length === 1)
    const operation = f.receive.startedAuthorities[0]!.runtime!
    const detached = deferred<void>()
    const detach = vi.spyOn(operation, 'detach').mockImplementation(() => detached.promise)
    const closing = f.app.close(reason)
    expect(reentrant).toBe(closing)
    expect(f.app.close(reason)).toBe(closing)
    expect(f.app.tasks.startBlockedReason()).not.toBeNull()
    let completed = false
    closing.then(() => { completed = true })
    await waitFor(() => detach.mock.calls.length === 1)
    await turns()
    expect(completed).toBe(false)
    const environments = f.receive.environmentCalls
    f.app.beginProjection(f.share, 'observation-replacement')
    await f.app.loadRetained()
    f.choose()
    expect(f.receive.environmentCalls).toBe(environments)
    expect(f.receive.startedAuthorities).toHaveLength(1)
    expect(f.receive.retainedSignals).toHaveLength(0)
    detached.resolve()
    await closing
    expect(detach).toHaveBeenCalledTimes(1)
  })
})
