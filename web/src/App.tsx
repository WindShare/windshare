import { LanguageSelect } from './ui/preferences/LanguageSelect'
import { shellCopy } from './ui/locales/shell'
import { useCopy } from './ui/preferences/use-preferences'
import { useRef, useSyncExternalStore } from 'react'
import { PortalApp } from './ui/portal/PortalApp'
import { DetailSheet } from './ui/controls/DetailSheet'
import { PortalIcon } from './ui/portal/PortalIcon'
import { usePortalCopy } from './ui/portal/locales/use-portal-copy'
import type { DocumentOwner, DocumentSnapshot } from './document/owner'
import './document/document.css'

export default function App({ owner }: { readonly owner: DocumentOwner }) {
  const t = useCopy(shellCopy)
  const snapshot = useSyncExternalStore(owner.subscribe, owner.getSnapshot, owner.getSnapshot)
  if (snapshot.view === 'receiver' && snapshot.runtime !== null) return <snapshot.runtime.View />
  if (snapshot.view === 'opening') return <main className="document-opening">
    <header><a href="/">WindShare</a><LanguageSelect /></header>
    <h1>{t.openingShare}</h1>
    <StartupStatus owner={owner} snapshot={snapshot} />
  </main>
  return <PortalApp controller={owner} downloads={<DocumentDownloads owner={owner} snapshot={snapshot} />} />
}

function StartupStatus({ owner, snapshot }: { readonly owner: DocumentOwner; readonly snapshot: DocumentSnapshot }) {
  const t = useCopy(shellCopy)
  return snapshot.failed ? <div role="alert">
    <p>{t.openFailed}</p>
    <button className="portal-btn-primary" type="button" onClick={() => owner.retry()}>{t.retry}</button>
  </div> : <p role="status">{t.loading}</p>
}

function DocumentDownloads({ owner, snapshot }: { readonly owner: DocumentOwner; readonly snapshot: DocumentSnapshot }) {
  const shell = useCopy(shellCopy)
  const t = usePortalCopy()
  const invoker = useRef<HTMLButtonElement>(null)
  if (snapshot.runtime !== null) return <snapshot.runtime.Downloads open={snapshot.downloadsOpen} onOpenChange={owner.setDownloadsOpen}
    entryLabel={t.downloads} entryDescription={t.downloadsDescription} />
  return <>
    <button ref={invoker} className="downloads-entry" type="button" onClick={() => owner.setDownloadsOpen(true)}>
      <PortalIcon name="download" />{t.downloads}
    </button>
    {snapshot.downloadsOpen && <DetailSheet title={t.downloads} onClose={() => owner.setDownloadsOpen(false)}
      returnFocus={invoker} returnLabel={shell.closeDownloads}>
      <StartupStatus owner={owner} snapshot={snapshot} />
    </DetailSheet>}
  </>
}
