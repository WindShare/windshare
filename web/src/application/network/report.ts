import { canonicalRelayIdentity } from '../../network/contracts/relay-endpoint'
import type { NetworkDirectory } from '../../network/directory'
import type { RelayCandidates } from '../../receiver/relay-selection/candidates'

type RelayDirectory = Pick<NetworkDirectory, 'snapshot' | 'observeSnapshot'>
type RelayCandidateStream = Pick<RelayCandidates, 'observe'>

function directoryHasRelay(directory: RelayDirectory, identity: string): boolean {
  return directory.snapshot?.nodes.some(node => node.identity === identity) ?? false
}

export interface ShareReport {
  readonly url: string | undefined
  subscribe(listener: () => void): () => void
  close(): void
}

export class OfficialShareReporter {
  readonly #origin: string

  constructor(endpoint: string) {
    const url = new URL(endpoint)
    const host = url.hostname.replace(/\.$/, '')
    if (url.protocol !== 'https:' || url.username || url.password || /[?#]/.test(url.href) ||
        host === 'workers.dev' || host.endsWith('.workers.dev')) throw new TypeError('Invalid authority endpoint')
    this.#origin = url.origin
  }

  track(
    share: string,
    candidates: RelayCandidateStream,
    directory: RelayDirectory,
  ): ShareReport {
    let url: string | undefined
    let closed = false
    const listeners = new Set<() => void>()
    const stop = this.#watchReport(share, candidates, directory, next => {
      if (closed || url === next) return
      url = next
      for (const listener of listeners) listener()
    })
    return {
      get url() { return url },
      subscribe(listener: () => void) {
        if (closed) return () => undefined
        listeners.add(listener)
        return () => { listeners.delete(listener) }
      },
      close() {
        if (closed) return
        closed = true
        url = undefined
        stop()
        listeners.clear()
      },
    }
  }

  #watchReport(
    share: string,
    candidates: RelayCandidateStream,
    directory: RelayDirectory,
    onAvailable: (url: string) => void,
  ): () => void {
    let identities: readonly string[] = []
    let closed = false
    let stopCandidates: (() => void) | undefined
    const stop = () => {
      if (closed) return
      closed = true
      stopDirectory()
      stopCandidates?.()
    }
    const classify = () => {
      if (closed || !identities.some(identity => directoryHasRelay(directory, identity))) return
      // Later directory changes must not hide reporting for an already identified share.
      stop()
      onAvailable(this.reportURL(share))
    }
    const stopDirectory = directory.observeSnapshot(classify)
    const unsubscribeCandidates = candidates.observe(bases => {
      identities = bases.map(canonicalRelayIdentity)
      classify()
    })
    if (closed) unsubscribeCandidates()
    else stopCandidates = unsubscribeCandidates
    return stop
  }

  reportURL(share: string): string {
    const url = new URL('/report', this.#origin)
    url.hash = new URLSearchParams({ share }).toString()
    return url.href
  }
}
