import type { Language } from '../preferences/model'
import { shell as en } from './en/shell'
import { shell as zh } from './zh-CN/shell'

export const shellCopy = { en, 'zh-CN': zh } satisfies Record<Language, typeof en>
