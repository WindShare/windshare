import { DiagnosticsEntry } from '../diagnostics/DiagnosticsEntry'
import { WindShareMark } from '../brand/WindShareMark'
import { PORTAL_LINKS } from './navigation'
import { usePortalCopy } from './locales/use-portal-copy'

export function PortalFooter() {
  const t = usePortalCopy()
  return (
    <footer className="portal-footer">
      <div className="portal-container portal-footer-inner">
        <div className="portal-footer-left">
          <span className="portal-footer-brand"><WindShareMark size={28} /><strong>WindShare</strong></span>
          <span className="portal-footer-note">{t.footerNote}</span>
        </div>
        <nav className="portal-footer-links" aria-label={t.resources}>
          <a href={PORTAL_LINKS.repository} target="_blank" rel="noopener noreferrer">GitHub</a>
          <a href={PORTAL_LINKS.protocol} target="_blank" rel="noopener noreferrer">{t.protocol}</a>
          <a href={PORTAL_LINKS.issues} target="_blank" rel="noopener noreferrer">{t.feedback}</a>
          <DiagnosticsEntry label={t.diagnostics} />
        </nav>
      </div>
    </footer>
  )
}
