import { PortalIcon } from '../PortalIcon'
import { PORTAL_LINKS } from '../navigation'
import { usePortalCopy } from '../locales/use-portal-copy'
import {
  detectVisitorEnvironment,
  formatArchLabel,
  formatAssetLabel,
  formatDownloadAction,
  getReleaseDownloads,
  selectRecommendedAsset,
  type ReleaseDownloadsManifest,
} from './release-downloads'

const CLIENT_PLATFORM_GROUPS = [
  { label: 'desktopDevices', icon: 'monitor', platforms: ['Windows', 'macOS', 'Linux'] },
  { label: 'mobileDevices', icon: 'smartphone', platforms: ['Android', 'iOS'] },
] as const

export function ClientDownloads() {
  const t = usePortalCopy()
  const releaseDownloads = getReleaseDownloads()
  const recommended = releaseDownloads === undefined
    ? undefined
    : selectRecommendedAsset(releaseDownloads, detectVisitorEnvironment(), {
        preferredKind: 'desktop',
        allowOtherKind: true,
      })

  return (
    <div className="portal-client-downloads">
      <div className="portal-panel-heading">
        <h2>{t.clientsTitle}</h2>
        <p>{t.clientsDescription}</p>
      </div>
      {recommended !== undefined && (
        <a className="portal-btn-primary portal-client-download" href={recommended.href} download={recommended.filename}>
          <PortalIcon name="download" size={18} />
          <span>{formatDownloadAction(t, recommended)}</span>
        </a>
      )}
      {releaseDownloads === undefined
        ? <PlatformOverview />
        : <ReleaseAssetList manifest={releaseDownloads} />}
      {recommended === undefined && (
        <a className="portal-btn-primary portal-client-download" href={PORTAL_LINKS.releases}
          target="_blank" rel="noopener noreferrer">
          <PortalIcon name="download" size={18} />
          <span>{releaseDownloads === undefined ? t.downloadClient : t.allReleases}</span>
        </a>
      )}
      <div className="portal-client-resources">
        {recommended !== undefined && (
          <a className="portal-text-link" href={PORTAL_LINKS.releases} target="_blank" rel="noopener noreferrer">
            <span>{t.allReleases}</span><PortalIcon name="arrow" size={14} />
          </a>
        )}
        <a className="portal-text-link" href={PORTAL_LINKS.installation} target="_blank" rel="noopener noreferrer">
          <span>{t.installGuide}</span><PortalIcon name="arrow" size={14} />
        </a>
        <a className="portal-text-link" href={PORTAL_LINKS.repository} target="_blank" rel="noopener noreferrer">
          <PortalIcon name="github" size={15} /><span>{t.viewSource}</span>
        </a>
      </div>
      <div className="portal-privacy-note"><PortalIcon name="globe" size={15} /><span>{t.recipientNoInstall}</span></div>
    </div>
  )
}

function PlatformOverview() {
  const t = usePortalCopy()
  return (
    <dl className="portal-client-platforms">
      {CLIENT_PLATFORM_GROUPS.map(group => <div className="portal-client-platform-group" key={group.label}>
        <dt><PortalIcon name={group.icon} size={18} /><span>{t[group.label]}</span></dt>
        <dd><ul>{group.platforms.map(platform => <li key={platform}>{platform}</li>)}</ul></dd>
      </div>)}
    </dl>
  )
}

function ReleaseAssetList({ manifest }: { readonly manifest: ReleaseDownloadsManifest }) {
  const t = usePortalCopy()
  return (
    <div className="portal-release-list">
      <div className="portal-release-heading">
        <span>{t.directDownloads}</span>
        <code>{manifest.version}</code>
      </div>
      <ul className="portal-release-assets">
        {manifest.assets.map(asset => (
          <li key={asset.filename}>
            <a className="portal-release-asset" href={asset.href} download={asset.filename}>
              <span>{formatAssetLabel(t, asset)}</span>
              <span className="portal-release-asset-meta">
                <span>{formatArchLabel(asset.arch)}</span>
                <PortalIcon name="download" size={15} />
              </span>
            </a>
          </li>
        ))}
      </ul>
    </div>
  )
}
