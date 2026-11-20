import type { Language } from '../preferences/model'
import * as en from './en/receiver'
import * as zh from './zh-CN/receiver'

export const receiverCopy = { en, 'zh-CN': zh } satisfies Record<Language, typeof en>
