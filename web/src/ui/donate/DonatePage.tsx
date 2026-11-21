import { PortalFooter } from '../portal/PortalFooter'
import { PortalHeader } from '../portal/PortalHeader'
import '../portal/portal.css'
import './donate.css'
import { useDonateCopy } from './locales/use-donate-copy'

export default function DonatePage() {
  const t = useDonateCopy()

  return (
    <div className="portal-root">
      <PortalHeader anchorPrefix="/" />
      <main className="donate-main">
        <div className="portal-container donate-grid">
          <section className="donate-card" aria-label={t.actionRegion}>
            <h1 className="donate-title">{t.title}</h1>
            <p className="donate-subtitle">{t.subtitle}</p>
          </section>
          <section className="donate-card" aria-label={t.wallRegion}>
            <h2 className="donate-wall-heading">{t.wallHeading}</h2>
            <p className="donate-placeholder">{t.scaffoldNotice}</p>
          </section>
        </div>
      </main>
      <PortalFooter />
    </div>
  )
}
