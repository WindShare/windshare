import { LanguageSelect } from '../preferences/LanguageSelect'
import { useId } from 'react'
import { usePreferences } from '../preferences/use-preferences'
import type { Appearance } from '../preferences/model'
import { PortalIcon, type PortalIconName } from './PortalIcon'
import { PortalPopover } from './PortalPopover'
import { usePortalCopy } from './locales/use-portal-copy'

const APPEARANCES: readonly { value: Appearance; icon: PortalIconName }[] = [
  { value: 'system', icon: 'monitor' }, { value: 'light', icon: 'sun' }, { value: 'dark', icon: 'moon' },
]

export function PortalPreferences() {
  const preferences = usePreferences()
  const t = usePortalCopy()
  const languageId = useId()

  return <PortalPopover className="portal-preferences" label={t.preferences} icon="globe">
    <div className="portal-language-control">
      <label htmlFor={languageId}>{t.language}</label>
      <span className="portal-language-select">
        <LanguageSelect id={languageId} />
        <PortalIcon name="chevronDown" size={14} />
      </span>
    </div>
    <fieldset className="portal-appearance-options"><legend>{t.appearance}</legend>
      <div>{APPEARANCES.map(option => <label key={option.value}>
        <input type="radio" name="portal-appearance" value={option.value}
          checked={preferences.appearance === option.value}
          onChange={() => preferences.update({ appearance: option.value })} />
        <span><PortalIcon name={option.icon} size={18} />{t[option.value]}</span>
      </label>)}</div>
    </fieldset>
  </PortalPopover>
}
