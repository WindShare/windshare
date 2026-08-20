import type { BrowserReceiveOperationLease } from '../../output/browser/session-lease'
import type { FSARootMutationLease } from '../../output/browser/namespace-mutation'
import {
  emitOutputTrace,
  outputTraceEvent,
  recordOutputException,
  type OutputDiagnosticsPorts,
} from '../../output/diagnostics'
import type { FileSystemAccessOutputSession } from '../../output/file-system-access/session'

export interface FSAResourceOwnerOptions {
  readonly repository?: Readonly<{ close(): void }>
  readonly operationLease?: BrowserReceiveOperationLease
  readonly rootLease?: FSARootMutationLease
  readonly outputSession?: FileSystemAccessOutputSession
  readonly closeOperationAuthority?: () => Promise<void>
  readonly diagnostics?: OutputDiagnosticsPorts
}

/**
 * The durable operation has one cleanup owner regardless of whether activation
 * reaches a bound runtime. Exactly-once closure preserves every cleanup consequence.
 */
export class FSAResourceOwner {
  readonly #repository: FSAResourceOwnerOptions['repository']
  readonly #operationLease: BrowserReceiveOperationLease | undefined
  readonly #rootLease: FSARootMutationLease | undefined
  readonly #closeOperationAuthority: (() => Promise<void>) | undefined
  readonly #diagnostics: OutputDiagnosticsPorts | undefined
  #outputSession: FileSystemAccessOutputSession | undefined
  #rootTransferred = false
  #closePromise: Promise<void> | undefined

  constructor(options: FSAResourceOwnerOptions) {
    if (options.closeOperationAuthority !== undefined &&
        (options.operationLease !== undefined || options.repository !== undefined)) {
      throw new TypeError('FSA resource owner received overlapping operation cleanup authorities')
    }
    this.#repository = options.repository
    this.#operationLease = options.operationLease
    this.#rootLease = options.rootLease
    this.#outputSession = options.outputSession
    this.#rootTransferred = options.outputSession !== undefined
    this.#closeOperationAuthority = options.closeOperationAuthority
    this.#diagnostics = options.diagnostics
  }

  adoptOutputSession(session: FileSystemAccessOutputSession): void {
    if (this.#closePromise !== undefined) {
      throw new DOMException('FSA resources are already closing', 'InvalidStateError')
    }
    if (this.#outputSession !== undefined && this.#outputSession !== session) {
      throw new DOMException('FSA output session authority was already transferred', 'InvalidStateError')
    }
    this.#outputSession = session
    this.#rootTransferred = true
  }

  replaceOutputSession(session: FileSystemAccessOutputSession): void {
    if (this.#closePromise !== undefined) {
      throw new DOMException('FSA resources are already closing', 'InvalidStateError')
    }
    this.#outputSession = session
    this.#rootTransferred = true
  }

  close(): Promise<void> {
    this.#closePromise ??= this.#close()
    return this.#closePromise
  }

  async #close(): Promise<void> {
    const failures: unknown[] = []
    try {
      await this.#outputSession?.close()
    } catch (error) {
      // The output session already classified file/checkpoint/root consequences.
      failures.push(error)
    }
    if (!this.#rootTransferred) {
      try {
        await this.#rootLease?.release()
      } catch (error) {
        failures.push(error)
        recordOutputException(this.#diagnostics?.failures?.cleanup, error)
      }
    }
    try {
      await (this.#closeOperationAuthority?.() ?? this.#operationLease?.release())
    } catch (error) {
      failures.push(error)
      recordOutputException(this.#diagnostics?.failures?.cleanup, error)
    }
    if (this.#closeOperationAuthority === undefined) {
      try {
        this.#repository?.close()
      } catch (error) {
        failures.push(error)
        recordOutputException(this.#diagnostics?.failures?.cleanup, error)
      }
    }
    if (failures.length === 0) return
    emitOutputTrace(this.#diagnostics?.trace, () => outputTraceEvent('cleanup', {
      backend: 'file_system_access',
      transition: 'failed',
    }))
    throw failures.length === 1
      ? failures[0]
      : new AggregateError(failures, 'FSA receive resources did not detach')
  }
}
