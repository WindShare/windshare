import { PortalIcon, type PortalIconName } from './PortalIcon'
import { PORTAL_SECTIONS } from './navigation'
import { usePortalCopy, type PortalMessageKey } from './locales/use-portal-copy'

const FEATURES: readonly { icon: PortalIconName; title: PortalMessageKey; description: PortalMessageKey; note: PortalMessageKey }[] = [
  { icon: 'bolt', title: 'featureInstantTitle', description: 'featureInstantDescription', note: 'featureInstantNote' },
  { icon: 'shield', title: 'featurePrivateTitle', description: 'featurePrivateDescription', note: 'featurePrivateNote' },
  { icon: 'globe', title: 'featureConnectionTitle', description: 'featureConnectionDescription', note: 'featureConnectionNote' },
  { icon: 'folder', title: 'featureFolderTitle', description: 'featureFolderDescription', note: 'featureFolderNote' },
]

export function FeatureGrid() {
  const t = usePortalCopy()
  return (
    <section id={PORTAL_SECTIONS.features.id} className="portal-section portal-features" aria-labelledby="portal-features-title">
      <div className="portal-container">
        <div className="portal-section-header">
          <div><span className="portal-section-tag">{t.featuresTag}</span><h2 id="portal-features-title">{t.featuresTitle}</h2></div>
          <p>{t.featuresDescription}</p>
        </div>
        <div className="portal-feature-grid">
          {FEATURES.map((item, index) => <article key={item.icon} className="portal-feature-card">
            <div className="portal-feature-top"><PortalIcon name={item.icon} size={24} /><span>0{index + 1}</span></div>
            <h3>{t[item.title]}</h3><p>{t[item.description]}</p>
            <span className="portal-feature-note">{t[item.note]}</span>
          </article>)}
        </div>
      </div>
    </section>
  )
}
