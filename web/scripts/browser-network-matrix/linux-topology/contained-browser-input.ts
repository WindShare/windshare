import { mkdtemp, mkdir, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import type { NetworkMatrixSampleExecutionContext } from '../runner.ts'
import type { NetworkMatrixOwnedOperation } from '../owned-operation.ts'
import {
  NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA,
  newNetworkMatrixProcessInstanceId,
  type NetworkMatrixSampleAuthority,
} from '../sample-authority.ts'
import type { ManualOperatorTopologyIdentity } from './external-fixture-attestation.ts'
import {
  eraseExternalFixtureControlCredential,
  newExternalFixtureProbeNonce,
  retireExternalFixtureControlCredential,
  validateExternalFixtureControlCredential,
  type ExternalFixtureControlCredentialAuthority,
  type ExternalFixtureControlCredentialLease,
} from './control-credential.ts'
import {
  CONTAINED_BROWSER_SAMPLE_SECRET_SCHEMA,
  encodeContainedBrowserSampleSecretFrame,
} from './contained-browser-sample.ts'
import type {
  ContainedBrowserSampleInputAuthority,
  ContainedBrowserSampleInputAuthorityFactory,
} from './contained-browser-broker.ts'

const MAXIMUM_AUTHORITY_FILE_BYTES = 1_048_576

export interface ContainedBrowserTopologyFiles {
  readonly topologyProfilePath: string
  readonly topologyProfileSha256: string
  readonly topologyResolutionPath: string
  readonly topologyResolutionSha256: string
}

export interface ContainedBrowserPionControlFiles {
  readonly controllerOrigin: string
  readonly tlsCertificateSha256: string
  readonly tlsCertificateAuthorityFile: string
  readonly attestationPublicKeyFile: string
  readonly manualOperatorIdentity: ManualOperatorTopologyIdentity | null
}

export interface FilesystemContainedBrowserSampleInputOptions {
  readonly checkoutSha: string
  readonly temporaryRoot?: string
  readonly topologyFiles: (
    context: NetworkMatrixSampleExecutionContext,
  ) => ContainedBrowserTopologyFiles
  readonly controlFiles: (
    context: NetworkMatrixSampleExecutionContext,
    signal: AbortSignal,
  ) => Promise<ContainedBrowserPionControlFiles>
  readonly controlCredentials: ExternalFixtureControlCredentialAuthority
  readonly attemptLeaseMs: number
  readonly resultPollIntervalMs: number
  readonly resultDeadlineMs: number
  readonly challengeDeadlineMs: number
  readonly cleanupDeadlineMs: number
  readonly probeNonce?: () => string
}

interface InputAcquisitionState {
  readonly controller: AbortController
  directory: string | undefined
  cleanup: Promise<void> | undefined
  result: Promise<ContainedBrowserSampleInputAuthority> | undefined
  secretFrame: Uint8Array | undefined
  credentialOffset: number | undefined
  credentialByteLength: number | undefined
  credentialLease: ExternalFixtureControlCredentialLease | undefined
}

export class FilesystemContainedBrowserSampleInputAuthorityFactory
implements ContainedBrowserSampleInputAuthorityFactory {
  readonly #options: FilesystemContainedBrowserSampleInputOptions
  readonly #probeNonce: () => string
  readonly #observedCredentialLeaseIds = new Set<string>()

  constructor(options: FilesystemContainedBrowserSampleInputOptions) {
    this.#options = options
    this.#probeNonce = options.probeNonce ?? newExternalFixtureProbeNonce
  }

  acquire(
    context: NetworkMatrixSampleExecutionContext,
    outerSignal: AbortSignal,
  ): NetworkMatrixOwnedOperation<ContainedBrowserSampleInputAuthority> {
    const state: InputAcquisitionState = {
      controller: new AbortController(),
      directory: undefined,
      cleanup: undefined,
      result: undefined,
      secretFrame: undefined,
      credentialOffset: undefined,
      credentialByteLength: undefined,
      credentialLease: undefined,
    }
    const abort = (): void => state.controller.abort()
    outerSignal.addEventListener('abort', abort, { once: true })
    if (outerSignal.aborted) state.controller.abort()
    const result = this.#acquire(context, state).finally(() => {
      outerSignal.removeEventListener('abort', abort)
    })
    state.result = result
    return Object.freeze({
      result,
      forceTerminateAndWait: async (): Promise<void> => {
        state.controller.abort()
        await result.catch(() => undefined)
        await cleanupInputState(state)
      },
    })
  }

  async #acquire(
    context: NetworkMatrixSampleExecutionContext,
    state: InputAcquisitionState,
  ): Promise<ContainedBrowserSampleInputAuthority> {
    try {
      requireActive(state.controller.signal)
      const topology = this.#options.topologyFiles(context)
      const control = await this.#options.controlFiles(context, state.controller.signal)
      requireActive(state.controller.signal)
      const [certificate, attestationPublicKey] = await Promise.all([
        boundedAuthorityFile(control.tlsCertificateAuthorityFile, state.controller.signal),
        boundedAuthorityFile(control.attestationPublicKeyFile, state.controller.signal),
      ])
      const directory = await mkdtemp(join(this.#options.temporaryRoot ?? tmpdir(), 'windshare-network-sample-'))
      state.directory = directory
      const sampleDirectory = join(directory, 'sample')
      const childAttachmentStagingRoot = join(directory, 'staging')
      await Promise.all([
        mkdir(sampleDirectory, { mode: 0o700 }),
        mkdir(childAttachmentStagingRoot, { mode: 0o700 }),
      ])
      const probeNonce = this.#probeNonce()
      const sampleAuthority: NetworkMatrixSampleAuthority = Object.freeze({
        schemaVersion: NETWORK_MATRIX_SAMPLE_AUTHORITY_SCHEMA,
        runId: context.runId,
        profileId: context.identity.profileId,
        browser: context.identity.browser,
        sampleOrdinal: context.identity.sampleOrdinal,
        processInstanceId: newNetworkMatrixProcessInstanceId(),
        operationId: context.operationId,
      })
      const credentialLease = await this.#options.controlCredentials.acquire({
        sampleAuthority,
        probeNonce,
        signal: state.controller.signal,
      })
      const controlLeaseId = credentialLease.controlAuthority.controlLeaseId
      if (this.#observedCredentialLeaseIds.has(controlLeaseId)) {
        await retireExternalFixtureControlCredential(credentialLease, true)
        throw new Error('contained browser control credential lease was reused')
      }
      this.#observedCredentialLeaseIds.add(controlLeaseId)
      state.credentialLease = credentialLease
      try {
        const now = Date.now()
        validateExternalFixtureControlCredential(credentialLease, {
          sampleAuthority,
          probeNonce,
          now,
        })
        if (Date.parse(credentialLease.expiresAt) <= now + this.#options.attemptLeaseMs) {
          throw new Error('contained browser control credential lease is too short for its attempt')
        }
        const framed = encodeContainedBrowserSampleSecretFrame({
          schemaVersion: CONTAINED_BROWSER_SAMPLE_SECRET_SCHEMA,
          expectedConnectivity: context.profile.connectivityExpectation === 'connectivity-established'
            ? 'established'
            : 'blocked',
          control: {
            controllerOrigin: control.controllerOrigin,
            controlLease: {
              controlAuthority: credentialLease.controlAuthority,
              probeNonce: credentialLease.probeNonce,
              authorityInstanceId: credentialLease.authorityInstanceId,
              attestationSha256: credentialLease.attestationSha256,
              issuedAt: credentialLease.issuedAt,
              expiresAt: credentialLease.expiresAt,
              maxAttempts: credentialLease.maxAttempts,
            },
            tlsCertificateAuthority: certificate,
            tlsCertificateSha256: control.tlsCertificateSha256,
            attestationPublicKey,
            manualOperatorIdentity: control.manualOperatorIdentity,
          },
          attemptLeaseMs: this.#options.attemptLeaseMs,
          resultPollIntervalMs: this.#options.resultPollIntervalMs,
          resultDeadlineMs: this.#options.resultDeadlineMs,
          challengeDeadlineMs: this.#options.challengeDeadlineMs,
          cleanupDeadlineMs: this.#options.cleanupDeadlineMs,
        }, credentialLease.credential)
        state.secretFrame = framed.bytes
        state.credentialOffset = framed.credentialOffset
        state.credentialByteLength = framed.credentialByteLength
      } finally {
        eraseExternalFixtureControlCredential(credentialLease)
      }
      requireActive(state.controller.signal)
      const secretFrame = state.secretFrame
      if (secretFrame === undefined) throw new Error('contained browser secret pipe was not framed')
      return Object.freeze({
        secretFrame,
        containsSensitiveValue: (encoded: string) => containsSensitiveValue(state, encoded),
        topologyProfilePath: topology.topologyProfilePath,
        topologyProfileSha256: topology.topologyProfileSha256,
        topologyResolutionPath: topology.topologyResolutionPath,
        topologyResolutionSha256: topology.topologyResolutionSha256,
        sampleDirectory,
        childAttachmentStagingRoot,
        checkoutSha: this.#options.checkoutSha,
        close: () => cleanupOwnedOperation(state),
        forceTerminateAndWait: () => cleanupInputState(state),
      })
    } catch {
      try {
        await cleanupInputState(state)
      } catch (cleanupFailure) {
        throw new AggregateError(
          [cleanupFailure],
          'contained browser input acquisition and cleanup both failed',
          { cause: cleanupFailure },
        )
      }
      // Credential-bearing parser/input failures are intentionally not chained:
      // the parent boundary must receive a constant, non-reflective symptom.
      throw new Error('contained browser input acquisition failed')
    }
  }
}

function cleanupOwnedOperation(state: InputAcquisitionState): NetworkMatrixOwnedOperation<void> {
  const result = cleanupInputState(state)
  return Object.freeze({
    result,
    forceTerminateAndWait: () => cleanupInputState(state),
  })
}

function cleanupInputState(state: InputAcquisitionState): Promise<void> {
  state.cleanup ??= cleanupInput(state).catch((cause: unknown) => {
    state.cleanup = undefined
    throw cause
  })
  return state.cleanup
}

async function cleanupInput(state: InputAcquisitionState): Promise<void> {
  const failures: unknown[] = []
  const credentialLease = state.credentialLease
  if (credentialLease !== undefined) {
    try {
      const retirement = await retireExternalFixtureControlCredential(credentialLease)
      state.credentialLease = undefined
      if (retirement.deliveryOutcome === 'release-failed-revoke-confirmed') {
        failures.push(new Error(
          'contained browser control credential release delivery degraded before forced retirement',
        ))
      }
    } catch {
      failures.push(new Error('contained browser control credential revocation failed'))
    }
  }
  if (state.secretFrame !== undefined) {
    try {
      state.secretFrame.fill(0)
    } catch {
      failures.push(new Error('contained browser in-memory secret retirement failed'))
    }
  }
  state.secretFrame = undefined
  state.credentialOffset = undefined
  state.credentialByteLength = undefined
  const directory = state.directory
  if (directory !== undefined) {
    try {
      await rm(directory, { recursive: true, force: true })
      state.directory = undefined
    } catch {
      failures.push(new Error('contained browser secret staging retirement failed'))
    }
  }
  if (failures.length !== 0) {
    throw new AggregateError(failures, 'contained browser input ownership did not retire')
  }
}

async function boundedAuthorityFile(path: string, signal: AbortSignal): Promise<string> {
  const bytes = await readFile(path, { signal })
  if (bytes.byteLength === 0 || bytes.byteLength > MAXIMUM_AUTHORITY_FILE_BYTES) {
    throw new Error('contained browser authority file is invalid')
  }
  return bytes.toString('utf8')
}

function containsSensitiveValue(state: InputAcquisitionState, encoded: string): boolean {
  const frame = state.secretFrame
  const credentialOffset = state.credentialOffset
  const credentialByteLength = state.credentialByteLength
  if (
    frame === undefined || credentialOffset === undefined ||
    credentialByteLength === undefined
  ) return false
  const candidate = Buffer.from(encoded, 'utf8')
  try {
    if (candidate.byteLength < credentialByteLength) return false
    scan: for (let start = 0; start <= candidate.byteLength - credentialByteLength; start += 1) {
      for (let index = 0; index < credentialByteLength; index += 1) {
        if (candidate[start + index] !== frame[credentialOffset + index]) continue scan
      }
      return true
    }
    return false
  } finally {
    // If the child reflected the credential, this temporary comparison buffer
    // is secret-bearing and must retire on both match and mismatch branches.
    candidate.fill(0)
  }
}

function requireActive(signal: AbortSignal): void {
  if (signal.aborted) throw new Error('contained browser input acquisition was terminated')
}
