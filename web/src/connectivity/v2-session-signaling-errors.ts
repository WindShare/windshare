import {
  V2MessageError,
  type V2OperationErrorControl,
} from '../session/v2-message'
import { V2SessionRuntimeError } from '../session/v2-runtime-types'
import {
  v2TypedErrorForPeerOperationCode,
  type V2AuthenticatedPeerOperationFailureDiagnostic,
  type V2BrowserConnectivityAttemptStage,
  type V2TypedPeerErrorCode,
} from './diagnostics'
import {
  CandidateLimitExceededError,
  PeerNegotiationError,
} from './errors'

const MAXIMUM_DIAGNOSTIC_TEXT_BYTES = 512

export class V2SessionSignalingError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'V2SessionSignalingError'
  }
}

/** Authenticated sender text remains byte-for-byte visible to evidence consumers. */
export class V2AuthenticatedPeerOperationError extends V2SessionSignalingError {
  readonly operationFailure: V2OperationErrorControl & { readonly scope: 'peer' }

  constructor(operationFailure: V2OperationErrorControl & { readonly scope: 'peer' }) {
    super(operationFailure.message)
    this.name = 'V2AuthenticatedPeerOperationError'
    this.operationFailure = Object.freeze({ ...operationFailure })
  }
}

export class V2PeerProtocolError extends V2SessionSignalingError {
  constructor(message: string) {
    super(message)
    this.name = 'V2PeerProtocolError'
  }
}

export interface V2ClassifiedTerminalSignalingFailure {
  readonly failureScope: 'attempt' | 'session'
  readonly typedErrorCode: V2TypedPeerErrorCode
  readonly failureMessage: string
  readonly authenticatedSenderOperationFailure?: V2AuthenticatedPeerOperationFailureDiagnostic
}

export function classifyV2TerminalSignalingFailure(
  reason: unknown,
  failedAtStage: Exclude<V2BrowserConnectivityAttemptStage, 'started' | 'failed'>,
  requestedScope: 'attempt' | 'session',
  explicitCode?: V2TypedPeerErrorCode,
): V2ClassifiedTerminalSignalingFailure {
  const authenticated = authenticatedSenderFailure(reason)
  const failureScope = authenticated === undefined ? requestedScope : 'attempt'
  return Object.freeze({
    failureScope,
    typedErrorCode: explicitCode ?? typedErrorCodeFor(reason, failedAtStage, failureScope),
    failureMessage: authenticated?.message ?? boundedDiagnosticMessage(reason),
    ...(authenticated === undefined ? {} : { authenticatedSenderOperationFailure: authenticated }),
  })
}

function authenticatedSenderFailure(
  reason: unknown,
): V2AuthenticatedPeerOperationFailureDiagnostic | undefined {
  const error = findNestedError<V2AuthenticatedPeerOperationError>(
    reason,
    (candidate): candidate is V2AuthenticatedPeerOperationError =>
      candidate instanceof V2AuthenticatedPeerOperationError,
  )
  if (error === undefined) return undefined
  const typed = v2TypedErrorForPeerOperationCode(error.operationFailure.code)
  if (typed === undefined) return undefined
  return Object.freeze({
    scope: 'peer',
    code: error.operationFailure.code,
    message: error.operationFailure.message,
  })
}

function typedErrorCodeFor(
  reason: unknown,
  failedAtStage: Exclude<V2BrowserConnectivityAttemptStage, 'started' | 'failed'>,
  failureScope: 'attempt' | 'session',
): V2TypedPeerErrorCode {
  const authenticated = findNestedError<V2AuthenticatedPeerOperationError>(
    reason,
    (candidate): candidate is V2AuthenticatedPeerOperationError =>
      candidate instanceof V2AuthenticatedPeerOperationError,
  )
  if (authenticated !== undefined) {
    return v2TypedErrorForPeerOperationCode(authenticated.operationFailure.code) ?? 'unexpected'
  }
  if (failureScope === 'session') return 'signaling-contract'
  const timeout = findNestedError(reason, (candidate) =>
    candidate instanceof DOMException && candidate.name === 'TimeoutError')
  if (timeout !== undefined) return 'peer-timeout'
  const cancelled = findNestedError(reason, (candidate) =>
    candidate instanceof DOMException && candidate.name === 'AbortError')
  if (cancelled !== undefined) return 'attempt-cancelled'
  const candidateFailure = findNestedError<CandidateLimitExceededError>(
    reason,
    (candidate): candidate is CandidateLimitExceededError =>
      candidate instanceof CandidateLimitExceededError,
  )
  if (candidateFailure !== undefined) return 'peer-candidates'
  const runtime = findNestedError<V2SessionRuntimeError>(
    reason,
    (candidate): candidate is V2SessionRuntimeError => candidate instanceof V2SessionRuntimeError,
  )
  if (runtime?.scope === 'session') return 'signaling-contract'
  if (runtime?.scope === 'lane') return 'peer-admission'
  if (failedAtStage === 'lane-granted' || failedAtStage === 'lane-attached' ||
      failedAtStage === 'admitted') return 'peer-admission'
  const signaling = findNestedError(reason, (candidate) =>
    candidate instanceof V2SessionSignalingError || candidate instanceof V2MessageError)
  if (signaling !== undefined) return 'signaling-contract'
  const negotiation = findNestedError<PeerNegotiationError>(
    reason,
    (candidate): candidate is PeerNegotiationError => candidate instanceof PeerNegotiationError,
  )
  return negotiation === undefined ? 'unexpected' : 'peer-negotiation'
}

function findNestedError<T extends Error = Error>(
  reason: unknown,
  accept: (candidate: Error) => boolean,
  seen: Set<unknown> = new Set(),
): T | undefined {
  if (!(reason instanceof Error) || seen.has(reason)) return undefined
  seen.add(reason)
  if (accept(reason)) return reason as T
  if (reason instanceof AggregateError) {
    for (const nested of reason.errors) {
      const match = findNestedError<T>(nested, accept, seen)
      if (match !== undefined) return match
    }
  }
  return findNestedError<T>(reason.cause, accept, seen)
}

function boundedDiagnosticMessage(reason: unknown): string {
  const message = diagnosticMessage(reason)
  let result = ''
  for (const character of message || 'Peer attempt failed') {
    if (new TextEncoder().encode(result + character).byteLength > MAXIMUM_DIAGNOSTIC_TEXT_BYTES) break
    result += character
  }
  return result || 'Peer attempt failed'
}

function diagnosticMessage(reason: unknown): string {
  if (reason instanceof Error) return reason.message
  if (typeof reason === 'string') return reason
  return 'Peer attempt failed without a diagnostic message'
}
