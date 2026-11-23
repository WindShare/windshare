import { LanguageSelect } from './preferences/LanguageSelect'
import { shellCopy } from './locales/shell'
import { useState, useSyncExternalStore } from 'react'
import type { ReceiverController } from './controller'
import { WindShareMark } from './brand/WindShareMark'
import { useCopy, usePreferences } from './preferences/use-preferences'
import type { Appearance } from './preferences/model'
import { ReceiverHeading } from './receiver-presentation/ReceiverHeading'
import { ReceiverBrowsing } from './receiver-presentation/ReceiverBrowsing'
import { ReceiverIcon, type ReceiverIconName } from './receiver-presentation/ReceiverIcon'
import { ReportLink, type ShareReportSource } from './receiver-presentation/ReportLink'
import { ReceiverActivity, ReceiverDownloads, ReceiverPrimarySaving } from './receiver-presentation/ReceiverTasks'

const APPEARANCE_ORDER: readonly Appearance[] = ['system', 'light', 'dark']
const APPEARANCE_META = {
  system: { icon: 'monitor', label: 'appearanceSystem' },
  light: { icon: 'sun', label: 'appearanceLight' },
  dark: { icon: 'moon', label: 'appearanceDark' },
} as const satisfies Record<Appearance, { icon: ReceiverIconName; label: keyof typeof shellCopy.en }>

function AppearanceToggle() {
  const { language, appearance, update } = usePreferences()
  const next = APPEARANCE_ORDER[(APPEARANCE_ORDER.indexOf(appearance) + 1) % APPEARANCE_ORDER.length] ?? 'system'
  const meta = APPEARANCE_META[appearance]
  return <button className="appearance-toggle" type="button" aria-label={shellCopy[language][meta.label]} title={shellCopy[language][meta.label]}
    onClick={() => update({ appearance: next })}>
    <ReceiverIcon name={meta.icon} />
  </button>
}

const EMPTY_DISMISSED_TASKS: ReadonlySet<string> = new Set()

export function ReceiverApp({ controller, report }: { readonly controller: ReceiverController; readonly report?: ShareReportSource }) {
  const t = useCopy(shellCopy)
  const source = controller.presentation.shareScope
  const { kind, visitId } = useSyncExternalStore(source.subscribe, source.getSnapshot, source.getSnapshot)
  const [downloadsOpen, setDownloadsOpen] = useState(false)
  // Scope dismissals to each monotonic share visit so A -> B -> A resets immediately with zero effect flash.
  const [dismissedState, setDismissedState] = useState<Readonly<{
    visitId: number
    keys: ReadonlySet<string>
  }>>(() => ({ visitId, keys: EMPTY_DISMISSED_TASKS }))
  const dismissedTaskKeys = dismissedState.visitId === visitId
    ? dismissedState.keys
    : EMPTY_DISMISSED_TASKS
  const dismissTask = (taskKey: string) => {
    setDismissedState(previous => {
      const current = previous.visitId === visitId ? previous.keys : EMPTY_DISMISSED_TASKS
      if (current.has(taskKey)) return previous
      const next = new Set(current)
      next.add(taskKey)
      return { visitId, keys: next }
    })
  }
  return <main className={`receiver-shell receiver-content-${kind}`}>
    <header className="receiver-header">
      <div className="receiver-header-inner">
        <a className="brand" href="/" aria-label={t.home}><WindShareMark size={32} /><span>WindShare</span></a>
        <div className="receiver-header-actions">
          {report && <ReportLink source={report} />}
          <LanguageSelect className="receiver-language" />
          <AppearanceToggle />
          <ReceiverDownloads controller={controller} open={downloadsOpen} onOpenChange={setDownloadsOpen} />
        </div>
      </div>
    </header>
    <div className="share-workspace">
      <ReceiverHeading controller={controller} actions={<ReceiverPrimarySaving controller={controller} dismissedTaskKeys={dismissedTaskKeys} />} />
      <ReceiverActivity
        controller={controller}
        openDownloads={() => setDownloadsOpen(true)}
        dismissedTaskKeys={dismissedTaskKeys}
        onDismissTask={dismissTask}
      />
      <ReceiverBrowsing controller={controller} />
    </div>
  </main>
}
