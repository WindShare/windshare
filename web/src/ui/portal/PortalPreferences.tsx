import { useRef } from 'react'
import { usePreferences } from '../preferences/use-preferences'
import type { Appearance, Language } from '../preferences/model'
import { PortalIcon, type PortalIconName } from './PortalIcon'
import { usePortalCopy } from './locales/use-portal-copy'

const APPEARANCES: readonly { value: Appearance; icon: PortalIconName }[] = [
  { value: 'system', icon: 'monitor' }, { value: 'light', icon: 'sun' }, { value: 'dark', icon: 'moon' },
]

export function PortalPreferences() {
  const preferences = usePreferences()
  const t = usePortalCopy()
  const menu = useRef<HTMLDetailsElement>(null)

  return <details className="portal-preferences" ref={menu} name="portal-header-menu" onKeyDown={event => {
    if (event.key === 'Escape' && menu.current?.open) {
      menu.current.open = false
      menu.current.querySelector('summary')?.focus()
    }
  }}>
    <summary aria-label={t.preferences} title={t.preferences}><PortalIcon name="globe" size={19} /></summary>
    <div className="portal-preferences-panel">
      <label className="portal-language-control"><span>{t.language}</span>
        <select aria-label={t.language} value={preferences.language}
          onChange={event => preferences.update({ language: event.target.value as Language })}>
          <option value="zh-CN">简体中文</option><option value="en">English</option>
        </select>
      </label>
      <fieldset className="portal-appearance-options"><legend>{t.appearance}</legend>
        <div>{APPEARANCES.map(option => <label key={option.value}>
          <input type="radio" name="portal-appearance" value={option.value}
            checked={preferences.appearance === option.value}
            onChange={() => preferences.update({ appearance: option.value })} />
          <span><PortalIcon name={option.icon} size={18} />{t[option.value]}</span>
        </label>)}</div>
      </fieldset>
      <label className="portal-motion-control"><span>{t.reduceMotion}</span>
        <input type="checkbox" checked={preferences.motion === 'reduced'}
          onChange={event => preferences.update({ motion: event.target.checked ? 'reduced' : 'system' })} />
      </label>
      <p>{t.motionHint}</p>
    </div>
  </details>
}
