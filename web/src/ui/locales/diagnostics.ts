import type { Language } from '../preferences/model'
import { diagnostics as en } from './en/diagnostics'
import { diagnostics as zh } from './zh-CN/diagnostics'

export const diagnosticsCopy = { en, 'zh-CN': zh } satisfies Record<Language, typeof en>
