import type { Translations } from '../locale'
import { en } from './en'
import { zhCN } from './zh-CN'

export const sharedCopy = { en, 'zh-CN': zhCN } satisfies Translations<typeof en>
