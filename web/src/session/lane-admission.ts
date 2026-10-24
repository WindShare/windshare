import type { LaneGrant, LaneRejection } from './lane-codec'
import { createProtocolOperationIdentity, type ProtocolOperationIdentity } from './identities'
import { SessionRuntimeError } from './runtime-types'

export class LaneAdmissionRejectedError extends SessionRuntimeError {
  readonly rejection: LaneRejection

  constructor(rejection: LaneRejection) {
    super('lane', `Sender rejected lane admission (code ${rejection.code})`)
    this.name = 'LaneAdmissionRejectedError'
    this.rejection = rejection
  }
}

export class LaneAdmissionTransportError extends SessionRuntimeError {
  constructor(message: string, options?: ErrorOptions) {
    super('lane', message, options)
    this.name = 'LaneAdmissionTransportError'
  }
}

export class LaneInstallationError extends SessionRuntimeError {
  constructor(options?: ErrorOptions) {
    super('lane', 'Accepted lane could not be installed', options)
    this.name = 'LaneInstallationError'
  }
}

interface LaneAdmissionIdentity {
  readonly grantOperationId: ProtocolOperationIdentity
  readonly laneId: number
  readonly laneEpoch: number
}

/**
 * Authentication and local installation are separate settlement facts. Keeping
 * them in one closed result prevents a later lifecycle signal from erasing a
 * sender-authenticated disposition when publication fails locally.
 */
export type LaneAdmissionResult =
  | {
      readonly disposition: 'unverified'
      readonly installation: 'not-attempted'
      readonly error: unknown
    }
  | (LaneAdmissionIdentity & {
      readonly disposition: 'rejected'
      readonly installation: 'not-attempted'
      readonly rejection: LaneRejection
      readonly error: LaneAdmissionRejectedError
    })
  | (LaneAdmissionIdentity & {
      readonly disposition: 'accepted'
      readonly installation: 'failed'
      readonly error: LaneInstallationError
    })
  | (LaneAdmissionIdentity & {
      readonly disposition: 'accepted'
      readonly installation: 'installed'
    })

export interface LaneGrantRequestOptions {
  readonly laneId?: number
  readonly signal?: AbortSignal
}

export interface LaneAdoptionOptions {
  readonly signal?: AbortSignal
}

export async function readLaneAdmission(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  signal?: AbortSignal,
): Promise<Uint8Array<ArrayBuffer>> {
  signal?.throwIfAborted()
  if (signal === undefined) {
    const result = await reader.read()
    if (result.done) {
      throw new LaneAdmissionTransportError('Candidate lane closed before admission')
    }
    return result.value.slice()
  }
  return new Promise<Uint8Array<ArrayBuffer>>((resolve, reject) => {
    let settled = false
    const finish = (operation: () => void) => {
      if (settled) return
      settled = true
      signal.removeEventListener('abort', aborted)
      operation()
    }
    const aborted = () => {
      const reason = signal.reason ?? new DOMException('Lane admission aborted', 'AbortError')
      reader.cancel(reason).catch(() => undefined)
      finish(() => reject(reason))
    }
    signal.addEventListener('abort', aborted, { once: true })
    reader.read().then(
      (result) => finish(() => {
        if (result.done) {
          reject(new LaneAdmissionTransportError('Candidate lane closed before admission'))
        } else {
          resolve(result.value.slice())
        }
      }),
      (error: unknown) => finish(() => reject(error)),
    )
  })
}

export function laneAdmissionIdentity(grant: LaneGrant): LaneAdmissionIdentity {
  return Object.freeze({
    grantOperationId: createProtocolOperationIdentity(grant.grantOperationId),
    laneId: grant.laneId,
    laneEpoch: grant.laneEpoch,
  })
}

export function deadlineSignal(
  parent: AbortSignal | undefined,
  milliseconds: number,
  timeout: Error,
): { readonly signal: AbortSignal; readonly close: () => void } {
  const controller = new AbortController()
  const abort = () => controller.abort(
    parent?.reason ?? new DOMException('Operation aborted', 'AbortError'),
  )
  parent?.addEventListener('abort', abort, { once: true })
  if (parent?.aborted) abort()
  const timer = globalThis.setTimeout(() => {
    controller.abort(timeout)
  }, milliseconds)
  return {
    signal: controller.signal,
    close: () => {
      globalThis.clearTimeout(timer)
      parent?.removeEventListener('abort', abort)
    },
  }
}
