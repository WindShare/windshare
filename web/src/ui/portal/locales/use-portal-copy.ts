import { usePreferences } from '../../preferences/use-preferences'
import { en } from './en'
import { zh } from './zh'

export type PortalMessageKey = keyof typeof en

export function usePortalCopy() {
  const { language } = usePreferences()
  return language === 'zh-CN' ? zh : en
}
