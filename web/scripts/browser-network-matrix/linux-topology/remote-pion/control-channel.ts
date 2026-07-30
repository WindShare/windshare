import {
  REMOTE_PION_REQUEST_DEADLINE_MS,
  type RemotePionCallResult,
  type RemotePionRequest,
} from './contracts.ts'
import {
  RemotePionOperationAbortedError,
  RemotePionTransportUnavailableError,
} from './errors.ts'
import { executeRemotePionRequest } from './http-transport.ts'
import {
  isControlCredentialBytes,
  parseCanonicalObject,
  protocolInvalid,
  requireObservedControllerAddress,
  requireRemotePionStatus,
  systemInterfaceAddresses,
} from './protocol.ts'

interface RemotePionControlChannelOptions {
  readonly endpoint: URL
  readonly tlsCertificateAuthority: string | Buffer
  readonly tlsCertificateSha256: string
  readonly controlCredential: Uint8Array
  readonly controlLeaseId: string
  readonly request?: RemotePionRequest
  readonly localInterfaceAddresses?: () => readonly string[]
}

export class RemotePionControlChannel {
  readonly #endpoint: URL
  readonly #tlsServerName: string
  readonly #ca: string | Buffer
  readonly #certificateSha256: string
  readonly #credential: Uint8Array
  readonly #controlLeaseId: string
  readonly #request: RemotePionRequest
  readonly #localInterfaceAddresses: () => readonly string[]

  constructor(options: RemotePionControlChannelOptions) {
    this.#endpoint = options.endpoint
    this.#tlsServerName = options.endpoint.hostname
    this.#ca = options.tlsCertificateAuthority
    this.#certificateSha256 = options.tlsCertificateSha256
    this.#credential = options.controlCredential
    this.#controlLeaseId = options.controlLeaseId
    this.#request = options.request ?? executeRemotePionRequest
    this.#localInterfaceAddresses = options.localInterfaceAddresses ?? systemInterfaceAddresses
  }

  post(
    path: string,
    body: Readonly<Record<string, unknown>>,
    signal: AbortSignal,
    expectedStatus: number | readonly number[],
    authorityBound = false,
  ): Promise<RemotePionCallResult> {
    return this.call(
      path,
      JSON.stringify(body),
      signal,
      expectedStatus,
      'POST',
      authorityBound,
    )
  }

  async call(
    path: string,
    body: string | null,
    outerSignal: AbortSignal,
    expectedStatus: number | readonly number[],
    method: 'GET' | 'POST' | 'DELETE',
    authorityBound = false,
  ): Promise<RemotePionCallResult> {
    if (!isControlCredentialBytes(this.#credential)) {
      throw protocolInvalid('remote Pion control credential authority is invalid')
    }
    const controller = new AbortController()
    let timedOut = false
    const abort = (): void => controller.abort()
    outerSignal.addEventListener('abort', abort, { once: true })
    if (outerSignal.aborted) controller.abort()
    const timeout = setTimeout(() => {
      timedOut = true
      controller.abort()
    }, REMOTE_PION_REQUEST_DEADLINE_MS)
    try {
      let response
      try {
        response = await this.#request({
          endpoint: this.#endpoint,
          path,
          body,
          method,
          controlCredential: this.#credential,
          controlLeaseId: this.#controlLeaseId,
          tlsServerName: this.#tlsServerName,
          tlsCertificateAuthority: this.#ca,
          tlsCertificateSha256: this.#certificateSha256,
          signal: controller.signal,
        })
      } catch (cause) {
        if (outerSignal.aborted) throw new RemotePionOperationAbortedError()
        if (timedOut) throw new RemotePionTransportUnavailableError()
        throw cause
      }
      if (outerSignal.aborted) throw new RemotePionOperationAbortedError()
      if (timedOut || controller.signal.aborted) throw new RemotePionTransportUnavailableError()
      requireRemotePionStatus(response.statusCode, expectedStatus, authorityBound)
      const observedRemoteAddress = requireObservedControllerAddress(
        response,
        this.#certificateSha256,
        this.#localInterfaceAddresses(),
      )
      return Object.freeze({
        value: parseCanonicalObject(response.body),
        observedRemoteAddress,
        observedTlsCertificateSha256: response.observedTlsCertificateSha256,
      })
    } finally {
      clearTimeout(timeout)
      outerSignal.removeEventListener('abort', abort)
    }
  }
}
