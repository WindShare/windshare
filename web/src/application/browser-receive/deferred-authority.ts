import type { ArtifactPresentationAuthority } from '../receive/contracts'

/** Module readiness cannot reacquire a destination or outlive its activation lease. */
export function deferArtifactAuthority(work: Promise<ArtifactPresentationAuthority>): ArtifactPresentationAuthority {
  let released: { readonly reason: unknown } | undefined
  const ready = work.then(value => value.ready)
  ready.catch(() => undefined)
  return {
    ready,
    commit: async input => {
      const value = await work
      input.signal.throwIfAborted()
      if (released !== undefined) throw new DOMException('Output activation was released', 'AbortError')
      return value.commit(input)
    },
    release: reason => {
      if (released !== undefined) return
      released = { reason }
      // If already resolved, delivery still happens in this microtask. The commit fence
      // above rejects immediately; pending picker promises keep their own observers.
      return work.then(value => value.release(reason), () => undefined)
    },
  }
}
