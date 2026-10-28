import { PortalIcon } from '../PortalIcon'
import { PORTAL_LINKS } from '../navigation'
import { usePortalCopy } from '../locales/use-portal-copy'

const CLIENT_PLATFORM_GROUPS = [
  { label: 'desktopDevices', icon: 'monitor', platforms: ['Windows', 'macOS', 'Linux'] },
  { label: 'mobileDevices', icon: 'smartphone', platforms: ['Android', 'iOS'] },
] as const

export function ClientDownloads() {
  const t = usePortalCopy()
  return (
    <div className="portal-client-downloads">
      <div className="portal-panel-heading">
        <h2>{t.clientsTitle}</h2>
        <p>{t.clientsDescription}</p>
      </div>
      <dl className="portal-client-platforms">
        {CLIENT_PLATFORM_GROUPS.map(group => <div className="portal-client-platform-group" key={group.label}>
          <dt><PortalIcon name={group.icon} size={18} /><span>{t[group.label]}</span></dt>
          <dd><ul>{group.platforms.map(platform => <li key={platform}>{platform}</li>)}</ul></dd>
        </div>)}
      </dl>
      <a className="portal-btn-primary portal-client-download" href={PORTAL_LINKS.releases}
        target="_blank" rel="noopener noreferrer">
        <PortalIcon name="download" size={18} />{t.downloadClient}
      </a>
      <div className="portal-client-resources">
        <a className="portal-text-link" href={PORTAL_LINKS.installation} target="_blank" rel="noopener noreferrer">
          {t.installGuide}<PortalIcon name="arrow" size={14} />
        </a>
        <a className="portal-text-link" href={PORTAL_LINKS.repository} target="_blank" rel="noopener noreferrer">
          <PortalIcon name="github" size={15} />{t.viewSource}
        </a>
      </div>
      <div className="portal-privacy-note"><PortalIcon name="globe" size={15} /><span>{t.recipientNoInstall}</span></div>
    </div>
  )
}
