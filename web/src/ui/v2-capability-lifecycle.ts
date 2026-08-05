import {
  capabilityRedactionValuesFromInput,
  createCapabilityRedactor,
  type CapabilityRedactor,
} from '../security/capability-redactor'
import { formatDiagnosticValue } from '../security/diagnostic-formatter'

export interface V2CapturedLocation {
  readonly capabilityInput: string | null
  readonly pageUrl: string
}

export type V2SecurityMilestone = 'location-cleared' | 'key-cleared'

export interface V2LocationCaptureOptions {
  readonly onSecurityMilestone?: (milestone: 'location-cleared') => void
}

export type V2DiagnosticFormatter = (
  error: unknown,
  redactor: CapabilityRedactor | undefined,
) => string

export interface V2CapabilityLifecycleOptions {
  readonly diagnosticFormatter?: V2DiagnosticFormatter
  readonly onSecurityMilestone?: (milestone: V2SecurityMilestone) => void
}

/**
 * A join lease owns the separate-key string until the gateway receives it.
 * Keeping handoff and release here prevents controller branches from retaining
 * another copy while an asynchronous gateway failure is being formatted.
 */
export interface V2CapabilityJoinLease {
  readonly activate: () => void
  readonly handoff: <T>(operation: (input: string) => T) => T
  readonly publicError: (error: unknown) => string
  readonly release: () => void
}

export function captureV2Location(
  windowPort: Window = window,
  options: V2LocationCaptureOptions = {},
): V2CapturedLocation {
  const input = windowPort.location.href
  const sanitized = new URL(input)
  const capabilityInput = sanitized.hash.length > 1 ? input : null
  sanitized.hash = ''
  // Secret erasure precedes crypto, browser feature detection, and relay dialing.
  windowPort.history.replaceState(windowPort.history.state, '', sanitized)
  notifySecurityMilestone(options.onSecurityMilestone, 'location-cleared')
  return Object.freeze({ capabilityInput, pageUrl: sanitized.href })
}

/**
 * Owns capability redactors, security milestones, and error publication for a
 * receiver instance. The controller delegates this boundary so navigation and
 * transfer orchestration cannot accidentally reintroduce secret retention.
 */
export class V2CapabilityInputLifecycle {
  readonly #diagnosticFormatter: V2DiagnosticFormatter
  readonly #onSecurityMilestone: ((milestone: V2SecurityMilestone) => void) | undefined
  #redactor: CapabilityRedactor | undefined

  constructor(options: V2CapabilityLifecycleOptions = {}) {
    this.#diagnosticFormatter = options.diagnosticFormatter ?? formatV2PublicError
    this.#onSecurityMilestone = options.onSecurityMilestone
  }

  acceptCapturedLocation(captured: V2CapturedLocation): void {
    const next = captured.capabilityInput === null
      ? undefined
      : createCapabilityRedactor(
        capabilityRedactionValuesFromInput(captured.capabilityInput, captured.pageUrl),
      )
    this.#replace(next)
  }

  beginJoin(input: string, pageUrl: string): V2CapabilityJoinLease {
    const redactor = createCapabilityRedactor(
      capabilityRedactionValuesFromInput(input, pageUrl),
    )
    return new OwnedCapabilityJoinLease(
      input,
      redactor,
      (next) => this.#replace(next),
      (error) => this.#format(error, redactor),
    )
  }

  publicError(error: unknown): string {
    return this.#format(error, this.#redactor)
  }

  notify(milestone: V2SecurityMilestone): void {
    notifySecurityMilestone(this.#onSecurityMilestone, milestone)
  }

  clear(): void {
    this.#redactor?.clear()
    this.#redactor = undefined
  }

  #format(error: unknown, redactor: CapabilityRedactor | undefined): string {
    const formatted = this.#diagnosticFormatter(error, redactor)
    return redactor?.redactText(formatted) ?? formatted
  }

  #replace(next: CapabilityRedactor | undefined): void {
    if (this.#redactor === next) return
    this.#redactor?.clear()
    this.#redactor = next
  }
}

class OwnedCapabilityJoinLease implements V2CapabilityJoinLease {
  #input: string | undefined
  readonly #redactor: CapabilityRedactor
  readonly #activateOwner: (redactor: CapabilityRedactor) => void
  readonly #formatError: (error: unknown) => string
  #activated = false

  constructor(
    input: string,
    redactor: CapabilityRedactor,
    activateOwner: (redactor: CapabilityRedactor) => void,
    formatError: (error: unknown) => string,
  ) {
    this.#input = input
    this.#redactor = redactor
    this.#activateOwner = activateOwner
    this.#formatError = formatError
  }

  activate(): void {
    if (this.#activated) return
    if (this.#input === undefined) throw new Error('Capability join lease was already released')
    this.#activated = true
    this.#activateOwner(this.#redactor)
  }

  handoff<T>(operation: (input: string) => T): T {
    const input = this.#input
    if (input === undefined) throw new Error('Capability input was already handed off')
    // Clear the lease before invoking user code. A rejected gateway promise then
    // cannot retain a second owner in this lifecycle object.
    this.#input = undefined
    return operation(input)
  }

  publicError(error: unknown): string {
    return this.#formatError(error)
  }

  release(): void {
    this.#input = undefined
    if (!this.#activated) this.#redactor.clear()
  }
}

export function formatV2PublicError(
  error: unknown,
  redactor: CapabilityRedactor | undefined,
): string {
  const detached = formatDiagnosticValue(
    error,
    redactor === undefined ? {} : { redactText: redactor.redactText },
  )
  const message = diagnosticMessageFromDetached(detached) ??
    'An unexpected receiver error occurred.'
  return redactor?.redactText(message) ?? message
}

function diagnosticMessageFromDetached(value: unknown): string | undefined {
  if (typeof value === 'string' && value.length > 0) return value
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return undefined
  if ('message' in value && typeof value.message === 'string' && value.message.length > 0) {
    return value.message
  }
  if ('cause' in value) return diagnosticMessageFromDetached(value.cause)
  if ('errors' in value && Array.isArray(value.errors)) {
    for (const nested of value.errors) {
      const message = diagnosticMessageFromDetached(nested)
      if (message !== undefined) return message
    }
  }
  return undefined
}

function notifySecurityMilestone<Milestone extends V2SecurityMilestone>(
  observer: ((milestone: Milestone) => void) | undefined,
  milestone: Milestone,
): void {
  try {
    observer?.(milestone)
  } catch {
    // Security probes are observers only; a test hook cannot own receiver
    // lifecycle or turn a successful erase into a user-visible failure.
  }
}
