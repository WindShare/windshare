import { SpiritArtwork } from './spirit/SpiritArtwork'
import { PortalIcon } from './PortalIcon'
import { usePortalCopy } from './locales/use-portal-copy'
import { useSpiritMotion } from './spirit/use-spirit-motion'

export function BrandScene() {
  const { scene, mascot } = useSpiritMotion()
  const t = usePortalCopy()

  return <figure ref={scene} className="portal-brand-scene" aria-label={t.sceneLabel} data-active="false">
    <div className="portal-spirit-halo" aria-hidden="true" />
    <svg className="portal-breeze-lines" viewBox="0 0 480 180" fill="none" aria-hidden="true">
      <path d="M8 98C75 98 73 42 132 42h58m112 74h42c44 0 41-58 91-58h37M39 127h91m205 22h71" />
      <path className="portal-breeze-travel" d="M8 98C75 98 73 42 132 42h58m112 74h42c44 0 41-58 91-58h37" />
    </svg>
    <div className="portal-share-chip" aria-hidden="true"><PortalIcon name="folder" size={22} /><span>{t.routeFiles}</span></div>
    <div className="portal-browser-chip" aria-hidden="true"><PortalIcon name="monitor" size={23} /><span>{t.routeBrowser}</span></div>
    <button ref={mascot} type="button" className="portal-mascot-button" aria-label={t.mascotLabel}>
      <span className="portal-spirit-response">
        <span className="portal-spirit-float"><SpiritArtwork size={176} /></span>
      </span>
    </button>
    <span className="portal-spirit-shadow" aria-hidden="true" />
    <figcaption>{t.mascotCaption}<PortalIcon name="lock" size={11} /></figcaption>
  </figure>
}
