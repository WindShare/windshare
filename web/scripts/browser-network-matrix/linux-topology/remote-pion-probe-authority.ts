import { createHash, createPublicKey } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import { createSecureContext } from 'node:tls'

import type {
  ExternalFixtureTrustInspectionResult,
  ExternalFixtureTrustInspector,
} from '../runtime-authority.ts'
import type { NetworkMatrixProfileId } from '../vocabulary.ts'
import type {
  ExternalFixtureTrustConfig,
  NetworkMatrixExternalFixtureConfig,
} from './concrete-runtime-config.ts'

const MAXIMUM_TRUST_FILE_BYTES = 1_048_576
const SHA256_PATTERN = /^[a-f0-9]{64}$/u

export interface FilesystemExternalFixtureTrustInspectorOptions {
  readonly config: NetworkMatrixExternalFixtureConfig
}

/**
 * Profile preparation proves only local trust configuration. A remote call would
 * require a real sample credential and would therefore claim an authority that
 * does not exist until sample execution begins.
 */
export class FilesystemExternalFixtureTrustInspector implements ExternalFixtureTrustInspector {
  readonly #config: NetworkMatrixExternalFixtureConfig

  constructor(options: FilesystemExternalFixtureTrustInspectorOptions) {
    this.#config = options.config
  }

  inspect(input: {
    readonly profileId: NetworkMatrixProfileId
    readonly expectedAttestationPublicKeySha256: string
    readonly signal: AbortSignal
  }) {
    const controller = new AbortController()
    const abort = (): void => controller.abort()
    input.signal.addEventListener('abort', abort, { once: true })
    if (input.signal.aborted) controller.abort()
    const result = inspectFilesystemTrust(
      fixtureForProfile(this.#config, input.profileId),
      input.profileId,
      input.expectedAttestationPublicKeySha256,
      controller.signal,
    ).finally(() => {
      input.signal.removeEventListener('abort', abort)
    })
    return Object.freeze({
      result,
      forceTerminateAndWait: async (): Promise<void> => {
        controller.abort()
        await result.catch(() => undefined)
      },
    })
  }
}

async function inspectFilesystemTrust(
  fixture: ExternalFixtureTrustConfig | null,
  profileId: NetworkMatrixProfileId,
  expectedAttestationPublicKeySha256: string,
  signal: AbortSignal,
): Promise<ExternalFixtureTrustInspectionResult> {
  if (fixture === null) {
    return Object.freeze({ outcome: 'unavailable', failureCode: 'authority-not-provisioned' })
  }
  try {
    requireActive(signal)
    const [tlsCertificateAuthority, attestationPublicKey] = await Promise.all([
      boundedTrustFile(fixture.control.tlsCertificateAuthorityFile, signal),
      boundedTrustFile(fixture.control.attestationPublicKeyFile, signal),
    ])
    requireActive(signal)
    const tlsCertificateAuthoritySha256 = createHash('sha256')
      .update(tlsCertificateAuthority)
      .digest('hex')
    try {
      createSecureContext({ ca: tlsCertificateAuthority })
    } catch {
      throw new InvalidExternalFixtureTrustError()
    }
    let key: ReturnType<typeof createPublicKey>
    try {
      key = createPublicKey(attestationPublicKey)
    } catch {
      throw new InvalidExternalFixtureTrustError()
    }
    const canonicalKey = Buffer.from(key.export({ type: 'spki', format: 'der' }))
    const attestationPublicKeySha256 = createHash('sha256').update(canonicalKey).digest('hex')
    if (
      key.asymmetricKeyType !== 'ed25519' ||
      !SHA256_PATTERN.test(expectedAttestationPublicKeySha256) ||
      attestationPublicKeySha256 !== expectedAttestationPublicKeySha256
    ) throw new InvalidExternalFixtureTrustError()
    return Object.freeze({
      outcome: 'satisfied',
      profileId,
      trust: Object.freeze({
        controllerOrigin: fixture.control.controllerOrigin,
        tlsCertificateSha256: fixture.control.tlsCertificateSha256,
        tlsCertificateAuthoritySha256,
        attestationPublicKeySpki: canonicalKey.toString('base64url'),
        attestationPublicKeySha256,
      }),
    })
  } catch (cause) {
    if (signal.aborted) {
      throw new Error('external fixture trust inspection was terminated', { cause })
    }
    if (cause instanceof InvalidExternalFixtureTrustError) {
      return Object.freeze({ outcome: 'invalid', failureCode: 'proof-invalid' })
    }
    if (isMissingFile(cause)) {
      return Object.freeze({ outcome: 'unavailable', failureCode: 'authority-not-provisioned' })
    }
    return Object.freeze({ outcome: 'failed', failureCode: 'runtime-check-failed' })
  }
}

async function boundedTrustFile(path: string, signal: AbortSignal): Promise<Buffer> {
  const bytes = await readFile(path, { signal })
  if (bytes.byteLength === 0 || bytes.byteLength > MAXIMUM_TRUST_FILE_BYTES) {
    throw new InvalidExternalFixtureTrustError()
  }
  return bytes
}

function fixtureForProfile(
  config: NetworkMatrixExternalFixtureConfig,
  profileId: NetworkMatrixProfileId,
): ExternalFixtureTrustConfig | null {
  return {
    'scheduled-public-stun': config.publicStun,
    'scheduled-restricted-udp': config.restrictedUdp,
    'scheduled-coturn': config.coturn,
    'manual-real-nat': config.manualRealNat,
  }[profileId]
}

function requireActive(signal: AbortSignal): void {
  if (signal.aborted) throw new Error('external fixture trust inspection was terminated')
}

function isMissingFile(value: unknown): boolean {
  return typeof value === 'object' && value !== null &&
    'code' in value && (value as { readonly code?: unknown }).code === 'ENOENT'
}

class InvalidExternalFixtureTrustError extends Error {}
