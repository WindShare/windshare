import type { LaneRequests } from './scheduling/requests'
import type {
  CatalogOperationClient,
  CatalogScanProgressListener,
} from '../catalog/client'
import type { CatalogPageRequest } from '../catalog/records'
import { equalBytes } from '../crypto/bytes'
import { protocolFailureFact, type FailureFact } from '../diagnostics/incident/fact'
import { type ReceivedProtocolError } from '../diagnostics/incident/protocol-error'
import {
  decodeScanProgress,
  type SessionMessage,
  MESSAGE_KIND,
} from '../session/message'
import type { ReceiverSessionRuntime } from '../session/runtime'
import { SessionRuntimeError } from '../session/runtime-types'
import { decodeCatalogResult, encodeListRequest } from './flow'

export class RemoteOperationError extends Error {
  readonly scope: 'directory' | 'revision' | 'block' | 'peer'
  readonly code: number
  readonly retryable: boolean
  readonly retryAfterMilliseconds: number | undefined
  readonly protocolFailure: ReceivedProtocolError
  readonly failureFact: FailureFact<'protocol_failure'>

  constructor(protocolFailure: ReceivedProtocolError) {
    super('Sender rejected the authenticated operation')
    this.name = 'RemoteOperationError'
    this.scope = protocolFailure.content.scope
    this.code = protocolFailure.content.code
    this.retryable = protocolFailure.content.retryable
    this.retryAfterMilliseconds = protocolFailure.content.retryAfterMilliseconds
    this.protocolFailure = protocolFailure
    this.failureFact = protocolFailureFact({
      error: this,
      stage: 'protocol_operation',
      recoveryDisposition: protocolFailure.content.retryable ? 'retryable' : 'terminal',
      protocolFailure,
    })
  }
}

export function remoteOperationErrorFor(
  session: ReceiverSessionRuntime,
  message: SessionMessage,
): RemoteOperationError {
  return new RemoteOperationError(session.authenticatedReceivedProtocolError(message))
}

export class CatalogSessionOperations implements CatalogOperationClient {
  readonly #session: ReceiverSessionRuntime

  readonly #requests: LaneRequests

  constructor(session: ReceiverSessionRuntime, requests: LaneRequests) {
    this.#requests = requests
    this.#session = session
  }

  async failProtocol(): Promise<void> {
    await this.#session.close()
  }

  async fetchPage(
    request: CatalogPageRequest,
    signal: AbortSignal,
    onProgress?: CatalogScanProgressListener,
  ): Promise<Uint8Array> {
    return this.#requests.run({ kind: 'list_children', signal }, route =>
      this.#fetchOnLane(request, route.laneId, route.signal, onProgress))
  }

  async #fetchOnLane(
    request: CatalogPageRequest,
    laneId: number,
    signal: AbortSignal,
    onProgress?: CatalogScanProgressListener,
  ): Promise<Uint8Array> {
    const operation = await this.#session.beginOperation(
      MESSAGE_KIND.listChildren,
      encodeListRequest(request.directoryId, request.generation, request.pageIndex),
      { laneId, signal },
    )
    let attemptId: Uint8Array<ArrayBuffer> | undefined
    let discoveredEntries = 0n
    while (true) {
      const message = await operation.next(signal)
      if (message.kind === MESSAGE_KIND.scanProgress) {
        const progress = decodeScanProgress(message.body)
        const replay = attemptId !== undefined &&
          equalBytes(attemptId, progress.attemptId) &&
          progress.discoveredEntries === discoveredEntries
        if (
          (attemptId !== undefined && !equalBytes(attemptId, progress.attemptId)) ||
          progress.discoveredEntries < discoveredEntries
        ) {
          const failure = new SessionRuntimeError(
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
      if (message.kind === MESSAGE_KIND.operationError) {
        throw remoteOperationErrorFor(this.#session, message)
      }
      if (message.kind !== MESSAGE_KIND.catalogResult) {
        throw new Error('Catalog operation received an unexpected response')
      }
      return decodeCatalogResult(message.body)
    }
  }
}
