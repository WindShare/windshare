import type { V2LaneGrant, V2LaneRejection } from './v2-lane-codec'
import { createV2ProtocolOperationIdentity, type V2ProtocolOperationIdentity } from './v2-identities'
import { V2SessionRuntimeError } from './v2-runtime-types'

export class V2LaneAdmissionRejectedError extends V2SessionRuntimeError {
  readonly rejection: V2LaneRejection

  constructor(rejection: V2LaneRejection) {
    super('lane', `Sender rejected lane admission (code ${rejection.code})`)
    this.name = 'V2LaneAdmissionRejectedError'
    this.rejection = rejection
  }
}

export class V2LaneAdmissionTransportError extends V2SessionRuntimeError {
  constructor(message: string, options?: ErrorOptions) {
    super('lane', message, options)
    this.name = 'V2LaneAdmissionTransportError'
  }
}

export class V2LaneInstallationError extends V2SessionRuntimeError {
  constructor(options?: ErrorOptions) {
    super('lane', 'Accepted lane could not be installed', options)
    this.name = 'V2LaneInstallationError'
  }
}

interface V2LaneAdmissionIdentity {
  readonly grantOperationId: V2ProtocolOperationIdentity
  readonly laneId: number
  readonly laneEpoch: number
}

/**
 * Authentication and local installation are separate settlement facts. Keeping
 * them in one closed result prevents a later lifecycle signal from erasing a
 * sender-authenticated disposition when publication fails locally.
 */
export type V2LaneAdmissionResult =
  | {
      readonly disposition: 'unverified'
      readonly installation: 'not-attempted'
      readonly error: unknown
    }
  | (V2LaneAdmissionIdentity & {
      readonly disposition: 'rejected'
      readonly installation: 'not-attempted'
      readonly rejection: V2LaneRejection
      readonly error: V2LaneAdmissionRejectedError
    })
  | (V2LaneAdmissionIdentity & {
      readonly disposition: 'accepted'
      readonly installation: 'failed'
      readonly error: V2LaneInstallationError
    })
  | (V2LaneAdmissionIdentity & {
      readonly disposition: 'accepted'
      readonly installation: 'installed'
    })

export interface V2LaneGrantRequestOptions {
  readonly laneId?: number
  readonly signal?: AbortSignal
}

export interface V2LaneAdoptionOptions {
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
      throw new V2LaneAdmissionTransportError('Candidate lane closed before admission')
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
          reject(new V2LaneAdmissionTransportError('Candidate lane closed before admission'))
        } else {
          resolve(result.value.slice())
        }
      }),
      (error: unknown) => finish(() => reject(error)),
    )
  })
}

export function laneAdmissionIdentity(grant: V2LaneGrant): V2LaneAdmissionIdentity {
  return Object.freeze({
    grantOperationId: createV2ProtocolOperationIdentity(grant.grantOperationId),
    laneId: grant.laneId,
    laneEpoch: grant.laneEpoch,
  })
}

export function deadlineSignal(
  parent: AbortSignal | undefined,
  milliseconds: number,
  message: string,
  scope: 'lane' | 'session',
): { readonly signal: AbortSignal; readonly close: () => void } {
  const controller = new AbortController()
  const abort = () => controller.abort(
    parent?.reason ?? new DOMException('Operation aborted', 'AbortError'),
  )
  parent?.addEventListener('abort', abort, { once: true })
  if (parent?.aborted) abort()
  const timer = globalThis.setTimeout(() => {
    controller.abort(new V2SessionRuntimeError(scope, message))
  }, milliseconds)
  return {
    signal: controller.signal,
    close: () => {
      globalThis.clearTimeout(timer)
      parent?.removeEventListener('abort', abort)
    },
  }
}
