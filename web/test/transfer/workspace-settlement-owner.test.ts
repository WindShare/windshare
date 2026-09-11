import { describe, expect, it, vi } from 'vitest'
import type { ReceiveLifecycleState } from '../../src/output/workspace/state'
import { WorkspaceSettlementOwner } from '../../src/transfer/settlement/workspace-settlement-owner'
import { withDurableLifecycleSettlementTimeout } from '../../src/transfer/settlement/v2-output'
import { deferred, manualSettlementDeadline } from './settlement-deadline'

const stable: ReceiveLifecycleState = {
  kind: 'waiting-to-save', operationId: 'operation', receiveIntentDigest: 'intent', generation: 2n, packageDigest: 'package',
}

describe('workspace terminal ownership', () => {
  it.each([false, true])('serializes a concurrent pause behind completion (completion failure: %s)', async fail => {
    const entered = deferred()
    const release = deferred()
    const settle = vi.fn(async () => {
      entered.resolve()
      await release.promise
      if (fail) throw new Error('finalization failed')
      return stable
    })
    const pause = vi.fn(async () => stable)
    const owner = new WorkspaceSettlementOwner()
    const finishing = owner.settle(settle)
    const observed = finishing.catch(() => undefined)
    await entered.promise
    const pausing = owner.pause(pause)
    expect(owner.pause(pause)).toBe(pausing)
    expect(pause).not.toHaveBeenCalled()
    release.resolve()
    await observed
    expect(await pausing).toBe(stable)
    expect(settle).toHaveBeenCalledOnce()
    expect(pause).toHaveBeenCalledTimes(fail ? 1 : 0)
    expect(await owner.settle(settle)).toBe(stable)
    expect(settle).toHaveBeenCalledOnce()
  })

  it('drains a slow pause without replacing its committed lifecycle with unknown ownership', async () => {
    const entered = deferred()
    const release = deferred()
    const deadline = manualSettlementDeadline()
    const owner = new WorkspaceSettlementOwner()
    const running = withDurableLifecycleSettlementTimeout('pause workspace', 1,
      () => owner.pause(async () => { entered.resolve(); await release.promise; return stable }), deadline)
    let exposed = false
    const observation = running.then(() => { exposed = true })
    await entered.promise
    deadline.expire()
    await Promise.resolve()
    expect(exposed).toBe(false)
    release.resolve()
    expect(await running).toBe(stable)
    await observation
  })
})
