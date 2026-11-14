import { describe, expect, it, vi } from 'vitest'
import { deferArtifactAuthority } from '../../src/application/browser-receive/deferred-authority'
import type { ArtifactPresentationAuthority, RouteCommitInput } from '../../src/application/receive/contracts'

function moduleGate() {
  let resolve!: (authority: ArtifactPresentationAuthority) => void
  const promise = new Promise<ArtifactPresentationAuthority>(yes => { resolve = yes })
  return { promise, resolve }
}

describe('deferred destination authority', () => {
  it('releases an acquired destination when its implementation arrives after cancellation', async () => {
    const module = moduleGate()
    const authority = deferArtifactAuthority(module.promise)
    const reason = new DOMException('Selection changed', 'AbortError')
    const release = vi.fn()
    const commit = vi.fn()
    const released = authority.release(reason)
    const attempt = authority.commit({ signal: new AbortController().signal } as RouteCommitInput)
    const rejected = expect(attempt).rejects.toMatchObject({ name: 'AbortError' })
    module.resolve({ ready: Promise.resolve(), release, commit })
    await released
    await rejected
    expect(release).toHaveBeenCalledExactlyOnceWith(reason)
    expect(commit).not.toHaveBeenCalled()
    await authority.release(reason)
    expect(release).toHaveBeenCalledTimes(1)
  })

  it('waits for the original picker authority without acquiring another destination', async () => {
    const module = moduleGate()
    let finishPicker!: () => void
    const picker = new Promise<void>(resolve => { finishPicker = resolve })
    const commit = vi.fn(async () => ({ kind: 'bound-operation' } as never))
    const authority = deferArtifactAuthority(module.promise)
    let ready = false
    void authority.ready.then(() => { ready = true })
    module.resolve({ ready: picker, commit, release: vi.fn() })
    await Promise.resolve()
    expect(ready).toBe(false)
    finishPicker()
    await authority.ready
    const input = { signal: new AbortController().signal } as RouteCommitInput
    await authority.commit(input)
    expect(commit).toHaveBeenCalledExactlyOnceWith(input)
  })
})
