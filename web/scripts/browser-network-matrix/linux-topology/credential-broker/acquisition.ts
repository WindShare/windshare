import { randomBytes } from 'node:crypto'

import {
  parseNetworkMatrixSampleAuthority,
  sameNetworkMatrixSampleAuthority,
  type NetworkMatrixSampleAuthority,
} from '../../sample-authority.ts'
import type { NetworkMatrixProfileId } from '../../vocabulary.ts'
import type { NetworkMatrixExternalFixtureConfig } from '../concrete-runtime-config.ts'
import type { ExternalFixtureControlCredentialLease } from '../control-credential.ts'
import { authenticateLeaseResponse, readPinnedPublicKey } from './authentication.ts'
import {
  EXTERNAL_FIXTURE_CREDENTIAL_BROKER_PROTOCOL,
  type AuthenticatedCredentialLease,
  type CredentialBrokerDispatchOutcome,
  type CredentialBrokerExchange,
  type CredentialBrokerScope,
  type CredentialBrokerSecretStore,
} from './contracts.ts'
import { CredentialRetirementRegistry } from './retirement.ts'

const MAXIMUM_OBSERVED_LEASE_IDS = 4_096
const OPAQUE_ID_PATTERN = /^[A-Za-z0-9_-]{16,128}$/u

interface CredentialBrokerAcquisition {
  readonly requestId: string
  readonly releaseRequestId: string
  readonly revokeRequestId: string
  readonly request: Readonly<Record<string, unknown>>
  readonly scope: CredentialBrokerScope
  readonly controllerOrigin: string
  readonly publicKeyFile: string
  readonly controller: AbortController
  readonly settled: Promise<void>
  settle(): void
  publicKey?: string
  recoveryController?: AbortController
  recoveryWorker?: Promise<void>
}

interface CredentialAcquisitionRegistryOptions {
  readonly config: NetworkMatrixExternalFixtureConfig
  readonly secretStore: CredentialBrokerSecretStore | undefined
  readonly exchange: CredentialBrokerExchange
  readonly retirements: CredentialRetirementRegistry
}

export class CredentialAcquisitionRegistry {
  readonly #options: CredentialAcquisitionRegistryOptions
  readonly #observedLeaseIds = new Map<string, number>()
  readonly #pending = new Map<string, CredentialBrokerAcquisition>()
  readonly #ambiguous = new Map<string, CredentialBrokerAcquisition>()
  #accepting = true

  constructor(options: CredentialAcquisitionRegistryOptions) {
    this.#options = Object.freeze({ ...options })
  }

  get hasAmbiguous(): boolean { return this.#ambiguous.size !== 0 }

  async acquire(input: {
    readonly sampleAuthority: NetworkMatrixSampleAuthority
    readonly probeNonce: string
    readonly signal: AbortSignal
  }): Promise<ExternalFixtureControlCredentialLease> {
    if (!this.#accepting) throw new Error('external fixture credential broker is closed')
    const sampleAuthority = parseNetworkMatrixSampleAuthority(input.sampleAuthority)
    requireBrokerScope(sampleAuthority, input.probeNonce)
    requireActive(input.signal)
    const fixture = fixtureForProfile(this.#options.config, sampleAuthority.profileId)
    if (fixture === null) throw new Error('credential broker requested an unprovisioned fixture')
    const acquisition = createCredentialBrokerAcquisition({
      sampleAuthority,
      probeNonce: input.probeNonce,
      controllerOrigin: fixture.control.controllerOrigin,
      publicKeyFile: fixture.control.attestationPublicKeyFile,
    })
    const { requestId, controller, scope } = acquisition
    const abort = (): void => controller.abort()
    input.signal.addEventListener('abort', abort, { once: true })
    if (input.signal.aborted) controller.abort()
    // Registration precedes every await so shutdown cannot miss an admitted
    // acquisition that has not reached the helper process yet.
    this.#pending.set(requestId, acquisition)
    let authenticatedRemoteLease = false
    let dispatchOutcome: Promise<CredentialBrokerDispatchOutcome> | undefined
    try {
      const publicKey = await readPinnedPublicKey(acquisition.publicKeyFile, controller.signal)
      acquisition.publicKey = publicKey
      const exchange = this.#options.exchange(
        acquisition.request,
        scope,
        controller.signal,
      )
      dispatchOutcome = exchange.dispatchOutcome
      const response = await exchange.result
      let lease: AuthenticatedCredentialLease
      try {
        lease = authenticateLeaseResponse(response, publicKey, this.#options.secretStore)
      } finally {
        response.fill(0)
      }
      const retirement = this.#options.retirements.register(
        lease,
        publicKey,
        acquisition.controllerOrigin,
      )
      if (!sameCredentialBrokerAcquisition(lease, acquisition)) {
        return await this.#options.retirements.rejectAcquiredLease(
          lease,
          retirement,
          new Error('credential broker lease crossed its exact acquisition replay scope'),
        )
      }
      authenticatedRemoteLease = true
      const duplicate = this.#hasObservedLease(lease.controlAuthority.controlLeaseId)
      const transferFailure = credentialLeaseTransferFailure(
        this.#accepting,
        controller.signal.aborted,
        duplicate,
      )
      if (transferFailure !== null) {
        return await this.#options.retirements.rejectAcquiredLease(
          lease,
          retirement,
          transferFailure,
        )
      }
      try {
        this.#rememberLease(lease.controlAuthority.controlLeaseId, lease.expiresAt)
      } catch (cause) {
        return await this.#options.retirements.rejectAcquiredLease(
          lease,
          retirement,
          cause instanceof Error ? cause : new Error('credential broker lease registry failed'),
        )
      }
      const {
        signedLease, requestId: signedRequestId,
        controlAuthority, releaseRequestId: signedReleaseRequestId,
        revokeRequestId: signedRevokeRequestId, probeNonce, authorityInstanceId,
        attestationSha256, issuedAt, expiresAt, credential, turnCapability,
        turnProviderLeaseId, turnCredentialId, turnUsername, turnExpiresAt,
      } = lease
      return Object.freeze({
        signedLease,
        requestId: signedRequestId,
        releaseRequestId: signedReleaseRequestId,
        revokeRequestId: signedRevokeRequestId,
        controlAuthority,
        probeNonce,
        authorityInstanceId,
        attestationSha256,
        issuedAt,
        expiresAt,
        maxAttempts: 1,
        turnCapability,
        turnProviderLeaseId,
        turnCredentialId,
        turnUsername,
        turnExpiresAt,
        credential,
        release: () => this.#options.retirements.retire(retirement, 'release'),
        revokeAndWait: () => this.#options.retirements.retire(retirement, 'revoke-and-wait'),
      })
    } catch (cause) {
      const dispatched = await credentialBrokerWasDispatched(dispatchOutcome)
      if (dispatched && !authenticatedRemoteLease) {
        this.#ambiguous.set(requestId, acquisition)
        try {
          await this.#recoverAmbiguousAcquisition(acquisition)
        } catch (cleanupCause) {
          throw new AggregateError(
            [cause, cleanupCause],
            'credential broker acquisition failed without settling remote ownership',
            { cause: cleanupCause },
          )
        }
      }
      throw cause
    } finally {
      input.signal.removeEventListener('abort', abort)
      this.#pending.delete(requestId)
      acquisition.settle()
    }
  }

  stopAdmission(): void {
    this.#accepting = false
    for (const acquisition of this.#pending.values()) acquisition.controller.abort()
  }

  async awaitPending(): Promise<void> {
    const pending = [...this.#pending.values()]
    for (const acquisition of pending) acquisition.controller.abort()
    await Promise.all(pending.map(({ settled }) => settled))
  }

  async recoverAllAmbiguous(): Promise<readonly unknown[]> {
    const failures: unknown[] = []
    for (const acquisition of [...this.#ambiguous.values()]) {
      try {
        await this.#recoverAmbiguousAcquisition(acquisition)
      } catch (cause) {
        failures.push(cause)
      }
    }
    return failures
  }

  async #recoverAmbiguousAcquisition(acquisition: CredentialBrokerAcquisition): Promise<void> {
    if (this.#ambiguous.get(acquisition.requestId) !== acquisition) return
    if (acquisition.publicKey === undefined) {
      throw new Error('credential broker cannot authenticate ambiguous acquisition replay')
    }
    if (acquisition.recoveryWorker !== undefined) return acquisition.recoveryWorker
    const controller = new AbortController()
    acquisition.recoveryController = controller
    const worker = this.#recoverAmbiguousAcquisitionOnce(acquisition, controller.signal)
    const retryable = worker.catch((cause: unknown) => {
      if (acquisition.recoveryWorker === retryable) delete acquisition.recoveryWorker
      if (acquisition.recoveryController === controller) delete acquisition.recoveryController
      throw cause
    })
    acquisition.recoveryWorker = retryable
    return retryable
  }

  async #recoverAmbiguousAcquisitionOnce(
    acquisition: CredentialBrokerAcquisition,
    signal: AbortSignal,
  ): Promise<void> {
    const publicKey = acquisition.publicKey
    if (publicKey === undefined) throw new Error('credential broker replay trust anchor is absent')
    const exchange = this.#options.exchange(
      acquisition.request,
      acquisition.scope,
      signal,
    )
    const response = await exchange.result
    let lease: AuthenticatedCredentialLease
    try {
      lease = authenticateLeaseResponse(response, publicKey, this.#options.secretStore)
    } finally {
      response.fill(0)
    }
    const bindingFailure = !sameCredentialBrokerAcquisition(lease, acquisition)
    const retirement = this.#options.retirements.register(
      lease,
      publicKey,
      acquisition.controllerOrigin,
    )
    lease.credential.fill(0)
    await this.#options.retirements.retire(retirement, 'revoke-and-wait')
    if (bindingFailure) {
      throw new Error('credential broker replay crossed its exact acquisition scope')
    }
    this.#ambiguous.delete(acquisition.requestId)
  }

  #hasObservedLease(leaseId: string): boolean {
    this.#pruneObservedLeases()
    return this.#observedLeaseIds.has(leaseId)
  }

  #rememberLease(leaseId: string, expiresAt: string): void {
    this.#pruneObservedLeases()
    if (this.#observedLeaseIds.size >= MAXIMUM_OBSERVED_LEASE_IDS) {
      throw new Error('credential broker observed lease authority is exhausted')
    }
    this.#observedLeaseIds.set(leaseId, Date.parse(expiresAt))
  }

  #pruneObservedLeases(): void {
    const now = Date.now()
    for (const [leaseId, expiresAt] of this.#observedLeaseIds) {
      if (expiresAt <= now) this.#observedLeaseIds.delete(leaseId)
    }
  }
}

function createCredentialBrokerAcquisition(input: {
  readonly sampleAuthority: NetworkMatrixSampleAuthority
  readonly probeNonce: string
  readonly controllerOrigin: string
  readonly publicKeyFile: string
}): CredentialBrokerAcquisition {
  const requestId = randomBytes(24).toString('base64url')
  const releaseRequestId = randomBytes(24).toString('base64url')
  const revokeRequestId = randomBytes(24).toString('base64url')
  const scope = Object.freeze({
    sampleAuthority: input.sampleAuthority,
    probeNonce: input.probeNonce,
  })
  const request = Object.freeze({
    schemaVersion: EXTERNAL_FIXTURE_CREDENTIAL_BROKER_PROTOCOL,
    operation: 'acquire',
    requestId,
    releaseRequestId,
    revokeRequestId,
    controllerOrigin: input.controllerOrigin,
    sampleAuthority: input.sampleAuthority,
    probeNonce: input.probeNonce,
    maxAttempts: 1,
  })
  const controller = new AbortController()
  let settle: (() => void) | undefined
  const settled = new Promise<void>((resolve) => { settle = resolve })
  return {
    requestId,
    releaseRequestId,
    revokeRequestId,
    request,
    scope,
    controllerOrigin: input.controllerOrigin,
    publicKeyFile: input.publicKeyFile,
    controller,
    settled,
    settle: () => settle?.(),
  }
}

function sameCredentialBrokerAcquisition(
  lease: AuthenticatedCredentialLease,
  acquisition: CredentialBrokerAcquisition,
): boolean {
  return lease.requestId === acquisition.requestId &&
    lease.releaseRequestId === acquisition.releaseRequestId &&
    lease.revokeRequestId === acquisition.revokeRequestId &&
    sameNetworkMatrixSampleAuthority(
      lease.controlAuthority.sampleAuthority,
      acquisition.scope.sampleAuthority,
    ) && lease.probeNonce === acquisition.scope.probeNonce
}

function credentialLeaseTransferFailure(
  accepting: boolean,
  aborted: boolean,
  duplicate: boolean,
): Error | null {
  if (!accepting || aborted) {
    return new Error('credential broker acquisition was terminated before ownership transfer')
  }
  if (duplicate) return new Error('credential broker lease crossed its one-shot request scope')
  return null
}

function fixtureForProfile(
  config: NetworkMatrixExternalFixtureConfig,
  profileId: NetworkMatrixProfileId,
) {
  return {
    'scheduled-public-stun': config.publicStun,
    'scheduled-restricted-udp': config.restrictedUdp,
    'scheduled-coturn': config.coturn,
  }[profileId]
}

function requireBrokerScope(
  sampleAuthority: NetworkMatrixSampleAuthority,
  probeNonce: string,
): void {
  parseNetworkMatrixSampleAuthority(sampleAuthority)
  if (!OPAQUE_ID_PATTERN.test(probeNonce)) {
    throw new Error('credential broker request scope is invalid')
  }
}

async function credentialBrokerWasDispatched(
  outcome: Promise<CredentialBrokerDispatchOutcome> | undefined,
): Promise<boolean> {
  return outcome !== undefined && await outcome === 'dispatched'
}

function requireActive(signal: AbortSignal): void {
  if (signal.aborted) throw new Error('credential broker operation was terminated')
}
