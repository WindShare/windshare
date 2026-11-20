import type { ReceiveDestinationLabel } from '../../output/model/operation-display'
import type { Language } from '../preferences/model'
import { receiverCopy } from '../locales/receiver'

export function destinationLabel(destination: ReceiveDestinationLabel, language: Language): string {
  return typeof destination === 'string' ? destination : receiverCopy[language].downloads.browserDownloads
}
