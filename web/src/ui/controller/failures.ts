import type { SingleFileDownloadOutcome } from '../../application/receive/contracts'
import { ReceiveBlockedError, SaveUnavailableError } from '../../application/receive/single-file'
import { CryptoError } from '../../crypto/errors'
import type { ArtifactChoice, ArtifactOffers } from '../../output/model'
import { isPermissionDeniedError, isSourceInvalidatedError } from '../../transfer/fault'
import type { ReceiverFailure } from '../model'

export type ChooseArtifactIntent = 'default' | 'redownload'

export type PreviewDownloadResult =
  | Readonly<{ kind: 'saved-image' | 'exported' | 'started' | 'resumed' | 'waiting-picker' | 'probing' }>
  | Readonly<{ kind: 'failed'; failure: ReceiverFailure }>

export interface PreviewDownloadCallbacks {
  readonly onExported?: () => void
  readonly onAsyncFailure?: (failure: ReceiverFailure) => void
}

export interface TaskActionCallbacks {
  /**
   * Handles an asynchronous task action failure locally.
   * Return `false` if the local surface is no longer visible so the controller
   * can fall back to the share-level error banner.
   */
  readonly onAsyncFailure?: (failure: ReceiverFailure) => boolean | void
}

export function findReadyOfferChoice(offers: ArtifactOffers | null, choiceId: string): ArtifactChoice | undefined {
  if (offers?.kind !== 'ready') return undefined
  const allChoices = [offers.primary, ...offers.alternatives, ...(offers.zip === null ? [] : [offers.zip])]
  return allChoices.find(candidate => candidate.choiceId === choiceId)
}

export function previewDownloadResultFromOutcome(
  outcome: SingleFileDownloadOutcome,
  publicError: (error: unknown) => string,
): PreviewDownloadResult {
  const failure = outcomeFailure(outcome, publicError)
  if (failure !== null) return Object.freeze({ kind: 'failed', failure })
  return Object.freeze({ kind: outcome.kind }) as PreviewDownloadResult
}

export function mapReceiverFailure(
  error: unknown,
  publicError: (error: unknown) => string,
): ReceiverFailure {
  if (error instanceof SaveUnavailableError) {
    return Object.freeze({ kind: 'save-unavailable', reason: error.reason })
  }
  if (error instanceof ReceiveBlockedError) {
    return Object.freeze({ kind: 'blocked', reason: error.reason })
  }
  if (isSourceInvalidatedError(error)) {
    return Object.freeze({ kind: 'source-changed' })
  }
  if (isPermissionDeniedError(error)) {
    return Object.freeze({ kind: 'permissionDenied', detail: publicError(error) })
  }
  return publicError(error)
}

export function outcomeFailure(
  outcome: SingleFileDownloadOutcome,
  publicError: (error: unknown) => string,
): ReceiverFailure | null {
  switch (outcome.kind) {
    case 'blocked':
      return Object.freeze({ kind: 'blocked', reason: outcome.reason })
    case 'unavailable':
      return Object.freeze({ kind: 'save-unavailable', reason: outcome.error.reason })
    case 'failed':
      return mapReceiverFailure(outcome.error, publicError)
    default:
      return null
  }
}

export function joinFailure(error: unknown, detail: string): ReceiverFailure {
  if (!(error instanceof CryptoError)) return detail
  switch (error.code) {
    case 'malformed-key':
    case 'malformed-link':
    case 'malformed-share-id':
    case 'missing-key':
    case 'key-conflict':
      return { kind: 'invalidLink', detail }
    case 'webcrypto-unavailable':
      return { kind: 'secureContext', detail }
    default:
      return detail
  }
}
