export function InAppBrowserHint() {
  const agent = typeof navigator === 'undefined' ? '' : navigator.userAgent
  let browser: string | undefined
  if (/MicroMessenger\//i.test(agent)) browser = 'WeChat'
  else if (/\bQQ\//i.test(agent)) browser = 'QQ'
  if (browser === undefined) return null
  return <aside className="in-app-browser-hint" role="note">
    <strong>Open in your browser to save files</strong>
    <p>Use the {browser} menu to open this same link in Safari, Chrome, or your system browser.</p>
  </aside>
}
