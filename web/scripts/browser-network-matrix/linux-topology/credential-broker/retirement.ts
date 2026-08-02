import { createHash } from 'node:crypto'

import type { NetworkMatrixControlAuthority } from '../../sample-authority.ts'
import type {
  ExternalFixtureControlCredentialLeasePayload,
  ExternalFixtureControlCredentialReceipt,
  ExternalFixtureControlCredentialRetirementReceipt,
} from '../control-credential.ts'
import { authenticateRetirementResponse } from './authentication.ts'
import {
  EXTERNAL_FIXTURE_CREDENTIAL_BROKER_PROTOCOL,
  type AuthenticatedCredentialLease,
  type CredentialBrokerExchange,
  type CredentialBrokerScope,
} from './contracts.ts'

type CredentialRetirementOperation = ExternalFixtureControlCredentialReceipt['operation']

interface CredentialBrokerRetirementAttempt {
  readonly request: Readonly<Record<string, unknown>>
}

export interface CredentialBrokerLeaseRetirement {
  readonly registryKey: string
  readonly controlAuthority: NetworkMatrixControlAuthority
  readonly publicKey: string
  readonly scope: CredentialBrokerScope
  readonly authorityInstanceId: string
  readonly attestationSha256: string
  readonly leaseExpiresAt: string
  readonly releaseRequestId: string
  readonly revokeRequestId: string
  readonly release: CredentialBrokerRetirementAttempt
  readonly revoke: CredentialBrokerRetirementAttempt
  forceStarted: boolean
  releaseController?: AbortController
  revokeController?: AbortController
  releaseWorker?: Promise<void>
  revokeWorker?: Promise<void>
  resolveTerminal?: (receipt: ExternalFixtureControlCredentialRetirementReceipt) => void
  readonly terminalWait: Promise<ExternalFixtureControlCredentialRetirementReceipt>
  terminalReceipt?: ExternalFixtureControlCredentialRetirementReceipt
}

export class CredentialRetirementRegistry {
  readonly #exchange: CredentialBrokerExchange
  readonly #active = new Map<string, CredentialBrokerLeaseRetirement>()

  constructor(exchange: CredentialBrokerExchange) {
    this.#exchange = exchange
  }

  get hasActive(): boolean { return this.#active.size !== 0 }

  register(
    lease: AuthenticatedCredentialLease,
    publicKey: string,
    controllerOrigin: string,
  ): CredentialBrokerLeaseRetirement {
    const registryKey = credentialRetirementRegistryKey(lease, publicKey, controllerOrigin)
    const existing = this.#active.get(registryKey)
    if (existing !== undefined) return existing
    const newAttempt = (
      operation: CredentialRetirementOperation,
      requestId: string,
    ): CredentialBrokerRetirementAttempt => ({
      request: Object.freeze({
        schemaVersion: EXTERNAL_FIXTURE_CREDENTIAL_BROKER_PROTOCOL,
        operation,
        requestId,
        controllerOrigin,
        controlAuthority: lease.controlAuthority,
        releaseRequestId: lease.releaseRequestId,
        revokeRequestId: lease.revokeRequestId,
        probeNonce: lease.probeNonce,
      }),
    })
    let resolveTerminal!: (receipt: ExternalFixtureControlCredentialRetirementReceipt) => void
    const terminalWait = new Promise<ExternalFixtureControlCredentialRetirementReceipt>((resolve) => {
      resolveTerminal = resolve
    })
    const retirement: CredentialBrokerLeaseRetirement = {
      registryKey,
      controlAuthority: lease.controlAuthority,
      publicKey,
      scope: Object.freeze({
        sampleAuthority: lease.controlAuthority.sampleAuthority,
        probeNonce: lease.probeNonce,
      }),
      authorityInstanceId: lease.authorityInstanceId,
      attestationSha256: lease.attestationSha256,
      leaseExpiresAt: lease.expiresAt,
      releaseRequestId: lease.releaseRequestId,
      revokeRequestId: lease.revokeRequestId,
      release: newAttempt('release', lease.releaseRequestId),
      revoke: newAttempt('revoke-and-wait', lease.revokeRequestId),
      forceStarted: false,
      resolveTerminal,
      terminalWait,
    }
    this.#active.set(registryKey, retirement)
    return retirement
  }

  retire(
    retirement: CredentialBrokerLeaseRetirement,
    operation: CredentialRetirementOperation,
  ): Promise<ExternalFixtureControlCredentialRetirementReceipt> {
    if (retirement.terminalReceipt !== undefined) {
      return Promise.resolve(retirement.terminalReceipt)
    }
    if (operation === 'release' && retirement.forceStarted) {
      return this.retire(retirement, 'revoke-and-wait')
    }
    if (operation === 'release') return this.#release(retirement)
    return this.#revoke(retirement)
  }

  async rejectAcquiredLease(
    lease: AuthenticatedCredentialLease,
    retirement: CredentialBrokerLeaseRetirement,
    cause: Error,
  ): Promise<never> {
    lease.credential.fill(0)
    try {
      await this.retire(retirement, 'revoke-and-wait')
    } catch (cleanupCause) {
      throw new AggregateError(
        [cause, cleanupCause],
        'credential broker rejected a lease without settling remote ownership',
        { cause: cleanupCause },
      )
    }
    throw cause
  }

  async revokeAll(): Promise<readonly unknown[]> {
    const failures: unknown[] = []
    for (const retirement of [...this.#active.values()]) {
      try {
        await this.retire(retirement, 'revoke-and-wait')
      } catch (cause) {
        failures.push(cause)
      }
    }
    return failures
  }

  #release(
    retirement: CredentialBrokerLeaseRetirement,
  ): Promise<ExternalFixtureControlCredentialRetirementReceipt> {
    if (retirement.releaseWorker === undefined) {
      const controller = new AbortController()
      retirement.releaseController = controller
      const worker = this.#performRetirement(
        retirement,
        'release',
        retirement.release,
        controller.signal,
      ).then((receipt) => {
        if (!retirement.forceStarted) this.#publishReceipt(retirement, receipt)
      })
      const retryable = worker.catch((cause: unknown) => {
        if (retirement.releaseWorker === retryable) delete retirement.releaseWorker
        if (retirement.releaseController === controller) delete retirement.releaseController
        throw cause
      })
      retirement.releaseWorker = retryable
    }
    return retirement.releaseWorker.then(() => retirement.terminalWait)
  }

  #revoke(
    retirement: CredentialBrokerLeaseRetirement,
  ): Promise<ExternalFixtureControlCredentialRetirementReceipt> {
    retirement.forceStarted = true
    retirement.releaseController?.abort()
    if (retirement.revokeWorker === undefined) {
      const releaseToJoin = retirement.releaseWorker
      const controller = new AbortController()
      retirement.revokeController = controller
      const worker = this.#performRetirement(
        retirement,
        'revoke-and-wait',
        retirement.revoke,
        controller.signal,
      ).then(async (receipt) => {
        await releaseToJoin?.catch(() => undefined)
        this.#publishReceipt(retirement, receipt)
      })
      const retryable = worker.catch((cause: unknown) => {
        if (retirement.revokeWorker === retryable) delete retirement.revokeWorker
        if (retirement.revokeController === controller) delete retirement.revokeController
        throw cause
      })
      retirement.revokeWorker = retryable
    }
    return retirement.revokeWorker.then(() => retirement.terminalWait)
  }

  async #performRetirement(
    retirement: CredentialBrokerLeaseRetirement,
    operation: CredentialRetirementOperation,
    attempt: CredentialBrokerRetirementAttempt,
    signal: AbortSignal,
  ): Promise<ExternalFixtureControlCredentialRetirementReceipt> {
    const retired = await this.#exchange(attempt.request, retirement.scope, signal).result
    try {
      return authenticateRetirementResponse({
        bytes: retired,
        publicKey: retirement.publicKey,
        operation,
        requestId: attempt.request.requestId as string,
        retirement,
      })
    } finally {
      retired.fill(0)
    }
  }

  #publishReceipt(
    retirement: CredentialBrokerLeaseRetirement,
    receipt: ExternalFixtureControlCredentialRetirementReceipt,
  ): ExternalFixtureControlCredentialRetirementReceipt {
    if (retirement.terminalReceipt !== undefined) return retirement.terminalReceipt
    retirement.terminalReceipt = receipt
    retirement.resolveTerminal?.(retirement.terminalReceipt)
    delete retirement.resolveTerminal
    if (this.#active.get(retirement.registryKey) === retirement) {
      this.#active.delete(retirement.registryKey)
    }
    return retirement.terminalReceipt
  }
}

function credentialRetirementRegistryKey(
  lease: ExternalFixtureControlCredentialLeasePayload,
  publicKey: string,
  controllerOrigin: string,
): string {
  // Only authenticated public metadata participates so the raw credential can
  // never become a digest-backed local comparison oracle.
  return createHash('sha256')
    .update(`${JSON.stringify({
      requestId: lease.requestId,
      releaseRequestId: lease.releaseRequestId,
      revokeRequestId: lease.revokeRequestId,
      controlAuthority: lease.controlAuthority,
      probeNonce: lease.probeNonce,
      authorityInstanceId: lease.authorityInstanceId,
      attestationSha256: lease.attestationSha256,
      expiresAt: lease.expiresAt,
      controllerOrigin,
      publicKey,
    })}\n`, 'utf8')
    .digest('hex')
}
