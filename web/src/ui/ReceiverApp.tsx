import { useState, useSyncExternalStore } from 'react'
import type { ReceiverController } from './controller'
import { WindShareMark } from './brand/WindShareMark'
import { usePreferences } from './preferences/use-preferences'
import type { Appearance } from './preferences/model'
import { ReceiverHeading } from './receiver-presentation/ReceiverHeading'
import { ReceiverBrowsing } from './receiver-presentation/ReceiverBrowsing'
import { ReceiverIcon, type ReceiverIconName } from './receiver-presentation/ReceiverIcon'
import { ReportLink, type ShareReportSource } from './receiver-presentation/ReportLink'
import { ReceiverActivity, ReceiverDownloads, ReceiverPrimarySaving } from './receiver-presentation/ReceiverTasks'

const APPEARANCE_ORDER: readonly Appearance[] = ['system', 'light', 'dark']
const APPEARANCE_META: Readonly<Record<Appearance, { readonly icon: ReceiverIconName; readonly label: string }>> = {
  system: { icon: 'monitor', label: 'Appearance: System' },
  light: { icon: 'sun', label: 'Appearance: Light' },
  dark: { icon: 'moon', label: 'Appearance: Dark' },
}

function AppearanceToggle() {
  const { appearance, update } = usePreferences()
  const next = APPEARANCE_ORDER[(APPEARANCE_ORDER.indexOf(appearance) + 1) % APPEARANCE_ORDER.length] ?? 'system'
  const meta = APPEARANCE_META[appearance]
  return <button className="appearance-toggle" type="button" aria-label={meta.label} title={meta.label}
    onClick={() => update({ appearance: next })}>
    <ReceiverIcon name={meta.icon} />
  </button>
}

export function ReceiverApp({ controller, report }: { readonly controller: ReceiverController; readonly report?: ShareReportSource }) {
  const source = controller.presentation.shareKind
  const kind = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const [downloadsOpen, setDownloadsOpen] = useState(false)
  return <main className={`receiver-shell receiver-content-${kind}`}>
    <header className="receiver-header">
      <div className="receiver-header-inner">
        <a className="brand" href="/" aria-label="WindShare home"><WindShareMark size={32} /><span>WindShare</span></a>
        <div className="receiver-header-actions">
          {report && <ReportLink source={report} />}
          <AppearanceToggle />
          <ReceiverDownloads controller={controller} open={downloadsOpen} onOpenChange={setDownloadsOpen} />
        </div>
      </div>
    </header>
    <div className="share-workspace">
      <ReceiverHeading controller={controller} actions={<ReceiverPrimarySaving controller={controller} />} />
      <ReceiverActivity controller={controller} openDownloads={() => setDownloadsOpen(true)} />
      <ReceiverBrowsing controller={controller} />
    </div>
  </main>
}
