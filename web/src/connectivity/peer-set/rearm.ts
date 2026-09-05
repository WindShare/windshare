import type { V2PeerRecoveryRearmSource } from './contract'

export class BrowserV2PeerRecoveryRearmSource implements V2PeerRecoveryRearmSource {
  subscribe(listener: () => void): () => void {
    const notify = () => {
      try {
        listener()
      } catch {
        // Browser event delivery must remain outside recovery policy authority.
      }
    }
    window.addEventListener('online', notify)
    const connection = browserNetworkConnection()
    connection?.addEventListener('change', notify)
    return () => {
      window.removeEventListener('online', notify)
      connection?.removeEventListener('change', notify)
    }
  }
}

function browserNetworkConnection(): EventTarget | undefined {
  return (navigator as Navigator & { readonly connection?: EventTarget }).connection
}
