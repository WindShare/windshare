import { isAbsolute, resolve } from 'node:path'

import type { NetworkMatrixSampleAuthority } from '../../sample-authority.ts'
import type { LinuxTopologyTraceChannel } from '../trace/index.ts'
import type {
  ExternalFixtureControlCredentialAuthority,
  ExternalFixtureControlCredentialAuthorityReceipt,
  ExternalFixtureControlCredentialLease,
} from '../control-credential.ts'
import {
  PARENT_WORKLOAD_IDENTITY_PROTOCOL,
  type ParentWorkloadIdentityBinding,
} from '../parent-workload-identity.ts'
import { CredentialAcquisitionRegistry } from './acquisition.ts'
import type {
  CredentialBrokerOptions,
  CredentialBrokerTestHarnessOptions,
  InternalCredentialBrokerOptions,
} from './contracts.ts'
import { CredentialBrokerProcessOwner } from './process-owner.ts'
import { CredentialRetirementRegistry } from './retirement.ts'

/**
 * The authority coordinates ownership fences while delegated modules retain the
 * mutable state for exactly one credential lifecycle phase.
 */
export class ProcessExternalFixtureControlCredentialAuthority
implements ExternalFixtureControlCredentialAuthority {
  readonly #owner: CredentialBrokerProcessOwner
  readonly #acquisitions: CredentialAcquisitionRegistry
  readonly #retirements: CredentialRetirementRegistry

  readonly traces: LinuxTopologyTraceChannel

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
      processOwner: options.processOwner,
      config: options.config,
      workloadIdentity: options.workloadIdentity,
    })
  }

  static createTestHarness(
    options: CredentialBrokerTestHarnessOptions,
  ): ProcessExternalFixtureControlCredentialAuthority {
    return new ProcessExternalFixtureControlCredentialAuthority(options)
  }

  private constructor(options: InternalCredentialBrokerOptions) {
    validateComposition(options)
    const composition = Object.freeze({ ...options })
    this.#owner = new CredentialBrokerProcessOwner(composition)
    this.traces = this.#owner.traces
    this.#retirements = new CredentialRetirementRegistry(this.#owner.exchange)
    this.#acquisitions = new CredentialAcquisitionRegistry({
      config: composition.config,
      secretStore: composition.secretStore,
      exchange: this.#owner.exchange,
      retirements: this.#retirements,
    })
  }

  acquire(input: {
    readonly sampleAuthority: NetworkMatrixSampleAuthority
    readonly probeNonce: string
    readonly signal: AbortSignal
  }): Promise<ExternalFixtureControlCredentialLease> {
    return this.#acquisitions.acquire(input)
  }

  closeAndWait(): Promise<ExternalFixtureControlCredentialAuthorityReceipt> {
    this.#acquisitions.stopAdmission()
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
    this.#acquisitions.stopAdmission()
    if (this.#terminalReceipt !== undefined) return Promise.resolve(this.#terminalReceipt)
    if (this.#forceOperation !== undefined) return this.#forceOperation
    const gracefulToJoin = this.#closeOperation
    const operation = this.#forceClose().then(async (receipt) => {
      // Forced settlement subsumes graceful failure, but the earlier identity
      // worker must still terminate before ownership can be published as closed.
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
    await this.#acquisitions.awaitPending()
    if (this.#acquisitions.hasAmbiguous || this.#retirements.hasActive) {
      throw new Error('external fixture credential broker still owns active leases')
    }
    return this.#closeIdentity(false)
  }

  async #forceClose(): Promise<ExternalFixtureControlCredentialAuthorityReceipt> {
    await this.#acquisitions.awaitPending()
    const failures = [
      ...await this.#acquisitions.recoverAllAmbiguous(),
      ...await this.#retirements.revokeAll(),
    ]
    if (
      failures.length !== 0 || this.#acquisitions.hasAmbiguous ||
      this.#retirements.hasActive
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
    await this.#owner.closeIdentity(force)
    return this.#publishTerminalReceipt()
  }

  #publishTerminalReceipt(): ExternalFixtureControlCredentialAuthorityReceipt {
    if (this.#terminalReceipt !== undefined) return this.#terminalReceipt
    this.#terminalReceipt = Object.freeze({ terminal: 'closed' })
    return this.#terminalReceipt
  }
}

function validateComposition(options: InternalCredentialBrokerOptions): void {
  if (
    !canonicalAbsolutePath(options.helperPath) ||
    !canonicalAbsolutePath(options.workingDirectory) ||
    options.platform !== 'linux' && options.platform !== 'win32' ||
    !validProcessOwner(options.processOwner) ||
    typeof options.workloadIdentity?.issue !== 'function' ||
    typeof options.workloadIdentity?.closeAndWait !== 'function' ||
    typeof options.workloadIdentity?.forceTerminateAndWait !== 'function' ||
    !validWorkloadIdentityBinding(options.workloadIdentity?.binding) ||
    options.pipeExchange !== undefined && typeof options.pipeExchange.exchange !== 'function' ||
    options.secretStore !== undefined && typeof options.secretStore.adopt !== 'function'
  ) throw new Error('external fixture credential broker composition is invalid')
}

function validProcessOwner(value: unknown): boolean {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false
  const owner = value as Record<string, unknown>
  return canonicalAbsolutePath(owner.path) && Number.isSafeInteger(owner.byteLength) &&
    (owner.byteLength as number) > 0 && typeof owner.sha256 === 'string' &&
    /^[0-9a-f]{64}$/u.test(owner.sha256)
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

function canonicalAbsolutePath(value: unknown): value is string {
  return typeof value === 'string' && isAbsolute(value) && resolve(value) === value
}
