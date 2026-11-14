import { useState, useSyncExternalStore } from 'react'
import type { ReceiverController } from './controller'
import { WindShareMark } from './brand/WindShareMark'
import { ReceiverHeading } from './receiver-presentation/ReceiverHeading'
import { ReceiverBrowsing } from './receiver-presentation/ReceiverBrowsing'
import { ReceiverTasks, ReceiverDownloads } from './receiver-presentation/ReceiverTasks'

export function ReceiverApp({ controller }: { readonly controller: ReceiverController }) {
  const source = controller.presentation.shareKind
  const kind = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const [downloadsOpen, setDownloadsOpen] = useState(false)
  return <main className={`receiver-shell receiver-content-${kind}`}>
    <header className="receiver-header">
      <a className="brand" href="/" aria-label="WindShare home"><WindShareMark size={36} /><span>WindShare</span></a>
      <div className="receiver-header-actions"><ReceiverDownloads controller={controller} open={downloadsOpen} onOpenChange={setDownloadsOpen} /></div>
    </header>
    <div className="share-workspace">
      <ReceiverHeading controller={controller} />
      <ReceiverBrowsing controller={controller} />
      <ReceiverTasks controller={controller} openDownloads={() => setDownloadsOpen(true)} />
    </div>
  </main>
}
