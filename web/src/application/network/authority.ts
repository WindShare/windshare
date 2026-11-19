import { AuthorityBlockFeed, type AuthorityConfig, type AuthorityObservation } from '../../network/authority/feed'
import { canonicalRelayIdentity } from '../../network/relay-endpoint'
import type { NetworkDirectory } from '../../network/directory'
import { RelayPolicyDeniedError, type RelayAdmission } from '../../receiver/relay-admission'

type RelayDirectory = Pick<NetworkDirectory, 'snapshot' | 'observeSnapshot'>

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
          if (closed || stopFeed || !directory.snapshot?.nodes.some(node => node.identity === identity)) return
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
  reportURL(share: string): string {
    const url = new URL('/report', this.#url)
    url.hash = new URLSearchParams({ share }).toString()
    return url.href
  }
  close(): void { this.#feed.close() }
}
