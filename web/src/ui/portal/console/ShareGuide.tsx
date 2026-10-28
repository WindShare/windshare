import { useEffect, useState } from 'react'
import { PortalIcon } from '../PortalIcon'
import { PORTAL_LINKS } from '../navigation'
import { usePortalCopy, type PortalMessageKey } from '../locales/use-portal-copy'

const SHARE_COMMAND = 'wind share ./My-Project-Folder'
const COPY_FEEDBACK_MS = 2400
type CopyStatus = 'idle' | 'copying' | 'copied' | 'failed'
const COPY_LABELS: Record<CopyStatus, PortalMessageKey> = {
  idle: 'copy', copying: 'copying', copied: 'copied', failed: 'copyRetry',
}
const COPY_MESSAGES: Record<CopyStatus, PortalMessageKey> = {
  idle: 'copyIdle', copying: 'copyPending', copied: 'copyDone', failed: 'copyFailed',
}

export function ShareGuide() {
  const t = usePortalCopy()
  const [status, setStatus] = useState<CopyStatus>('idle')
  useEffect(() => {
    if (status !== 'copied') return
    const timer = window.setTimeout(() => setStatus('idle'), COPY_FEEDBACK_MS)
    return () => window.clearTimeout(timer)
  }, [status])

  const copy = async () => {
    setStatus('copying')
    try {
      await navigator.clipboard.writeText(SHARE_COMMAND)
      setStatus('copied')
    } catch {
      setStatus('failed')
    }
  }

  return (
    <div className="portal-share-guide">
      <div className="portal-panel-heading">
        <h2>{t.shareTitle}</h2>
        <p>{t.shareDescription}</p>
      </div>
      <a href={PORTAL_LINKS.installation} target="_blank" rel="noopener noreferrer" className="portal-text-link">
        {t.installGuide}<PortalIcon name="arrow" size={16} />
      </a>
      <div className="portal-command">
        <div className="portal-command-heading"><span>{t.terminalLabel}</span>
          <button type="button" className="portal-btn-copy" onClick={() => { void copy() }} disabled={status === 'copying'}>
            <PortalIcon name={status === 'copied' ? 'check' : 'clipboard'} size={14} />{t[COPY_LABELS[status]]}
          </button>
        </div>
        <code><span aria-hidden="true">$ </span>{SHARE_COMMAND}</code>
      </div>
      <p className="portal-copy-feedback" role="status">
        {t[COPY_MESSAGES[status]]}
      </p>
      <div className="portal-privacy-note"><PortalIcon name="monitor" size={15} /><span>{t.stayOnline}</span></div>
    </div>
  )
}
