import { receiverCopy } from '../locales/receiver'
import { useCopy } from '../preferences/use-preferences'
export function InAppBrowserHint() {
  const t = useCopy(receiverCopy).saving
  const agent = typeof navigator === 'undefined' ? '' : navigator.userAgent
  let browser: string | undefined
  if (/MicroMessenger\//i.test(agent)) browser = t.wechat
  else if (/\bQQ\//i.test(agent)) browser = 'QQ'
  if (browser === undefined) return null
  return <aside className="in-app-browser-hint" role="note">
    <strong>{t.openBrowser}</strong>
    <p>{t.browserMenu(browser)}</p>
  </aside>
}
