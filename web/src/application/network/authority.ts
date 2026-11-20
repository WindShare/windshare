import { AuthorityBlockFeed, type AuthorityConfig, type AuthorityObservation } from '../../network/authority/feed'
import { canonicalRelayIdentity } from '../../network/relay-endpoint'
import type { NetworkDirectory } from '../../network/directory'
import { RelayPolicyDeniedError, type RelayAdmission } from '../../receiver/relay-admission'
import type { RelayCandidates } from '../../receiver/relay-selection/candidates'

type RelayDirectory = Pick<NetworkDirectory, 'snapshot' | 'observeSnapshot'>
type RelayCandidateStream = Pick<RelayCandidates, 'observe'>

function directoryHasRelay(directory: RelayDirectory, identity: string): boolean {
  return directory.snapshot?.nodes.some(node => node.identity === identity) ?? false
}

export class OfficialRelayAuthority {
  readonly #feed: AuthorityBlockFeed
  readonly #url: string
  constructor(config: AuthorityConfig, observe?: (event: AuthorityObservation) => void) {
    this.#url = config.url
    this.#feed = new AuthorityBlockFeed({ ...config, ...(observe ? { observe } : {}) })
  }
  admission(share: string, directory: RelayDirectory): RelayAdmission {
    // The capability's read secret never enters the authority client, policy cache or report URL.
    return {
      watch: (base, deny) => {
        const identity = canonicalRelayIdentity(base)
        let stopFeed: (() => void) | undefined
        let closed = false
        const enforce = () => {
          if (this.#feed.blocked(share)) deny(new RelayPolicyDeniedError())
        }
        const classify = () => {
          if (closed || stopFeed || !directoryHasRelay(directory, identity)) return
          // Membership can arrive after dialing starts. Keep confirmed membership
          // for the lifetime of this connection, even if the directory changes.
          stopFeed = this.#feed.observe(enforce)
          enforce()
        }
        const stopDirectory = directory.observeSnapshot(classify)
        classify()
        return {
          beforeDial: stopFeed ? this.#feed.ready : undefined,
          close: () => {
            if (closed) return
            closed = true
            stopDirectory()
            stopFeed?.()
          },
        }
      },
    }
  }
  watchReport(
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
      // Keep the report link once official membership is confirmed for this share,
      // matching admission so directory churn cannot hide reporting mid-session.
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
    const url = new URL('/report', this.#url)
    url.hash = new URLSearchParams({ share }).toString()
    return url.href
  }
  close(): void { this.#feed.close() }
}
