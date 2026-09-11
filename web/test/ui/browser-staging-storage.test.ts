import { describe, expect, it } from 'vitest'
import { inspectBrowserStagingStorage } from '../../src/ui/browser-receive/fsa/staging-storage'

describe('browser staging storage observations', () => {
  it('keeps persistence denial separate from usable OPFS and observed quota', async () => {
    expect(await inspectBrowserStagingStorage({
      persisted: async () => false,
      estimate: async () => ({ quota: 1000, usage: 400 }),
    }, true)).toEqual({
      opfs: 'usable', persistence: 'not-persisted',
      quota: { kind: 'estimated', quotaBytes: 1000n, usageBytes: 400n },
      pressure: 'normal',
    })
  })

  it('keeps unknown observations separate from OPFS availability', async () => {
    expect(await inspectBrowserStagingStorage({
      persisted: async () => { throw new Error('permission observation unavailable') },
      estimate: async () => ({ quota: Number.POSITIVE_INFINITY, usage: 0 }),
    }, true)).toEqual({
      opfs: 'usable', persistence: 'unknown', quota: { kind: 'unknown' }, pressure: 'normal',
    })
  })

  it('does not mistake persistence permission for native OPFS support', async () => {
    const facts = await inspectBrowserStagingStorage({ persisted: async () => true }, false)
    expect(facts.opfs).toBe('unavailable')
    expect(facts.persistence).toBe('persisted')
    expect(facts.quota.kind).toBe('unknown')
  })

  it('observes abort after asynchronous browser facts finish', async () => {
    const controller = new AbortController()
    let finish!: (value: boolean) => void
    const pending = inspectBrowserStagingStorage({
      persisted: () => new Promise(resolve => { finish = resolve }),
    }, true, controller.signal)
    controller.abort()
    finish(false)
    await expect(pending).rejects.toMatchObject({ name: 'AbortError' })
  })
})
