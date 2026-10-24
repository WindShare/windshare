import type { CatalogScanProgress } from '../../catalog/client'
import type { ReceiverConnectionSnapshot } from '../../receiver/connection-state'
import type { ReceiverPathActivitySnapshot } from '../../receiver/path-activity'
import type { JoinedBrowserShare } from '../../application/browser-share/gateway'

interface JoinedShareObservationOptions {
  readonly ownsJoinedShare: (joined: JoinedBrowserShare) => boolean
  readonly isJoining: () => boolean
  readonly onConnection: (connection: ReceiverConnectionSnapshot) => void
  readonly onPathActivity: (pathActivity: ReceiverPathActivitySnapshot) => void
  readonly onCatalogScanProgress: (joined: JoinedBrowserShare, progress: CatalogScanProgress) => void
  readonly onProtocolGeneration: (joined: JoinedBrowserShare) => void
}

export class JoinedShareObservation {
  readonly #options: JoinedShareObservationOptions
  #unsubscribeScanProgress: (() => void) | undefined
  #unsubscribeProtocolGeneration: (() => void) | undefined
  #unsubscribePathActivity: (() => void) | undefined
  #unsubscribeConnection: (() => void) | undefined

  constructor(options: JoinedShareObservationOptions) {
    this.#options = options
  }

  observe(joined: JoinedBrowserShare): void {
    this.suspendForJoin()
    this.#unsubscribeConnection?.()
    this.#unsubscribeConnection = joined.subscribeConnection(connection => {
      if (this.#options.ownsJoinedShare(joined)) this.#options.onConnection(connection)
    })
    this.#unsubscribeScanProgress = joined.subscribeCatalogScanProgress(
      progress => this.#options.onCatalogScanProgress(joined, progress),
    )
    this.#unsubscribePathActivity = joined.subscribePathActivity(pathActivity => {
      if (this.#options.ownsJoinedShare(joined)) this.#options.onPathActivity(pathActivity)
    })
    this.#unsubscribeProtocolGeneration = joined.subscribeProtocolGeneration(() => {
      if (this.#options.ownsJoinedShare(joined) && !this.#options.isJoining()) {
        this.#options.onProtocolGeneration(joined)
      }
    })
  }

  suspendForJoin(): void {
    // The previous share stays alive until replacement commits, so keep its
    // connection visible without letting its catalog or path update the new draft.
    this.#unsubscribeScanProgress?.()
    this.#unsubscribeScanProgress = undefined
    this.#unsubscribeProtocolGeneration?.()
    this.#unsubscribeProtocolGeneration = undefined
    this.#unsubscribePathActivity?.()
    this.#unsubscribePathActivity = undefined
  }

  close(): void {
    this.suspendForJoin()
    this.#unsubscribeConnection?.()
    this.#unsubscribeConnection = undefined
  }
}
