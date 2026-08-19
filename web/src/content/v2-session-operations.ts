import type {
  V2CatalogOperationClient,
  V2CatalogScanProgressListener,
} from '../catalog/v2-client'
import type { V2CatalogPageRequest } from '../catalog/v2-records'
import { equalBytes } from '../crypto/bytes'
import {
  protocolFailureFact,
  type FailureFact,
  type ProtocolFailure,
} from '../diagnostics/incident/fact'
import {
  decodeV2ScanProgress,
  type V2SessionMessage,
  V2_MESSAGE_KIND,
} from '../session/v2-message'
import type { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import { V2SessionRuntimeError } from '../session/v2-runtime-types'
import { decodeV2CatalogResult, encodeV2ListRequest } from './v2-flow'

export class V2RemoteOperationError extends Error {
  readonly scope: 'directory' | 'revision' | 'block' | 'peer'
  readonly code: number
  readonly retryable: boolean
  readonly retryAfterMilliseconds: number | undefined
  readonly protocolFailure: ProtocolFailure
  readonly failureFact: FailureFact<'protocol_failure'>

  constructor(protocolFailure: ProtocolFailure) {
    super('Sender rejected the authenticated operation')
    this.name = 'V2RemoteOperationError'
    this.scope = protocolFailure.wireScope
    this.code = protocolFailure.wireCode
    this.retryable = protocolFailure.retryable
    this.retryAfterMilliseconds = protocolFailure.retryAfterMilliseconds
    this.protocolFailure = protocolFailure
    this.failureFact = protocolFailureFact({
      stage: 'protocol_operation',
      recoveryDisposition: protocolFailure.retryable ? 'retryable' : 'terminal',
      protocolFailure,
    })
  }
}

export function remoteOperationErrorFor(
  session: V2ReceiverSessionRuntime,
  message: V2SessionMessage,
): V2RemoteOperationError {
  return new V2RemoteOperationError(session.authenticatedProtocolFailure(message))
}

export class V2CatalogSessionOperations implements V2CatalogOperationClient {
  readonly #session: V2ReceiverSessionRuntime

  constructor(session: V2ReceiverSessionRuntime) {
    this.#session = session
  }

  async failProtocol(): Promise<void> {
    await this.#session.close()
  }

  async fetchPage(
    request: V2CatalogPageRequest,
    signal: AbortSignal,
    onProgress?: V2CatalogScanProgressListener,
  ): Promise<Uint8Array> {
    const operation = await this.#session.beginOperation(
      V2_MESSAGE_KIND.listChildren,
      encodeV2ListRequest(request.directoryId, request.generation, request.pageIndex),
      { signal },
    )
    let attemptId: Uint8Array<ArrayBuffer> | undefined
    let discoveredEntries = 0n
    while (true) {
      const message = await operation.next(signal)
      if (message.kind === V2_MESSAGE_KIND.scanProgress) {
        const progress = decodeV2ScanProgress(message.body)
        const replay = attemptId !== undefined &&
          equalBytes(attemptId, progress.attemptId) &&
          progress.discoveredEntries === discoveredEntries
        if (
          (attemptId !== undefined && !equalBytes(attemptId, progress.attemptId)) ||
          progress.discoveredEntries < discoveredEntries
        ) {
          const failure = new V2SessionRuntimeError(
            'session',
            'Directory scan progress changed identity or regressed',
          )
          await this.#session.close()
          throw failure
        }
        attemptId ??= progress.attemptId
        if (replay) continue
        discoveredEntries = progress.discoveredEntries
        onProgress?.(Object.freeze({
          directoryId: request.directoryId.slice(),
          attemptId: progress.attemptId.slice(),
          discoveredEntries: progress.discoveredEntries,
        }))
        continue
      }
      if (message.kind === V2_MESSAGE_KIND.operationError) {
        throw remoteOperationErrorFor(this.#session, message)
      }
      if (message.kind !== V2_MESSAGE_KIND.catalogResult) {
        throw new Error('Catalog operation received an unexpected response')
      }
      return decodeV2CatalogResult(message.body)
    }
  }
}
