import type { ReceiveBlockReason } from '../../application/receive/admission'
import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'

export function presentReceiveReason(reason: ReceiveBlockReason, language: Language): string
export function presentReceiveReason(reason: ReceiveBlockReason | null, language: Language): string | null
export function presentReceiveReason(reason: ReceiveBlockReason | null, language: Language): string | null {
  return reason === null ? null : receiverCopy[language].admission[reason]
}
