import { useCopy } from '../../preferences/use-preferences'
import { en } from './en'
import { zh } from './zh'

const donateCopy = { en, 'zh-CN': zh }

export function useDonateCopy() {
  return useCopy(donateCopy)
}
