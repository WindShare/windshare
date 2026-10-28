import type { KeyboardEvent } from 'react'
import type { ReceiverController } from '../../controller'
import { PortalIcon } from '../PortalIcon'
import { usePortalCopy } from '../locales/use-portal-copy'
import { ReceiveForm } from './ReceiveForm'
import { ClientDownloads } from './ClientDownloads'
import { ShareGuide } from './ShareGuide'
import { CONSOLE_TABS, type PortalTab } from './tabs'

export function ShareConsole({ controller, activeTab, onTabChange }: {
  readonly controller: ReceiverController
  readonly activeTab: PortalTab
  readonly onTabChange: (tab: PortalTab) => void
}) {
  const t = usePortalCopy()
  const navigateTabs = (event: KeyboardEvent<HTMLDivElement>) => {
    const index = CONSOLE_TABS.findIndex(tab => tab.id === activeTab)
    const last = CONSOLE_TABS.length - 1
    const next = { ArrowRight: (index + 1) % CONSOLE_TABS.length,
      ArrowLeft: (index + last) % CONSOLE_TABS.length, Home: 0, End: last }[event.key]
    if (next === undefined) return
    const tab = CONSOLE_TABS[next]
    if (!tab) return
    event.preventDefault()
    onTabChange(tab.id)
    event.currentTarget.querySelector<HTMLButtonElement>(`#portal-tab-${tab.id}`)?.focus()
  }

  return (
    <div className="portal-console-card" id="cli">
      <div className="portal-console-tabs" role="tablist" aria-label={t.consoleLabel} onKeyDown={navigateTabs}>
        {CONSOLE_TABS.map(tab => <button key={tab.id} type="button" role="tab"
          id={`portal-tab-${tab.id}`} aria-controls={`portal-panel-${tab.id}`}
          aria-selected={activeTab === tab.id} tabIndex={activeTab === tab.id ? 0 : -1}
          className="portal-tab-btn" onClick={() => onTabChange(tab.id)}>
          <PortalIcon name={tab.icon} size={17} /><span>{t[tab.label]}</span>
        </button>)}
      </div>
      <div className="portal-console-panels">
        {CONSOLE_TABS.map(tab => <div key={tab.id} role="tabpanel" className="portal-tab-panel"
          id={`portal-panel-${tab.id}`} aria-labelledby={`portal-tab-${tab.id}`}
          aria-hidden={activeTab !== tab.id} inert={activeTab !== tab.id}>
          {tab.id === 'receive' && <ReceiveForm controller={controller} />}
          {tab.id === 'cli' && <ShareGuide />}
          {tab.id === 'download' && <ClientDownloads />}
        </div>)}
      </div>
    </div>
  )
}
