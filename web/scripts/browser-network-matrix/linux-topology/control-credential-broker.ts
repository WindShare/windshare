import { createHash, randomBytes } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import { isAbsolute, resolve } from 'node:path'

import { executeNativeProcessGroupCommand } from '../../browser-evidence/process/native-process-group-backend.ts'
import { inheritedSampleEnvironment } from '../../browser-evidence/process/sample-environment.ts'
import { executeWindowsJob } from '../../browser-evidence/process/windows-job-client.ts'
import type { BrowserSampleContainmentExecution } from '../../browser-evidence/process/containment.ts'
import type { NetworkMatrixProfileId } from '../vocabulary.ts'
import {
  parseNetworkMatrixSampleAuthority,
  sameNetworkMatrixControlAuthority,
  sameNetworkMatrixSampleAuthority,
  type NetworkMatrixControlAuthority,
  type NetworkMatrixSampleAuthority,
} from '../sample-authority.ts'
import type { NetworkMatrixExternalFixtureConfig } from './concrete-runtime-config.ts'
import type {
  ExternalFixtureControlCredentialAuthority,
  ExternalFixtureControlCredentialAuthorityReceipt,
  ExternalFixtureControlCredentialLease,
  ExternalFixtureControlCredentialReceipt,
  ExternalFixtureControlCredentialRetirementReceipt,
  ExternalFixtureControlCredentialLeasePayload,
  SignedExternalFixtureControlCredentialLease,
} from './control-credential.ts'
import {
  authenticateSignedExternalFixtureControlCredentialLease,
  authenticateSignedExternalFixtureControlCredentialReceipt,
  parseSignedExternalFixtureControlCredentialLease,
  parseSignedExternalFixtureControlCredentialReceipt,
} from './control-credential.ts'
import {
  PARENT_WORKLOAD_IDENTITY_PROTOCOL,
  type ParentWorkloadIdentityAuthority,
  type ParentWorkloadIdentityBinding,
} from './parent-workload-identity.ts'

export const EXTERNAL_FIXTURE_CREDENTIAL_BROKER_PROTOCOL =
  'windshare.browser-network-matrix.credential-broker/v2' as const

const BROKER_OPERATION_DEADLINE_MS = 15_000
const BROKER_TERMINATION_GRACE_MS = 5_000
const MAXIMUM_BROKER_FRAME_BYTES = 1_048_576
const BROKER_METADATA_LENGTH_BYTES = 4
const MINIMUM_CREDENTIAL_BYTES = 32
const MAXIMUM_CREDENTIAL_BYTES = 512
const MAXIMUM_OBSERVED_LEASE_IDS = 4_096
const OPAQUE_ID_PATTERN = /^[A-Za-z0-9_-]{16,128}$/u

export interface CredentialBrokerPipeExchange {
  exchange(input: {
    readonly operationId: string
    readonly stdin: Uint8Array
    readonly signal: AbortSignal
  }): Promise<Uint8Array>
}

export interface CredentialBrokerSecretStore {
  /** Adopt into a distinct erasable allocation owned exclusively by the caller. */
  adopt(source: Uint8Array): Uint8Array
}

export interface CredentialBrokerOptions {
  readonly helperPath: string
  readonly workingDirectory: string
  readonly platform: NodeJS.Platform
  readonly windowsJobHelperPath?: string
  readonly config: NetworkMatrixExternalFixtureConfig
  readonly workloadIdentity: ParentWorkloadIdentityAuthority
  readonly trace?: (event: Readonly<Record<string, unknown>>) => void
}

export interface CredentialBrokerTestHarnessOptions extends CredentialBrokerOptions {
  readonly pipeExchange: CredentialBrokerPipeExchange
  readonly secretStore?: CredentialBrokerSecretStore
}

interface InternalCredentialBrokerOptions extends CredentialBrokerOptions {
  readonly pipeExchange?: CredentialBrokerPipeExchange
  readonly secretStore?: CredentialBrokerSecretStore
}

interface AuthenticatedCredentialLease extends ExternalFixtureControlCredentialLeasePayload {
  readonly signedLease: SignedExternalFixtureControlCredentialLease
  readonly credential: Uint8Array
}

type CredentialRetirementPayload = ExternalFixtureControlCredentialReceipt

interface CredentialBrokerScope {
  readonly sampleAuthority: NetworkMatrixSampleAuthority
  readonly probeNonce: string
}

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
  dispatched: boolean
  publicKey?: string
  recoveryController?: AbortController
  recoveryWorker?: Promise<void>
}

interface CredentialBrokerRetirementAttempt {
  readonly request: Readonly<Record<string, unknown>>
}

interface CredentialBrokerLeaseRetirement {
  readonly registryKey: string
  readonly controlAuthority: NetworkMatrixControlAuthority
  readonly publicKey: string
  readonly scope: CredentialBrokerScope
  readonly authorityInstanceId: string
  readonly attestationSha256: string
  readonly leaseExpiresAt: string
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

/**
 * The broker executable owns host authentication. The parent grants it no
 * credential-bearing argv or environment channel. Signed non-secret lease metadata
 * crosses with raw bytes on a fresh exact-length anonymous pipe; the raw credential
 * is never hashed, signed, serialized, or exposed as a public verification oracle.
 */
export class ProcessExternalFixtureControlCredentialAuthority
implements ExternalFixtureControlCredentialAuthority {
  readonly #options: InternalCredentialBrokerOptions
  readonly #observedLeaseIds = new Map<string, number>()
  readonly #pendingAcquisitions = new Map<string, CredentialBrokerAcquisition>()
  readonly #ambiguousAcquisitions = new Map<string, CredentialBrokerAcquisition>()
  readonly #activeRetirements = new Map<string, CredentialBrokerLeaseRetirement>()
  #accepting = true
  #terminalReceipt: ExternalFixtureControlCredentialAuthorityReceipt | undefined
  #closeOperation: Promise<ExternalFixtureControlCredentialAuthorityReceipt> | undefined
  #forceOperation: Promise<ExternalFixtureControlCredentialAuthorityReceipt> | undefined

  static create(
    options: CredentialBrokerOptions,
  ): ProcessExternalFixtureControlCredentialAuthority {
    return new ProcessExternalFixtureControlCredentialAuthority({
      helperPath: options.helperPath,
      workingDirectory: options.workingDirectory,
      platform: options.platform,
      ...(options.windowsJobHelperPath === undefined
        ? {}
        : { windowsJobHelperPath: options.windowsJobHelperPath }),
      config: options.config,
      workloadIdentity: options.workloadIdentity,
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
  }

  static createTestHarness(
    options: CredentialBrokerTestHarnessOptions,
  ): ProcessExternalFixtureControlCredentialAuthority {
    return new ProcessExternalFixtureControlCredentialAuthority(options)
  }

  private constructor(options: InternalCredentialBrokerOptions) {
    if (
      !canonicalAbsolutePath(options.helperPath) ||
      !canonicalAbsolutePath(options.workingDirectory) ||
      options.platform !== 'linux' && options.platform !== 'win32' ||
      options.platform === 'win32' && !canonicalAbsolutePath(options.windowsJobHelperPath) ||
      options.platform === 'linux' && options.windowsJobHelperPath !== undefined ||
      typeof options.workloadIdentity?.issue !== 'function' ||
      typeof options.workloadIdentity?.closeAndWait !== 'function' ||
      typeof options.workloadIdentity?.forceTerminateAndWait !== 'function' ||
      !validWorkloadIdentityBinding(options.workloadIdentity?.binding) ||
      options.pipeExchange !== undefined && typeof options.pipeExchange.exchange !== 'function' ||
      options.secretStore !== undefined && typeof options.secretStore.adopt !== 'function'
    ) throw new Error('external fixture credential broker composition is invalid')
    this.#options = Object.freeze({ ...options })
  }

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
    // Registration precedes every await so close cannot miss an acquisition
    // that has passed admission but has not yet reached the helper process.
    this.#pendingAcquisitions.set(requestId, acquisition)
    let authenticatedRemoteLease = false
    try {
      const publicKey = await readPinnedPublicKey(acquisition.publicKeyFile, controller.signal)
      acquisition.publicKey = publicKey
      const response = await this.#exchange(
        acquisition.request,
        scope,
        controller.signal,
        () => { acquisition.dispatched = true },
      )
      let lease: AuthenticatedCredentialLease
      try {
        lease = authenticateLeaseResponse(response, publicKey, this.#options.secretStore)
      } finally {
        response.fill(0)
      }
      const retirement = this.#registerRetirement(
        lease,
        publicKey,
        acquisition.controllerOrigin,
      )
      if (!sameCredentialBrokerAcquisition(lease, acquisition)) {
        return await this.#rejectAcquiredLease(
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
        return await this.#rejectAcquiredLease(lease, retirement, transferFailure)
      }
      try {
        this.#rememberLease(lease.controlAuthority.controlLeaseId, lease.expiresAt)
      } catch (cause) {
        return await this.#rejectAcquiredLease(
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
        release: () => this.#retire(retirement, 'release'),
        revokeAndWait: () => this.#retire(retirement, 'revoke-and-wait'),
      })
    } catch (cause) {
      if (acquisition.dispatched && !authenticatedRemoteLease) {
        this.#ambiguousAcquisitions.set(requestId, acquisition)
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
      this.#pendingAcquisitions.delete(requestId)
      acquisition.settle()
    }
  }

  closeAndWait(): Promise<ExternalFixtureControlCredentialAuthorityReceipt> {
    this.#stopAdmission()
    if (this.#terminalReceipt !== undefined) return Promise.resolve(this.#terminalReceipt)
    if (this.#forceOperation !== undefined) return this.#forceOperation
    if (this.#closeOperation !== undefined) return this.#closeOperation
    const operation = this.#closeNormally().catch((cause: unknown) => {
      if (this.#closeOperation === operation) this.#closeOperation = undefined
      throw cause
    })
    this.#closeOperation = operation
    return this.#closeOperation
  }

  forceTerminateAndWait(): Promise<ExternalFixtureControlCredentialAuthorityReceipt> {
    this.#stopAdmission()
    if (this.#terminalReceipt !== undefined) return Promise.resolve(this.#terminalReceipt)
    if (this.#forceOperation !== undefined) return this.#forceOperation
    const gracefulToJoin = this.#closeOperation
    const operation = this.#forceClose().then(async (receipt) => {
      // A forced terminal subsumes a graceful failure, but it does not make an
      // already-started worker disappear. Join it before publishing settlement.
      await gracefulToJoin?.catch(() => undefined)
      return receipt
    }).catch((cause: unknown) => {
      if (this.#forceOperation === operation) this.#forceOperation = undefined
      throw cause
    })
    this.#forceOperation = operation
    return this.#forceOperation
  }

  async #closeNormally(): Promise<ExternalFixtureControlCredentialAuthorityReceipt> {
    await this.#awaitPendingAcquisitions()
    if (this.#ambiguousAcquisitions.size !== 0 || this.#activeRetirements.size !== 0) {
      throw new Error('external fixture credential broker still owns active leases')
    }
    return this.#closeIdentity(false)
  }

  async #forceClose(): Promise<ExternalFixtureControlCredentialAuthorityReceipt> {
    await this.#awaitPendingAcquisitions()
    const failures: unknown[] = []
    for (const acquisition of [...this.#ambiguousAcquisitions.values()]) {
      try {
        await this.#recoverAmbiguousAcquisition(acquisition)
      } catch (cause) {
        failures.push(cause)
      }
    }
    for (const retirement of [...this.#activeRetirements.values()]) {
      try {
        await this.#retire(retirement, 'revoke-and-wait')
      } catch (cause) {
        failures.push(cause)
      }
    }
    if (
      failures.length !== 0 || this.#ambiguousAcquisitions.size !== 0 ||
      this.#activeRetirements.size !== 0
    ) {
      throw new AggregateError(
        failures,
        'external fixture credential broker force termination did not settle ownership',
      )
    }
    return this.#closeIdentity(true)
  }

  async #closeIdentity(force: boolean): Promise<ExternalFixtureControlCredentialAuthorityReceipt> {
    if (this.#terminalReceipt !== undefined) return this.#terminalReceipt
    const receipt = force
      ? await this.#options.workloadIdentity.forceTerminateAndWait()
      : await this.#options.workloadIdentity.closeAndWait()
    if (!exactClosedReceipt(receipt)) {
      throw new Error('parent workload identity did not publish its terminal receipt')
    }
    return this.#publishTerminalReceipt()
  }

  #publishTerminalReceipt(): ExternalFixtureControlCredentialAuthorityReceipt {
    if (this.#terminalReceipt !== undefined) return this.#terminalReceipt
    this.#terminalReceipt = Object.freeze({ terminal: 'closed' })
    return this.#terminalReceipt
  }

  #stopAdmission(): void {
    this.#accepting = false
    for (const acquisition of this.#pendingAcquisitions.values()) {
      acquisition.controller.abort()
    }
  }

  async #awaitPendingAcquisitions(): Promise<void> {
    const pending = [...this.#pendingAcquisitions.values()]
    for (const acquisition of pending) acquisition.controller.abort()
    await Promise.all(pending.map(({ settled }) => settled))
  }

  #registerRetirement(
    lease: AuthenticatedCredentialLease,
    publicKey: string,
    controllerOrigin: string,
  ): CredentialBrokerLeaseRetirement {
    const registryKey = credentialRetirementRegistryKey(lease, publicKey, controllerOrigin)
    const existing = this.#activeRetirements.get(registryKey)
    if (existing !== undefined) return existing
    const newAttempt = (
      operation: CredentialRetirementPayload['operation'],
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
      release: newAttempt('release', lease.releaseRequestId),
      revoke: newAttempt('revoke-and-wait', lease.revokeRequestId),
      forceStarted: false,
      resolveTerminal,
      terminalWait,
    }
    this.#activeRetirements.set(registryKey, retirement)
    return retirement
  }

  #retire(
    retirement: CredentialBrokerLeaseRetirement,
    operation: CredentialRetirementPayload['operation'],
  ): Promise<ExternalFixtureControlCredentialRetirementReceipt> {
    if (retirement.terminalReceipt !== undefined) {
      return Promise.resolve(retirement.terminalReceipt)
    }
    if (operation === 'release' && retirement.forceStarted) {
      return this.#retire(retirement, 'revoke-and-wait')
    }
    if (operation === 'release') {
      if (retirement.releaseWorker === undefined) {
        const controller = new AbortController()
        retirement.releaseController = controller
        const worker = this.#performRetirement(
          retirement,
          'release',
          retirement.release,
          controller.signal,
        ).then((receipt) => {
          if (!retirement.forceStarted) this.#publishRetirementReceipt(retirement, receipt)
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
        this.#publishRetirementReceipt(retirement, receipt)
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
    operation: CredentialRetirementPayload['operation'],
    attempt: CredentialBrokerRetirementAttempt,
    signal: AbortSignal,
  ): Promise<ExternalFixtureControlCredentialRetirementReceipt> {
    const retired = await this.#exchange(attempt.request, retirement.scope, signal)
    try {
      return authenticateRetirementResponse(
        retired,
        retirement.publicKey,
        operation,
        attempt.request.requestId as string,
        retirement,
      )
    } finally {
      retired.fill(0)
    }
  }

  #publishRetirementReceipt(
    retirement: CredentialBrokerLeaseRetirement,
    receipt: ExternalFixtureControlCredentialRetirementReceipt,
  ): ExternalFixtureControlCredentialRetirementReceipt {
    if (retirement.terminalReceipt !== undefined) return retirement.terminalReceipt
    retirement.terminalReceipt = receipt
    retirement.resolveTerminal?.(retirement.terminalReceipt)
    delete retirement.resolveTerminal
    if (this.#activeRetirements.get(retirement.registryKey) === retirement) {
      this.#activeRetirements.delete(retirement.registryKey)
    }
    return retirement.terminalReceipt
  }

  async #rejectAcquiredLease(
    lease: AuthenticatedCredentialLease,
    retirement: CredentialBrokerLeaseRetirement,
    cause: Error,
  ): Promise<never> {
    lease.credential.fill(0)
    try {
      await this.#retire(retirement, 'revoke-and-wait')
    } catch (cleanupCause) {
      throw new AggregateError(
        [cause, cleanupCause],
        'credential broker rejected a lease without settling remote ownership',
        { cause: cleanupCause },
      )
    }
    throw cause
  }

  async #recoverAmbiguousAcquisition(acquisition: CredentialBrokerAcquisition): Promise<void> {
    if (this.#ambiguousAcquisitions.get(acquisition.requestId) !== acquisition) return
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
    const response = await this.#exchange(
      acquisition.request,
      acquisition.scope,
      signal,
    )
    let lease: AuthenticatedCredentialLease
    try {
      lease = authenticateLeaseResponse(
        response,
        publicKey,
        this.#options.secretStore,
      )
    } finally {
      response.fill(0)
    }
    const bindingFailure =
      lease.requestId !== acquisition.requestId ||
      lease.releaseRequestId !== acquisition.releaseRequestId ||
      lease.revokeRequestId !== acquisition.revokeRequestId ||
      !sameNetworkMatrixSampleAuthority(
        lease.controlAuthority.sampleAuthority,
        acquisition.scope.sampleAuthority,
      ) ||
      lease.probeNonce !== acquisition.scope.probeNonce
    const retirement = this.#registerRetirement(
      lease,
      publicKey,
      acquisition.controllerOrigin,
    )
    lease.credential.fill(0)
    await this.#retire(retirement, 'revoke-and-wait')
    if (bindingFailure) {
      throw new Error('credential broker replay crossed its exact acquisition scope')
    }
    this.#ambiguousAcquisitions.delete(acquisition.requestId)
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

  async #exchange(
    request: Readonly<Record<string, unknown>>,
    scope: CredentialBrokerScope,
    signal: AbortSignal,
    onDispatch?: () => void,
  ): Promise<Buffer> {
    requireActive(signal)
    const workloadIdentity = await this.#options.workloadIdentity.issue({
      runId: scope.sampleAuthority.runId,
      profileId: scope.sampleAuthority.profileId,
      probeNonce: scope.probeNonce,
      signal,
    })
    if (!(workloadIdentity instanceof Uint8Array) || workloadIdentity.byteLength === 0) {
      if (workloadIdentity instanceof Uint8Array) workloadIdentity.fill(0)
      throw new Error('credential broker workload identity authority is invalid')
    }
    let stdin: Buffer
    try {
      requireActive(signal)
      stdin = encodeBrokerPipeFrame({
        ...request,
        workloadIdentity: this.#options.workloadIdentity.binding,
        workloadIdentityByteLength: workloadIdentity.byteLength,
      }, workloadIdentity)
    } finally {
      workloadIdentity.fill(0)
    }
    const operationId = `credential-broker-${randomBytes(24).toString('hex')}`
    try {
      const pipeExchange = this.#options.pipeExchange
      if (pipeExchange !== undefined) {
        return exchangeWithCredentialBrokerPipe(
          pipeExchange,
          operationId,
          stdin,
          signal,
          onDispatch,
        )
      }
      const stdout = new BoundedBrokerCapture()
      const stderr = new BoundedBrokerCapture()
      try {
        onDispatch?.()
        const execution = await this.#execute(operationId, stdin, stdout, stderr, signal)
        if (
          execution.timedOut || execution.processEvidence.terminal !== 'exited' ||
          execution.processEvidence.exitCode !== 0 || stderr.byteLength !== 0
        ) throw new Error('credential broker process did not publish an authenticated response')
        const response = stdout.take()
        stderr.erase()
        if (signal.aborted) {
          response.fill(0)
          requireActive(signal)
        }
        return response
      } catch (cause) {
        stdout.erase()
        stderr.erase()
        throw cause
      }
    } finally {
      stdin.fill(0)
    }
  }

  #execute(
    operationId: string,
    stdin: Uint8Array,
    stdout: BoundedBrokerCapture,
    stderr: BoundedBrokerCapture,
    signal: AbortSignal,
  ): Promise<BrowserSampleContainmentExecution> {
    const command = Object.freeze({
      executable: this.#options.helperPath,
      arguments: Object.freeze([]),
      cwd: this.#options.workingDirectory,
      stdin,
    })
    const trace = (event: {
      readonly milestone: string
      readonly context?: Readonly<Record<string, unknown>>
    }): void => {
      this.#options.trace?.(Object.freeze({
        operationId,
        milestone: event.milestone,
        ...(event.context === undefined ? {} : { context: event.context }),
      }))
    }
    if (this.#options.platform === 'linux') {
      return executeNativeProcessGroupCommand({
        command,
        environment: inheritedSampleEnvironment(),
        deadlineMs: BROKER_OPERATION_DEADLINE_MS,
        terminationGraceMs: BROKER_TERMINATION_GRACE_MS,
        terminationSignal: signal,
        stdout: (chunk) => stdout.consume(chunk),
        stderr: (chunk) => stderr.consume(chunk),
        trace: (event) => trace(event),
      })
    }
    return executeWindowsJob({
      helperPath: this.#options.windowsJobHelperPath as string,
      operationId,
      command,
      inheritedEnvironment: inheritedSampleEnvironment(),
      injectedEnvironment: Object.freeze({}),
      deadlineMs: BROKER_OPERATION_DEADLINE_MS,
      terminationGraceMs: BROKER_TERMINATION_GRACE_MS,
      stdout: (chunk) => stdout.consume(chunk),
      stderr: (chunk) => stderr.consume(chunk),
    })
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
    dispatched: false,
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

async function exchangeWithCredentialBrokerPipe(
  pipeExchange: CredentialBrokerPipeExchange,
  operationId: string,
  stdin: Buffer,
  signal: AbortSignal,
  onDispatch: (() => void) | undefined,
): Promise<Buffer> {
  onDispatch?.()
  const output = await pipeExchange.exchange({ operationId, stdin, signal })
  if (!(output instanceof Uint8Array) || output.byteLength > MAXIMUM_BROKER_FRAME_BYTES) {
    if (output instanceof Uint8Array) output.fill(0)
    throw new Error('credential broker response exceeded its pipe authority')
  }
  if (arraysOverlap(stdin, output)) {
    output.fill(0)
    throw new Error('credential broker pipe crossed request and response ownership')
  }
  const response = Buffer.from(output)
  output.fill(0)
  if (signal.aborted) {
    response.fill(0)
    requireActive(signal)
  }
  return response
}

class BoundedBrokerCapture {
  readonly #chunks: Buffer[] = []
  #byteLength = 0

  get byteLength(): number { return this.#byteLength }

  consume(chunk: Uint8Array): void {
    try {
      this.#byteLength += chunk.byteLength
      if (this.#byteLength > MAXIMUM_BROKER_FRAME_BYTES) {
        throw new Error('credential broker response exceeded its pipe authority')
      }
      this.#chunks.push(Buffer.from(chunk))
    } finally {
      // The containment adapter transfers each raw secret-bearing chunk to this
      // sink. Erase that source allocation even when capture rejects it.
      chunk.fill(0)
    }
  }

  take(): Buffer {
    const result = Buffer.concat(this.#chunks, this.#byteLength)
    for (const chunk of this.#chunks) chunk.fill(0)
    this.#chunks.length = 0
    this.#byteLength = 0
    return result
  }

  erase(): void {
    for (const chunk of this.#chunks) chunk.fill(0)
    this.#chunks.length = 0
    this.#byteLength = 0
  }
}

function authenticateLeaseResponse(
  bytes: Buffer,
  publicKey: string,
  secretStore: CredentialBrokerSecretStore | undefined,
): AuthenticatedCredentialLease {
  const frame = parseBrokerPipeFrame(bytes)
  const value = frame.metadata
  const signedLease = parseSignedExternalFixtureControlCredentialLease(value)
  const lease = authenticateSignedExternalFixtureControlCredentialLease(signedLease, publicKey)
  if (
    lease.credentialByteLength !== frame.payload.byteLength ||
    !isCredentialBytes(frame.payload)
  ) invalidBrokerResponse()
  const credential = secretStore?.adopt(frame.payload) ?? Buffer.from(frame.payload)
  if (
    !(credential instanceof Uint8Array) || arraysOverlap(credential, frame.payload) ||
    credential.byteLength !== frame.payload.byteLength ||
    !sameBytes(credential, frame.payload)
  ) {
    if (credential instanceof Uint8Array) credential.fill(0)
    invalidBrokerResponse()
  }
  return Object.freeze({ ...lease, signedLease, credential })
}

function authenticateRetirementResponse(
  bytes: Buffer,
  publicKey: string,
  operation: CredentialRetirementPayload['operation'],
  requestId: string,
  retirement: CredentialBrokerLeaseRetirement,
): ExternalFixtureControlCredentialRetirementReceipt {
  const frame = parseBrokerPipeFrame(bytes)
  if (frame.payload.byteLength !== 0) invalidBrokerResponse()
  const signedReceipt = parseSignedExternalFixtureControlCredentialReceipt(frame.metadata)
  const receipt = authenticateSignedExternalFixtureControlCredentialReceipt(signedReceipt, publicKey)
  if (
    receipt.operation !== operation || receipt.requestId !== requestId ||
    receipt.releaseRequestId !== retirement.release.request.requestId ||
    receipt.revokeRequestId !== retirement.revoke.request.requestId ||
    !sameNetworkMatrixControlAuthority(receipt.controlAuthority, retirement.controlAuthority) ||
    !sameNetworkMatrixSampleAuthority(
      receipt.controlAuthority.sampleAuthority,
      retirement.scope.sampleAuthority,
    ) ||
    receipt.probeNonce !== retirement.scope.probeNonce ||
    receipt.authorityInstanceId !== retirement.authorityInstanceId ||
    receipt.attestationSha256 !== retirement.attestationSha256 ||
    receipt.leaseExpiresAt !== retirement.leaseExpiresAt
  ) invalidBrokerResponse()
  return Object.freeze({ receipt, signedReceipt })
}

function encodeBrokerPipeFrame(
  metadata: Readonly<Record<string, unknown>>,
  payload: Uint8Array,
): Buffer {
  const metadataBytes = Buffer.from(`${JSON.stringify(metadata)}\n`, 'utf8')
  const total = BROKER_METADATA_LENGTH_BYTES + metadataBytes.byteLength + payload.byteLength
  if (metadataBytes.byteLength === 0 || total > MAXIMUM_BROKER_FRAME_BYTES) {
    throw new Error('credential broker request exceeded its anonymous pipe authority')
  }
  const frame = Buffer.alloc(total)
  frame.writeUInt32BE(metadataBytes.byteLength, 0)
  metadataBytes.copy(frame, BROKER_METADATA_LENGTH_BYTES)
  frame.set(payload, BROKER_METADATA_LENGTH_BYTES + metadataBytes.byteLength)
  return frame
}

function parseBrokerPipeFrame(bytes: Buffer): {
  readonly metadata: unknown
  readonly payload: Uint8Array
} {
  if (bytes.byteLength <= BROKER_METADATA_LENGTH_BYTES) invalidBrokerResponse()
  const metadataByteLength = bytes.readUInt32BE(0)
  const payloadOffset = BROKER_METADATA_LENGTH_BYTES + metadataByteLength
  if (metadataByteLength === 0 || payloadOffset > bytes.byteLength) invalidBrokerResponse()
  let encoded: string
  try {
    encoded = new TextDecoder('utf-8', { fatal: true }).decode(
      bytes.subarray(BROKER_METADATA_LENGTH_BYTES, payloadOffset),
    )
  } catch {
    invalidBrokerResponse()
  }
  if (!encoded.endsWith('\n') || encoded.slice(0, -1).includes('\n') || encoded.includes('\r')) {
    invalidBrokerResponse()
  }
  let value: unknown
  try {
    value = JSON.parse(encoded)
  } catch {
    invalidBrokerResponse()
  }
  if (encoded !== `${JSON.stringify(value)}\n`) invalidBrokerResponse()
  return Object.freeze({ metadata: value, payload: bytes.subarray(payloadOffset) })
}

function isCredentialBytes(value: Uint8Array): boolean {
  if (
    value.byteLength < MINIMUM_CREDENTIAL_BYTES ||
    value.byteLength > MAXIMUM_CREDENTIAL_BYTES
  ) return false
  for (const byte of value) {
    const alphaNumeric = byte >= 0x30 && byte <= 0x39 ||
      byte >= 0x41 && byte <= 0x5a || byte >= 0x61 && byte <= 0x7a
    if (!alphaNumeric && byte !== 0x2d && byte !== 0x5f) return false
  }
  return true
}

function sameBytes(left: Uint8Array, right: Uint8Array): boolean {
  if (left.byteLength !== right.byteLength) return false
  for (let index = 0; index < left.byteLength; index += 1) {
    if (left[index] !== right[index]) return false
  }
  return true
}

function arraysOverlap(left: Uint8Array, right: Uint8Array): boolean {
  if (left.buffer !== right.buffer) return false
  const leftEnd = left.byteOffset + left.byteLength
  const rightEnd = right.byteOffset + right.byteLength
  return left.byteOffset < rightEnd && right.byteOffset < leftEnd
}

async function readPinnedPublicKey(path: string, signal: AbortSignal): Promise<string> {
  const bytes = await readFile(path, { signal })
  if (bytes.byteLength === 0 || bytes.byteLength > MAXIMUM_BROKER_FRAME_BYTES) {
    throw new Error('credential broker public key authority is invalid')
  }
  return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
}

function fixtureForProfile(config: NetworkMatrixExternalFixtureConfig, profileId: NetworkMatrixProfileId) {
  return {
    'scheduled-public-stun': config.publicStun,
    'scheduled-restricted-udp': config.restrictedUdp,
    'scheduled-coturn': config.coturn,
    'manual-real-nat': config.manualRealNat,
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

function credentialRetirementRegistryKey(
  lease: ExternalFixtureControlCredentialLeasePayload,
  publicKey: string,
  controllerOrigin: string,
): string {
  // Only authenticated public lease metadata participates. The raw credential
  // remains outside every digest and cannot become a local comparison oracle.
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

function validWorkloadIdentityBinding(value: unknown): value is ParentWorkloadIdentityBinding {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false
  const binding = value as Record<string, unknown>
  const keys = [
    'protocolVersion', 'kind', 'audience', 'issuer', 'repository', 'ref',
    'workflowRef', 'requestOrigin', 'requestPath', 'requestQuery',
  ]
  if (
    Object.keys(binding).length !== keys.length ||
    Object.keys(binding).some((key, index) => key !== keys[index]) ||
    binding.protocolVersion !== PARENT_WORKLOAD_IDENTITY_PROTOCOL ||
    binding.kind !== 'github-actions-oidc' ||
    typeof binding.audience !== 'string' || binding.audience.length < 8 ||
    typeof binding.issuer !== 'string' || typeof binding.repository !== 'string' ||
    typeof binding.ref !== 'string' || typeof binding.workflowRef !== 'string' ||
    typeof binding.requestOrigin !== 'string' || typeof binding.requestPath !== 'string' ||
    typeof binding.requestQuery !== 'string'
  ) return false
  try {
    const issuer = new URL(binding.issuer)
    const requestOrigin = new URL(binding.requestOrigin)
    return issuer.protocol === 'https:' && issuer.origin === binding.issuer &&
      requestOrigin.protocol === 'https:' && requestOrigin.origin === binding.requestOrigin &&
      binding.requestPath.startsWith('/') && binding.requestQuery.startsWith('?')
  } catch {
    return false
  }
}

function exactClosedReceipt(value: unknown): value is { readonly terminal: 'closed' } {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false
  const receipt = value as Record<string, unknown>
  return Object.keys(receipt).length === 1 && Object.keys(receipt)[0] === 'terminal' &&
    receipt.terminal === 'closed'
}

function canonicalAbsolutePath(value: unknown): value is string {
  return typeof value === 'string' && isAbsolute(value) && resolve(value) === value
}

function requireActive(signal: AbortSignal): void {
  if (signal.aborted) throw new Error('credential broker operation was terminated')
}

function invalidBrokerResponse(): never {
  throw new Error('credential broker response is not canonical or authenticated')
}
