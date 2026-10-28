import { PortalIcon } from './PortalIcon'
import { PORTAL_LINKS, PORTAL_SECTIONS } from './navigation'
import { usePortalCopy } from './locales/use-portal-copy'

export function SelfHostSection() {
  const t = usePortalCopy()
  return (
    <section id={PORTAL_SECTIONS.selfHost.id} className="portal-section portal-selfhost" aria-labelledby="portal-selfhost-title">
      <div className="portal-container">
        <div className="portal-selfhost-box">
          <div className="portal-selfhost-copy">
            <span className="portal-section-tag">{t.selfHostTag}</span>
            <h2 id="portal-selfhost-title">{t.selfHostTitle}</h2>
            <p>{t.selfHostDescription}</p>
            <div className="portal-selfhost-actions">
              <a href={PORTAL_LINKS.deployment} target="_blank" rel="noopener noreferrer" className="portal-button">
                {t.selfHostGuide}<PortalIcon name="arrow" size={17} />
              </a>
              <a href={PORTAL_LINKS.cloudflare} target="_blank" rel="noopener noreferrer" className="portal-text-link">{t.cloudflareGuide}<PortalIcon name="arrow" size={16} /></a>
            </div>
          </div>
          <div className="portal-selfhost-principles">
            <div><PortalIcon name="github" /><span><strong>{t.principleOpen}</strong><small>{t.principleOpenDescription}</small></span></div>
            <div><PortalIcon name="lock" /><span><strong>{t.principlePrivate}</strong><small>{t.principlePrivateDescription}</small></span></div>
            <div><PortalIcon name="globe" /><span><strong>{t.principleHost}</strong><small>{t.principleHostDescription}</small></span></div>
          </div>
        </div>
      </div>
    </section>
  )
}
