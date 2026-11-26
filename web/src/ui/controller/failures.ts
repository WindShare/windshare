import type { SingleFileDownloadOutcome } from '../../application/receive/contracts'
import { ReplacementFileNotFoundError } from '../../application/browser-share/source-replacement'
import { ReceiveBlockedError, SaveUnavailableError } from '../../application/receive/single-file'
import { CryptoError } from '../../crypto/errors'
import { isRelayAccessDenied } from '../../receiver/recovery-failure'
import { FSARestoreTargetMissingError } from '../../output/model'
import { catalogCapacityReason, isPermissionDeniedError, isSourceInvalidatedError } from '../../transfer/fault'
import type { ReceiverFailure } from '../model'

export type ChooseArtifactIntent = 'default' | 'redownload'

export type PreviewDownloadResult =
  | Readonly<{ kind: 'saved-image' | 'started' | 'waiting-picker' | 'probing' }>
  | Readonly<{ kind: 'failed'; failure: ReceiverFailure }>

export interface AsyncActionCallbacks {
  /**
   * Handles an asynchronous action failure locally.
   * Return `true` when the local surface displayed the failure, or `false`
   * when the surface is no longer visible so the controller falls back to
   * the share-level error banner.
   */
  readonly onAsyncFailure?: (failure: ReceiverFailure) => boolean
}

export interface ReplacementActionCallbacks extends AsyncActionCallbacks {
  /** Called once the replacement file has been located and selected in the share workspace. */
  readonly onPrepared?: () => void
  /** Called when the replacement lookup settles (prepared, cancelled, blocked, or failed). */
  readonly onSettled?: () => void
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
  if (error instanceof FSARestoreTargetMissingError) {
    return Object.freeze({ kind: 'restore-target-missing', directoryLabel: error.directoryLabel })
  }
  if (error instanceof ReplacementFileNotFoundError) {
    return Object.freeze({ kind: 'replacement-not-found', path: error.path })
  }
  if (isRelayAccessDenied(error)) return { kind: 'relayAccessDenied', detail: publicError(error) }
  if (isSourceInvalidatedError(error)) {
    return Object.freeze({ kind: 'source-changed' })
  }
  if (isPermissionDeniedError(error)) {
    return Object.freeze({ kind: 'permissionDenied', detail: publicError(error) })
  }
  const catalogLimit = catalogCapacityReason(error)
  if (catalogLimit === 'browser-catalog-limit') {
    return Object.freeze({ kind: 'browserCatalogLimit', detail: publicError(error) })
  }
  if (catalogLimit === 'share-catalog-limit') {
    return Object.freeze({ kind: 'shareCatalogLimit', detail: publicError(error) })
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
  if (isRelayAccessDenied(error)) return Object.freeze({ kind: 'relayAccessDenied', detail })
  if (!(error instanceof CryptoError)) return Object.freeze({ kind: 'open', detail })
  switch (error.code) {
    case 'malformed-key':
    case 'malformed-link':
    case 'malformed-share-id':
    case 'missing-key':
    case 'key-conflict':
      return Object.freeze({ kind: 'invalidLink', detail })
    case 'webcrypto-unavailable':
      return Object.freeze({ kind: 'secureContext', detail })
    default:
      return Object.freeze({ kind: 'open', detail })
  }
}
