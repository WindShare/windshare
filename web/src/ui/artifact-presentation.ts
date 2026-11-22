import type { Language } from './preferences/model'
import { receiverCopy } from './locales/receiver'
import type { ActiveActivationSnapshot } from '../application/receive/contracts'

export interface ArtifactActivationPresentation {
  readonly kind: 'waiting' | 'committing'
  readonly title: string
  readonly description: string
}

export function presentActivation(
  activation: ActiveActivationSnapshot | null,
  language: Language = 'en',
): ArtifactActivationPresentation | null {
  if (activation === null) return null
  const t = receiverCopy[language].artifacts
  if (activation.kind === 'waiting-picker') {
    return Object.freeze({
      kind: 'waiting',
      title: t.waitingLocation,
      description: t.completePrompt,
    })
  }
  return Object.freeze({
    kind: 'committing',
    title: t.preparing,
    description: t.bindingLocation,
  })
}
