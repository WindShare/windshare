import { useCopy } from '../../preferences/use-preferences'
import { en } from './en'
import { zh } from './zh'

const portalCopy = { en, 'zh-CN': zh }

export type PortalMessageKey = keyof typeof en

export function usePortalCopy() {
  return useCopy(portalCopy)
}
