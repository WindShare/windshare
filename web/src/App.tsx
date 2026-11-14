import { useRef, useSyncExternalStore } from 'react'
import { PortalApp } from './ui/portal/PortalApp'
import { DetailSheet } from './ui/controls/DetailSheet'
import { PortalIcon } from './ui/portal/PortalIcon'
import { usePortalCopy } from './ui/portal/locales/use-portal-copy'
import type { DocumentOwner, DocumentSnapshot } from './document/owner'
import './document/document.css'

export default function App({ owner }: { readonly owner: DocumentOwner }) {
  const snapshot = useSyncExternalStore(owner.subscribe, owner.getSnapshot, owner.getSnapshot)
  if (snapshot.view === 'receiver' && snapshot.runtime !== null) return <snapshot.runtime.View />
  if (snapshot.view === 'opening') return <main className="document-opening">
    <a href="/">WindShare</a>
    <h1>Opening your share</h1>
    <StartupStatus owner={owner} snapshot={snapshot} />
  </main>
  return <PortalApp controller={owner} downloads={<DocumentDownloads owner={owner} snapshot={snapshot} />} />
}

function StartupStatus({ owner, snapshot }: { readonly owner: DocumentOwner; readonly snapshot: DocumentSnapshot }) {
  return snapshot.failed ? <div role="alert">
    <p>WindShare could not open. Try again, or reopen the original sharing link.</p>
    <button className="portal-btn-primary" type="button" onClick={() => owner.retry()}>Try again</button>
  </div> : <p role="status">Loading&hellip;</p>
}

function DocumentDownloads({ owner, snapshot }: { readonly owner: DocumentOwner; readonly snapshot: DocumentSnapshot }) {
  const t = usePortalCopy()
  const invoker = useRef<HTMLButtonElement>(null)
  if (snapshot.runtime !== null) return <snapshot.runtime.Downloads open={snapshot.downloadsOpen} onOpenChange={owner.setDownloadsOpen}
    entryLabel={t.downloads} entryDescription={t.downloadsDescription} />
  return <>
    <button ref={invoker} className="downloads-entry" type="button" onClick={() => owner.setDownloadsOpen(true)}>
      <PortalIcon name="download" />{t.downloads}
    </button>
    {snapshot.downloadsOpen && <DetailSheet title={t.downloads} onClose={() => owner.setDownloadsOpen(false)}
      returnFocus={invoker} returnLabel="Close downloads">
      <StartupStatus owner={owner} snapshot={snapshot} />
    </DetailSheet>}
  </>
}
