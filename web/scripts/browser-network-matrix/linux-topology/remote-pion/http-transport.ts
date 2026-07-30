import { createHash, timingSafeEqual } from 'node:crypto'
import { request as httpsRequest, type RequestOptions } from 'node:https'
import { checkServerIdentity, type TLSSocket } from 'node:tls'

import type {
  RemotePionHttpResponse,
  RemotePionRequest,
} from './contracts.ts'
import {
  RemotePionProtocolError,
  RemotePionTransportUnavailableError,
} from './errors.ts'
import { isControlCredentialBytes, protocolInvalid, validOpaqueId } from './protocol.ts'

const MAXIMUM_RESPONSE_BYTES = 1_048_576
const CONTROL_LEASE_ID_HEADER = 'X-WindShare-Control-Lease-ID'

export const executeRemotePionRequest: RemotePionRequest = async (
  request,
): Promise<RemotePionHttpResponse> => new Promise((resolve, reject) => {
  if (
    !isControlCredentialBytes(request.controlCredential) ||
    !validOpaqueId(request.controlLeaseId)
  ) {
    reject(protocolInvalid('remote Pion control credential authority is invalid'))
    return
  }
  // Node's HTTP API requires a string header. Materializing it at this final
  // transport boundary keeps the erasable authority as bytes everywhere above it.
  const authorization = `Bearer ${new TextDecoder('ascii', { fatal: true })
    .decode(request.controlCredential)}`
  const options: RequestOptions = {
    protocol: 'https:',
    hostname: request.endpoint.hostname,
    port: request.endpoint.port,
    path: request.path,
    method: request.method,
    servername: request.tlsServerName,
    ca: request.tlsCertificateAuthority,
    minVersion: 'TLSv1.3',
    agent: false,
    headers: {
      Authorization: authorization,
      [CONTROL_LEASE_ID_HEADER]: request.controlLeaseId,
      Accept: 'application/json',
      ...(request.body === null
        ? {}
        : { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(request.body) }),
    },
    checkServerIdentity: (hostname, certificate) => {
      const hostnameFailure = checkServerIdentity(hostname, certificate)
      if (hostnameFailure !== undefined) {
        return new RemotePionProtocolError(
          'authority-binding-mismatch',
          'remote Pion TLS hostname identity mismatch',
        )
      }
      const expected = Buffer.from(request.tlsCertificateSha256, 'hex')
      const raw = certificate.raw === undefined
        ? Buffer.alloc(0)
        : Buffer.from(certificate.raw).subarray(0)
      const digest = createHash('sha256').update(raw).digest()
      return digest.length === expected.length && timingSafeEqual(digest, expected)
        ? undefined
        : new RemotePionProtocolError(
            'authority-binding-mismatch',
            'remote Pion TLS certificate pin mismatch',
          )
    },
    signal: request.signal,
  }
  const child = httpsRequest(options, (response) => {
    const chunks: Buffer[] = []
    let bytes = 0
    response.on('data', (chunk: Buffer) => {
      bytes += chunk.byteLength
      if (bytes > MAXIMUM_RESPONSE_BYTES) {
        child.destroy(protocolInvalid('remote Pion response exceeded its authority'))
        return
      }
      chunks.push(chunk)
    })
    response.once('end', () => {
      const socket = response.socket as TLSSocket
      const certificate = socket.getPeerCertificate()
      const raw = certificate.raw === undefined ? Buffer.alloc(0) : Buffer.from(certificate.raw)
      resolve(Object.freeze({
        statusCode: response.statusCode ?? 0,
        body: Buffer.concat(chunks).toString('utf8'),
        observedRemoteAddress: socket.remoteAddress ?? '',
        observedTlsCertificateSha256: createHash('sha256').update(raw).digest('hex'),
      }))
    })
  })
  child.once('error', (cause: Error & { readonly code?: string }) => {
    if (cause instanceof RemotePionProtocolError) {
      reject(cause)
      return
    }
    if (isTlsIdentityFailure(cause.code)) {
      reject(new RemotePionProtocolError(
        'authority-binding-mismatch',
        'remote Pion TLS identity could not be authenticated',
      ))
      return
    }
    reject(new RemotePionTransportUnavailableError())
  })
  if (request.body !== null) child.write(request.body)
  child.end()
})

function isTlsIdentityFailure(code: string | undefined): boolean {
  return code !== undefined && (
    code.startsWith('CERT_') || code.startsWith('ERR_TLS_CERT_') ||
    code.includes('SELF_SIGNED_CERT') || code.includes('UNABLE_TO_VERIFY') ||
    code.includes('UNABLE_TO_GET_ISSUER')
  )
}
