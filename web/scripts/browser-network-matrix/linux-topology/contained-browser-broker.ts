import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

import type { ChildEvidenceContext } from '../../browser-evidence/child-evidence.ts'
import { browserRunPolicy } from '../../browser-evidence/run-policy.ts'
import type {
  BrowserSampleContainmentBackend,
  BrowserSampleContainmentExecution,
  BrowserSampleContainmentRequest,
  BrowserSampleContainmentTrace,
} from '../../browser-evidence/process/containment.ts'
import {
  NetworkMatrixSampleExecutionError,
  type NetworkMatrixSampleExecutionContext,
} from '../runner.ts'
import type {
  NetworkMatrixOperationClass,
  NetworkMatrixOwnedOperation,
} from '../owned-operation.ts'
import type {
  ContainedPlaywrightCandidateEvidence,
  NetworkMatrixContainedPlaywrightProcessBroker,
} from '../contained-playwright.ts'
import {
  parseNetworkMatrixAttemptEvidence,
  networkMatrixPionSelectedPairFromTerminalReceipt,
  type NetworkMatrixAttemptEvidence,
} from '../attempt-evidence.ts'
import {
  parseContainedBrowserSampleOutput,
  type ContainedBrowserSampleOutput,
} from './contained-browser-sample.ts'

const DEFAULT_CHILD_PATH = fileURLToPath(new URL('./sample-child.ts', import.meta.url))
const MAXIMUM_CAPTURE_BYTES = 4_194_304
const NETWORK_MATRIX_PROCESS_POLICY = browserRunPolicy('stability')

export interface ContainedBrowserSampleInputAuthority {
  readonly secretFrame: Uint8Array
  containsSensitiveValue(encoded: string): boolean
  readonly topologyProfilePath: string
  readonly topologyProfileSha256: string
  readonly topologyResolutionPath: string
  readonly topologyResolutionSha256: string
  readonly sampleDirectory: string
  readonly childAttachmentStagingRoot: string
  readonly checkoutSha: string
  close(): NetworkMatrixOwnedOperation<void>
  forceTerminateAndWait(reason: NetworkMatrixOperationClass): Promise<void>
}

export interface ContainedBrowserSampleInputAuthorityFactory {
  acquire(
    context: NetworkMatrixSampleExecutionContext,
    signal: AbortSignal,
  ): NetworkMatrixOwnedOperation<ContainedBrowserSampleInputAuthority>
}

export interface ConcreteContainedBrowserProcessBrokerOptions {
  readonly containment: BrowserSampleContainmentBackend
  readonly inputs: ContainedBrowserSampleInputAuthorityFactory
  readonly nodeExecutable: string
  readonly repositoryRoot: string
  readonly processDeadlineMs: number
  readonly terminationGraceMs: number
  readonly maximumCaptureBytes?: number
  readonly childPath?: string
  readonly trace?: (event: {
    readonly operationId: string
    readonly milestone: string
    readonly context?: Readonly<Record<string, unknown>>
  }) => void
}

interface BrokerOperationState {
  readonly controller: AbortController
  acquisition: NetworkMatrixOwnedOperation<ContainedBrowserSampleInputAuthority> | undefined
  input: ContainedBrowserSampleInputAuthority | undefined
  backendTerminal: Promise<BrowserSampleContainmentExecution> | undefined
  result: Promise<ContainedPlaywrightCandidateEvidence> | undefined
  force: Promise<void> | undefined
}

export class ConcreteContainedBrowserProcessBroker
implements NetworkMatrixContainedPlaywrightProcessBroker {
  readonly #options: ConcreteContainedBrowserProcessBrokerOptions
  readonly #maximumCaptureBytes: number
  readonly #childPath: string

  constructor(options: ConcreteContainedBrowserProcessBrokerOptions) {
    if (
      !Number.isSafeInteger(options.processDeadlineMs) || options.processDeadlineMs < 1 ||
      !Number.isSafeInteger(options.terminationGraceMs) || options.terminationGraceMs < 1
    ) throw new Error('contained browser process broker authority is invalid')
    const maximumCaptureBytes = options.maximumCaptureBytes ?? MAXIMUM_CAPTURE_BYTES
    if (!Number.isSafeInteger(maximumCaptureBytes) || maximumCaptureBytes < 1) {
      throw new Error('contained browser process capture authority is invalid')
    }
    this.#options = options
    this.#maximumCaptureBytes = maximumCaptureBytes
    this.#childPath = options.childPath ?? DEFAULT_CHILD_PATH
  }

  start(
    context: NetworkMatrixSampleExecutionContext,
  ): NetworkMatrixOwnedOperation<ContainedPlaywrightCandidateEvidence> {
    const state: BrokerOperationState = {
      controller: new AbortController(),
      acquisition: undefined,
      input: undefined,
      backendTerminal: undefined,
      result: undefined,
      force: undefined,
    }
    const result = this.#run(context, state)
    state.result = result
    return Object.freeze({
      result,
      forceTerminateAndWait: (reason: NetworkMatrixOperationClass) =>
        this.#forceTerminateAndWait(state, reason),
    })
  }

  async #run(
    context: NetworkMatrixSampleExecutionContext,
    state: BrokerOperationState,
  ): Promise<ContainedPlaywrightCandidateEvidence> {
    let primaryFailure: unknown
    let evidence: ContainedPlaywrightCandidateEvidence | undefined
    try {
      const acquisition = this.#options.inputs.acquire(context, state.controller.signal)
      state.acquisition = acquisition
      const input = await acquisition.result
      state.input = input
      const stdout = new BoundedCapture(this.#maximumCaptureBytes)
      const stderr = new BoundedCapture(this.#maximumCaptureBytes)
      const request = containmentRequest(context, input, state.controller.signal, {
        options: this.#options,
        childPath: this.#childPath,
        stdout,
        stderr,
      })
      await this.#options.containment.preflight(request)
      const terminal = this.#options.containment.execute(request)
      state.backendTerminal = terminal
      const execution = await terminal
      evidence = candidateEvidence(
        context,
        execution,
        stdout,
        stderr,
        input.containsSensitiveValue,
      )
    } catch (cause) {
      primaryFailure = cause
    }
    const closeFailure = await closeInput(state.input)
    if (closeFailure !== undefined) {
      throw new AggregateError(
        [...(primaryFailure === undefined ? [] : [primaryFailure]), closeFailure],
        'contained browser sample input cleanup failed',
      )
    }
    if (primaryFailure !== undefined) throw primaryFailure
    if (evidence === undefined) throw sampleEvidenceError()
    return evidence
  }

  #forceTerminateAndWait(
    state: BrokerOperationState,
    reason: NetworkMatrixOperationClass,
  ): Promise<void> {
    state.force ??= this.#force(state, reason)
    return state.force
  }

  async #force(state: BrokerOperationState, reason: NetworkMatrixOperationClass): Promise<void> {
    state.controller.abort()
    const failures: unknown[] = []
    if (state.acquisition !== undefined) {
      await collectFailure(failures, () => state.acquisition?.forceTerminateAndWait(reason))
    }
    if (state.backendTerminal !== undefined) {
      await collectFailure(failures, async () => { await state.backendTerminal })
    }
    if (state.input !== undefined) {
      await collectFailure(failures, () => state.input?.forceTerminateAndWait(reason))
    }
    if (state.result !== undefined) await state.result.catch(() => undefined)
    if (failures.length !== 0) {
      throw new AggregateError(failures, 'contained browser force termination did not settle ownership')
    }
  }
}

function containmentRequest(
  context: NetworkMatrixSampleExecutionContext,
  input: ContainedBrowserSampleInputAuthority,
  terminationSignal: AbortSignal,
  dependencies: {
    readonly options: ConcreteContainedBrowserProcessBrokerOptions
    readonly childPath: string
    readonly stdout: BoundedCapture
    readonly stderr: BoundedCapture
  },
): BrowserSampleContainmentRequest {
  const childContext: ChildEvidenceContext = Object.freeze({
    runId: context.runId,
    runPolicy: NETWORK_MATRIX_PROCESS_POLICY,
    suite: 'pion',
    browser: context.identity.browser,
    sampleIndex: context.identity.sampleOrdinal,
    checkoutSha: input.checkoutSha,
    topologyProfileSha256: input.topologyProfileSha256,
    topologyResolutionSha256: input.topologyResolutionSha256,
    topologyProfilePath: input.topologyProfilePath,
    topologyResolutionPath: input.topologyResolutionPath,
    evidencePath: join(input.childAttachmentStagingRoot, 'unused-evidence.jsonl'),
    artifactRoot: input.childAttachmentStagingRoot,
  })
  return Object.freeze({
    operationId: context.operationId,
    topologyProfilePath: input.topologyProfilePath,
    topologyProfileSha256: input.topologyProfileSha256,
    topologyResolutionPath: input.topologyResolutionPath,
    topologyResolutionSha256: input.topologyResolutionSha256,
    readOnlyInputRoots: Object.freeze([]),
    terminationSignal,
    command: Object.freeze({
      executable: dependencies.options.nodeExecutable,
      arguments: Object.freeze([
        dependencies.childPath,
        '--browser', context.identity.browser,
      ]),
      cwd: dependencies.options.repositoryRoot,
      stdin: input.secretFrame,
    }),
    sampleDirectory: input.sampleDirectory,
    childAttachmentStagingRoot: input.childAttachmentStagingRoot,
    childContext,
    deadlineMs: dependencies.options.processDeadlineMs,
    terminationGraceMs: dependencies.options.terminationGraceMs,
    stdout: (chunk: Uint8Array) => dependencies.stdout.consume(chunk),
    stderr: (chunk: Uint8Array) => dependencies.stderr.consume(chunk),
    trace: (event: BrowserSampleContainmentTrace) => dependencies.options.trace?.(Object.freeze({
      operationId: context.operationId,
      milestone: event.milestone,
      ...(event.context === undefined ? {} : { context: event.context }),
    })) ?? undefined,
  })
}

function candidateEvidence(
  context: NetworkMatrixSampleExecutionContext,
  execution: BrowserSampleContainmentExecution,
  stdout: BoundedCapture,
  stderr: BoundedCapture,
  containsSensitiveValue: (encoded: string) => boolean,
): ContainedPlaywrightCandidateEvidence {
  if (
    execution.timedOut || execution.processEvidence.terminal !== 'exited' ||
    execution.processEvidence.exitCode !== 0 || stdout.truncated || stderr.truncated ||
    containsSensitiveValue(stdout.encoded) ||
    containsSensitiveValue(stderr.encoded)
  ) throw sampleEvidenceError()
  const output = parseSingleOutput(stdout.encoded)
  if (output.browser !== context.identity.browser) throw sampleEvidenceError()
  return Object.freeze({
    processInstanceId: output.processInstanceId,
    attemptEvidence: attemptEvidence(context, output),
  })
}

function attemptEvidence(
  context: NetworkMatrixSampleExecutionContext,
  output: ContainedBrowserSampleOutput,
): NetworkMatrixAttemptEvidence {
  const restricted = context.identity.profileId === 'scheduled-restricted-udp'
  const sampleAuthority = output.protocolResult.attemptAuthority
    .requestAuthority.controlAuthority.sampleAuthority
  if (
    output.protocolResult.runId !== context.runId ||
    sampleAuthority.runId !== context.runId ||
    sampleAuthority.profileId !== context.identity.profileId ||
    sampleAuthority.browser !== context.identity.browser ||
    sampleAuthority.sampleOrdinal !== context.identity.sampleOrdinal ||
    sampleAuthority.processInstanceId !== output.processInstanceId ||
    sampleAuthority.operationId !== context.operationId ||
    restricted && (
      output.protocolResult.state !== 'failed' ||
      output.protocolResult.failureCode !== 'ice-failed' ||
      output.protocolResult.selectedPair !== null ||
      output.protocolResult.challengeEchoed
    ) ||
    !restricted && (
      output.protocolResult.state !== 'established' ||
      output.protocolResult.failureCode !== null ||
      output.protocolResult.selectedPair === null ||
      !output.protocolResult.challengeEchoed
    )
  ) throw sampleEvidenceError()
  try {
    return parseNetworkMatrixAttemptEvidence({
      attemptAuthority: output.protocolResult.attemptAuthority,
      pionAuthority: 'external-remote',
      externalFixture: {
        runId: output.protocolResult.runId,
        authorityInstanceId: output.protocolResult.authorityInstanceId,
        remoteServiceInstanceId: output.protocolResult.remoteServiceInstanceId,
        attestationSha256: output.protocolResult.attestationSha256,
        attestationPublicKeySpki: output.protocolResult.attestationPublicKeySpki,
        signedAttestation: output.protocolResult.signedAttestation,
        networkBindingSha256: output.protocolResult.networkBindingSha256,
        remotePeerBindingSha256: output.protocolResult.remotePeerBindingSha256,
        controllerPublicIp: output.protocolResult.controllerPublicIp,
        attestationExpiresAt: output.protocolResult.attestationExpiresAt,
        remotePeerPublicIp: output.protocolResult.remotePeerPublicIp,
        remotePeerUdpPortMin: output.protocolResult.remotePeerUdpPortMin,
        remotePeerUdpPortMax: output.protocolResult.remotePeerUdpPortMax,
      },
      browserSelectedPair: output.browserSelectedPair,
      pionSelectedPair: networkMatrixPionSelectedPairFromTerminalReceipt(
        output.protocolResult.selectedPair,
      ),
      challenge: restricted
        ? null
        : {
            bindingSha256: output.protocolResult.challengeBindingSha256,
            challenge: output.protocolResult.challenge,
            pionChallengeObserved: true,
            browserEchoObserved: true,
          },
      terminalReceipt: output.protocolResult.terminalReceipt,
    }, context.identity.profileId)
  } catch {
    throw sampleEvidenceError()
  }
}

export function parseContainedBrowserSampleStdout(encoded: string): ContainedBrowserSampleOutput {
  return parseSingleOutput(encoded)
}

function parseSingleOutput(encoded: string): ContainedBrowserSampleOutput {
  if (!encoded.endsWith('\n')) throw sampleEvidenceError()
  const body = encoded.slice(0, -1)
  if (body.length === 0 || body.includes('\n') || body.includes('\r')) throw sampleEvidenceError()
  let value: unknown
  try {
    value = JSON.parse(body)
  } catch {
    throw sampleEvidenceError()
  }
  try {
    return parseContainedBrowserSampleOutput(value)
  } catch {
    throw sampleEvidenceError()
  }
}

async function closeInput(input: ContainedBrowserSampleInputAuthority | undefined): Promise<unknown> {
  if (input === undefined) return undefined
  try {
    await input.close().result
    return undefined
  } catch (cause) {
    return cause
  }
}

async function collectFailure(
  failures: unknown[],
  operation: () => Promise<unknown> | undefined,
): Promise<void> {
  try {
    await operation()
  } catch (cause) {
    failures.push(cause)
  }
}

function sampleEvidenceError(): NetworkMatrixSampleExecutionError {
  return new NetworkMatrixSampleExecutionError(
    'evidence-collection-failed',
    'contained browser sample did not produce one bounded authoritative result',
  )
}

class BoundedCapture {
  readonly #maximumBytes: number
  readonly #chunks: Uint8Array[] = []
  #observedBytes = 0
  #capturedBytes = 0

  constructor(maximumBytes: number) {
    this.#maximumBytes = maximumBytes
  }

  consume(chunk: Uint8Array): void {
    this.#observedBytes += chunk.byteLength
    const remaining = this.#maximumBytes - this.#capturedBytes
    if (remaining <= 0) return
    const accepted = chunk.subarray(0, Math.min(remaining, chunk.byteLength))
    this.#chunks.push(accepted.slice())
    this.#capturedBytes += accepted.byteLength
  }

  get truncated(): boolean {
    return this.#observedBytes !== this.#capturedBytes
  }

  get encoded(): string {
    try {
      const combined = new Uint8Array(this.#capturedBytes)
      let offset = 0
      for (const chunk of this.#chunks) {
        combined.set(chunk, offset)
        offset += chunk.byteLength
      }
      return new TextDecoder('utf-8', { fatal: true }).decode(combined)
    } catch {
      throw sampleEvidenceError()
    }
  }
}
