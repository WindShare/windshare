import type {
  V2CatalogOperationClient,
  V2CatalogScanProgressListener,
} from '../catalog/v2-client'
import type { V2CatalogPageRequest } from '../catalog/v2-records'
import { equalBytes } from '../crypto/bytes'
import {
  decodeV2OperationErrorControl,
  decodeV2ScanProgress,
  V2_MESSAGE_KIND,
} from '../session/v2-message'
import type { V2ReceiverSessionRuntime } from '../session/v2-runtime'
import { V2SessionRuntimeError } from '../session/v2-runtime-types'
import { decodeV2CatalogResult, encodeV2ListRequest } from './v2-flow'

export class V2RemoteOperationError extends Error {
  readonly body: Uint8Array<ArrayBuffer>
  readonly scope: 'directory' | 'revision' | 'block' | 'peer'
  readonly code: number
  readonly retryable: boolean
  readonly retryAfterMilliseconds: number | undefined
  readonly remoteMessage: string

  constructor(body: Uint8Array) {
    const decoded = decodeV2OperationErrorControl(body)
    super(decoded.message.length === 0
      ? 'Sender rejected the authenticated operation'
      : decoded.message)
    this.name = 'V2RemoteOperationError'
    this.body = body.slice()
    this.scope = decoded.scope
    this.code = decoded.code
    this.retryable = decoded.retryable
    this.retryAfterMilliseconds = decoded.retryAfterMilliseconds
    this.remoteMessage = decoded.message
  }
}

export async function remoteOperationErrorFor(
  session: V2ReceiverSessionRuntime,
  body: Uint8Array,
  expected: 'directory' | 'revision' | 'block',
): Promise<V2RemoteOperationError> {
  let remote: V2RemoteOperationError
  try {
    remote = new V2RemoteOperationError(body)
  } catch (cause) {
    await session.close().catch(() => undefined)
    throw new V2SessionRuntimeError('session', 'Authenticated operation error was malformed', {
      cause,
    })
  }
  if (remote.scope !== expected) {
    await session.close().catch(() => undefined)
    throw new V2SessionRuntimeError(
      'session',
      'Authenticated operation error belonged to another operation scope',
      { cause: remote },
    )
  }
  return remote
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
        throw await remoteOperationErrorFor(this.#session, message.body, 'directory')
      }
      if (message.kind !== V2_MESSAGE_KIND.catalogResult) {
        throw new Error('Catalog operation received an unexpected response')
      }
      return decodeV2CatalogResult(message.body)
    }
  }
}
