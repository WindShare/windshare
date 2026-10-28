import type { ReactNode } from 'react'
import type { ReceiverController } from '../controller'
import { PortalIcon } from './PortalIcon'
import { BrandScene } from './BrandScene'
import { ShareConsole } from './console/ShareConsole'
import type { PortalTab } from './console/tabs'
import { usePortalCopy } from './locales/use-portal-copy'

export function HeroSection({ controller, downloads, activeTab, onTabChange }: {
  readonly controller: ReceiverController
  readonly downloads: ReactNode
  readonly activeTab: PortalTab
  readonly onTabChange: (tab: PortalTab) => void
}) {
  const t = usePortalCopy()
  return (
    <section className="portal-hero" aria-labelledby="portal-title">
      <div className="portal-container">
        <div className="portal-hero-layout">
          <div className="portal-hero-copy">
            <p className="portal-eyebrow"><span />{t.eyebrow}</p>
            <h1 id="portal-title">{t.titleFirst}<br /><span>{t.titleSecond}</span></h1>
            <p className="portal-hero-subtitle">{t.subtitleFirst}<br />{t.subtitleSecond}</p>
            <div className="portal-hero-assurances">
              <span><PortalIcon name="shield" size={16} />{t.assurancePrivate}</span>
              <span><PortalIcon name="bolt" size={16} />{t.assuranceInstant}</span>
              <span><PortalIcon name="github" size={16} />{t.assuranceOpen}</span>
            </div>
            <BrandScene />
          </div>
          <div className="portal-console-area">
            <div className="portal-console-caption"><span>{t.consoleCaption}</span><PortalIcon name="lock" size={13} /></div>
            <ShareConsole controller={controller} activeTab={activeTab} onTabChange={onTabChange} />
            <div className="portal-downloads-utility">{downloads}<span>{t.downloadsHint}</span></div>
          </div>
        </div>
        <div className="portal-hero-footnote"><span>{t.footnote}</span><a href="#how-it-works">{t.howLink}<PortalIcon name="arrow" size={16} /></a></div>
      </div>
    </section>
  )
}
