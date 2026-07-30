import type { BrowserSampleContainmentBackend } from '../../browser-evidence/process/containment.ts'
import { ContainedPlaywrightNetworkMatrixSampleExecutor } from '../contained-playwright.ts'
import type {
  NetworkMatrixExecutionRuntime,
  NetworkMatrixRuntimeBootstrap,
  NetworkMatrixRuntimeBootstrapContext,
} from '../cli/execute.ts'
import type { NetworkMatrixOwnedOperation } from '../owned-operation.ts'
import {
  InjectedNetworkMatrixAuthorityResolver,
} from '../runtime-authority.ts'
import type { NetworkMatrixSampleExecutionContext } from '../runner.ts'
import type { NetworkMatrixProfileId } from '../vocabulary.ts'
import {
  parseNetworkMatrixExternalFixtureConfig,
  runtimeInputsFromExternalFixtureConfig,
  type ExternalFixtureTrustConfig,
  type NetworkMatrixExternalFixtureConfig,
} from './concrete-runtime-config.ts'
import {
  ConcreteContainedBrowserProcessBroker,
  type ConcreteContainedBrowserProcessBrokerOptions,
} from './contained-browser-broker.ts'
import {
  FilesystemContainedBrowserSampleInputAuthorityFactory,
  type ContainedBrowserPionControlFiles,
  type ContainedBrowserTopologyFiles,
} from './contained-browser-input.ts'
import { FilesystemExternalFixtureTrustInspector } from './remote-pion-probe-authority.ts'
import type { ManualOperatorTopologyIdentity } from './external-fixture-attestation.ts'
import type { ExternalFixtureControlCredentialAuthority } from './control-credential.ts'
import { REMOTE_PION_MAXIMUM_ATTEMPT_LEASE_MS } from './remote-pion.ts'

export interface ExternalFixtureNetworkMatrixRuntimeBootstrapOptions {
  readonly config: NetworkMatrixExternalFixtureConfig
  readonly controlCredentials: ExternalFixtureControlCredentialAuthority
  readonly containment: BrowserSampleContainmentBackend
  readonly checkoutSha: string
  readonly topologyFiles: (
    context: NetworkMatrixSampleExecutionContext,
  ) => ContainedBrowserTopologyFiles
  readonly nodeExecutable: string
  readonly repositoryRoot: string
  readonly processDeadlineMs: number
  readonly terminationGraceMs: number
  readonly attemptLeaseMs: number
  readonly resultPollIntervalMs: number
  readonly resultDeadlineMs: number
  readonly challengeDeadlineMs: number
  readonly cleanupDeadlineMs: number
  readonly temporaryRoot?: string
  readonly maximumCaptureBytes?: number
  readonly childPath?: string
  readonly trace?: ConcreteContainedBrowserProcessBrokerOptions['trace']
  readonly manualOperatorIdentity?: ManualOperatorTopologyIdentity
}

/**
 * Construction is deliberately resource-free. Profile preparation validates only
 * local trust material; each real sample owns the credentialed live probe and
 * remote attempt, so no profile-level step fabricates a sample authority.
 */
export function createExternalFixtureNetworkMatrixRuntimeBootstrap(
  options: ExternalFixtureNetworkMatrixRuntimeBootstrapOptions,
): NetworkMatrixRuntimeBootstrap {
  const config = parseNetworkMatrixExternalFixtureConfig(options.config)
  if (
    !Number.isSafeInteger(options.attemptLeaseMs) || options.attemptLeaseMs < 1 ||
    options.attemptLeaseMs > REMOTE_PION_MAXIMUM_ATTEMPT_LEASE_MS
  ) throw new Error('external fixture attempt lease exceeds the protocol policy')
  if (config.manualRealNat !== null && options.manualOperatorIdentity === undefined) {
    throw new Error('manual real-NAT runtime requires a separate local operator topology identity')
  }
  return Object.freeze({
    bootstrap: (
      bootstrapContext: NetworkMatrixRuntimeBootstrapContext,
    ): NetworkMatrixOwnedOperation<NetworkMatrixExecutionRuntime> => {
      const runtimeInputs = runtimeInputsFromExternalFixtureConfig(config)
      const authorities = new InjectedNetworkMatrixAuthorityResolver({
        inputs: runtimeInputs,
        externalFixtureTrust: new FilesystemExternalFixtureTrustInspector({
          config,
        }),
      })
      const inputs = new FilesystemContainedBrowserSampleInputAuthorityFactory({
        checkoutSha: options.checkoutSha,
        ...(options.temporaryRoot === undefined ? {} : { temporaryRoot: options.temporaryRoot }),
        topologyFiles: options.topologyFiles,
        controlFiles: (context, signal) => controlFiles(
          config,
          options.manualOperatorIdentity,
          context,
          signal,
        ),
        controlCredentials: options.controlCredentials,
        attemptLeaseMs: options.attemptLeaseMs,
        resultPollIntervalMs: options.resultPollIntervalMs,
        resultDeadlineMs: options.resultDeadlineMs,
        challengeDeadlineMs: options.challengeDeadlineMs,
        cleanupDeadlineMs: options.cleanupDeadlineMs,
      })
      const broker = new ConcreteContainedBrowserProcessBroker({
        containment: options.containment,
        inputs,
        nodeExecutable: options.nodeExecutable,
        repositoryRoot: options.repositoryRoot,
        processDeadlineMs: options.processDeadlineMs,
        terminationGraceMs: options.terminationGraceMs,
        ...(options.maximumCaptureBytes === undefined
          ? {}
          : { maximumCaptureBytes: options.maximumCaptureBytes }),
        ...(options.childPath === undefined ? {} : { childPath: options.childPath }),
        ...(options.trace === undefined ? {} : { trace: options.trace }),
      })
      const settlement = createRuntimeSettlement(options.controlCredentials)
      const runtime: NetworkMatrixExecutionRuntime = Object.freeze({
        authorities: Object.freeze({
          prepare: (context: Parameters<typeof authorities.prepare>[0]) => {
            if (context.runId !== bootstrapContext.runId) {
              throw new Error('external fixture authority crossed its bootstrapped run')
            }
            return authorities.prepare(context)
          },
        }),
        samples: (() => {
          const executor = new ContainedPlaywrightNetworkMatrixSampleExecutor(broker)
          return Object.freeze({
            execute: (context: NetworkMatrixSampleExecutionContext) => {
              if (context.runId !== bootstrapContext.runId) {
                throw new Error('external fixture sample crossed its bootstrapped run')
              }
              return executor.execute(context)
            },
          })
        })(),
        closeAndWait: settlement.closeAndWait,
        forceTerminateAndWait: settlement.forceTerminateAndWait,
      })
      return Object.freeze({
        result: Promise.resolve(runtime),
        forceTerminateAndWait: async (): Promise<void> => {
          await settlement.forceTerminateAndWait()
        },
      })
    },
  })
}

function createRuntimeSettlement(
  credentials: ExternalFixtureControlCredentialAuthority,
): {
  readonly closeAndWait: () => Promise<{ readonly terminal: 'closed' }>
  readonly forceTerminateAndWait: () => Promise<{ readonly terminal: 'closed' }>
} {
  let terminalReceipt: { readonly terminal: 'closed' } | undefined
  let closeOperation: Promise<{ readonly terminal: 'closed' }> | undefined
  let forceOperation: Promise<{ readonly terminal: 'closed' }> | undefined
  const settle = (force: boolean): Promise<{ readonly terminal: 'closed' }> => {
    if (terminalReceipt !== undefined) return Promise.resolve(terminalReceipt)
    if (!force && forceOperation !== undefined) return forceOperation
    const existing = force ? forceOperation : closeOperation
    if (existing !== undefined) return existing
    const gracefulOrForced = Promise.resolve().then(async () => {
      if (terminalReceipt !== undefined) return terminalReceipt
      const receipt = force
        ? await credentials.forceTerminateAndWait()
        : await credentials.closeAndWait()
        if (!exactClosedReceipt(receipt)) {
          throw new Error('external fixture credential authority did not settle runtime ownership')
        }
        terminalReceipt ??= Object.freeze({ terminal: 'closed' })
        return terminalReceipt
      })
    const workerToJoin = force ? closeOperation : undefined
    const settlement = gracefulOrForced.then(async (receipt) => {
      await workerToJoin?.catch(() => undefined)
      return receipt
    })
    const retryable = settlement.catch((cause: unknown) => {
      if (force) {
        if (forceOperation === retryable) forceOperation = undefined
      } else if (closeOperation === retryable) closeOperation = undefined
      throw cause
    })
    if (force) forceOperation = retryable
    else closeOperation = retryable
    return retryable
  }
  return Object.freeze({
    closeAndWait: () => settle(false),
    forceTerminateAndWait: () => settle(true),
  })
}

function exactClosedReceipt(value: unknown): value is { readonly terminal: 'closed' } {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false
  const receipt = value as Record<string, unknown>
  const keys = Object.keys(receipt)
  return keys.length === 1 && keys[0] === 'terminal' && receipt.terminal === 'closed'
}

async function controlFiles(
  config: NetworkMatrixExternalFixtureConfig,
  manualOperatorIdentity: ManualOperatorTopologyIdentity | undefined,
  context: NetworkMatrixSampleExecutionContext,
  signal: AbortSignal,
): Promise<ContainedBrowserPionControlFiles> {
  if (signal.aborted) throw new Error('external fixture control acquisition was terminated')
  const profileId = context.identity.profileId
  const fixture = fixtureForProfile(config, profileId)
  if (fixture === null) {
    throw new Error('sample requested an external fixture that was not provisioned')
  }
  return Object.freeze({
    controllerOrigin: fixture.control.controllerOrigin,
    tlsCertificateSha256: fixture.control.tlsCertificateSha256,
    tlsCertificateAuthorityFile: fixture.control.tlsCertificateAuthorityFile,
    attestationPublicKeyFile: fixture.control.attestationPublicKeyFile,
    manualOperatorIdentity: profileId === 'manual-real-nat'
      ? manualOperatorIdentity ?? null
      : null,
  })
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
