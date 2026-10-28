import { useRef } from 'react'
import { WindShareMark } from '../brand/WindShareMark'
import { PortalIcon } from './PortalIcon'
import { PortalPreferences } from './PortalPreferences'
import { PortalPopover } from './PortalPopover'
import { PORTAL_LINKS, PORTAL_SECTIONS } from './navigation'
import { usePortalCopy } from './locales/use-portal-copy'

export function PortalHeader({ onShare }: { readonly onShare: () => void }) {
  const t = usePortalCopy()
  const menu = useRef<HTMLDivElement>(null)
  const links = Object.values(PORTAL_SECTIONS)
  const navigate = (id: string) => {
    if (id === PORTAL_SECTIONS.cli.id) onShare()
    menu.current?.hidePopover()
  }

  return (
    <header className="portal-header">
      <div className="portal-container portal-header-inner">
        <a href="/" className="portal-brand" aria-label={t.home}>
          <WindShareMark size={38} /><span>WindShare</span>
        </a>
        <nav className="portal-nav" aria-label={t.nav}>
          {links.map(section => <a key={section.id} href={`#${section.id}`}
            onClick={() => navigate(section.id)}>{t[section.labelKey]}</a>)}
        </nav>
        <div className="portal-header-actions">
          <a href={PORTAL_LINKS.repository} target="_blank" rel="noopener noreferrer"
            className="portal-button portal-repository-link" aria-label={t.repository}>
            <PortalIcon name="github" size={17} /><span>GitHub</span>
          </a>
          <PortalPreferences />
          <PortalPopover className="portal-mobile-nav" label={t.openNav} icon="menu" panelRef={menu}>
            <nav className="portal-mobile-links" aria-label={t.mobileNav}>
              {links.map(section => <a key={section.id} href={`#${section.id}`}
                onClick={() => navigate(section.id)}>{t[section.labelKey]}<PortalIcon name="arrow" size={16} /></a>)}
            </nav>
          </PortalPopover>
        </div>
      </div>
    </header>
  )
}
