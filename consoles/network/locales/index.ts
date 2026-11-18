import type { Translations } from '../../ui/locale'
import { en } from './en'
import { zhCN } from './zh-CN'

export const networkCopy = { en, 'zh-CN': zhCN } satisfies Translations<typeof en>
