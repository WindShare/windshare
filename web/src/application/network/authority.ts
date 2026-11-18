import { AuthorityBlockFeed, type AuthorityConfig, type AuthorityObservation } from '../../network/authority/feed'
import { canonicalRelayIdentity } from '../../network/relay-endpoint'
import type { NetworkDirectory } from '../../network/directory'
import { RelayPolicyDeniedError, type RelayAdmission } from '../../receiver/relay-admission'

export class OfficialRelayAuthority {
  readonly #feed: AuthorityBlockFeed
  readonly #url: string
  constructor(config: AuthorityConfig, observe?: (event: AuthorityObservation) => void) {
    this.#url = config.url
    this.#feed = new AuthorityBlockFeed({ ...config, ...(observe ? { observe } : {}) })
  }
  admission(share: string, directory: NetworkDirectory): RelayAdmission {
    // Snapshot public identity only. The capability's read secret never enters
    // the authority client, policy cache or manual report URL.
    const official = (base: string) => directory.snapshot?.nodes.some(node =>
      node.identity === canonicalRelayIdentity(base)) ?? false
    return {
      prepare: async base => {
        await directory.ready.catch(() => undefined)
        if (official(base)) await this.#feed.ready
      },
      refusal: base => official(base) && this.#feed.blocked(share) ? new RelayPolicyDeniedError() : undefined,
      observe: listener => this.#feed.observe(listener),
    }
  }
  reportURL(share: string): string {
    const url = new URL('/report', this.#url)
    url.hash = new URLSearchParams({ share }).toString()
    return url.href
  }
  close(): void { this.#feed.close() }
}
