import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
import { isProxy } from 'node:util/types'

import type { ChildEvidenceContext } from '../../browser-evidence/child-evidence.ts'
import { browserRunPolicy } from '../../browser-evidence/run-policy.ts'
import {
  BrowserSampleContainmentError,
  type BrowserSampleContainmentBackend,
  type BrowserSampleContainmentExecution,
  type BrowserSampleContainmentRequest,
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
import {
  LinuxTopologyTraceJournal,
  settleLinuxTopologyTraceJournal,
  type LinuxTopologyTraceChannel,
  type LinuxTopologyTraceIdentity,
} from './trace/index.ts'

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
}

export interface ContainedBrowserProcessExecution
extends NetworkMatrixOwnedOperation<ContainedPlaywrightCandidateEvidence> {
  readonly traces: LinuxTopologyTraceChannel
}

interface ContainedBrowserCollection {
  readonly evidence: ContainedPlaywrightCandidateEvidence | undefined
  readonly primaryFailure: unknown
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
implements NetworkMatrixContainedPlaywrightProcessBroker<LinuxTopologyTraceChannel> {
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
  ): ContainedBrowserProcessExecution {
    const state: BrokerOperationState = {
      controller: new AbortController(),
      acquisition: undefined,
      input: undefined,
      backendTerminal: undefined,
      result: undefined,
      force: undefined,
    }
    const journal = new LinuxTopologyTraceJournal()
    const identity = journal.start(
      containedBrowserTraceIdentity(context),
      'contained-browser-started',
      Object.freeze({ containmentBackend: this.#options.containment.kind }),
    )
    const result = settleLinuxTopologyTraceJournal(
      this.#run(context, state, journal, identity),
      journal,
    )
    state.result = result
    return Object.freeze({
      result,
      traces: journal.view,
      forceTerminateAndWait: (reason: NetworkMatrixOperationClass) =>
        this.#forceTerminateAndWait(state, reason),
    })
  }

  async #run(
    context: NetworkMatrixSampleExecutionContext,
    state: BrokerOperationState,
    journal: LinuxTopologyTraceJournal,
    identity: LinuxTopologyTraceIdentity,
  ): Promise<ContainedPlaywrightCandidateEvidence> {
    const collection: ContainedBrowserCollection = await this.#collectEvidence(
      context,
      state,
      journal,
      identity,
    ).then(
      (evidence) => Object.freeze({ evidence, primaryFailure: undefined }),
      (cause: unknown) => Object.freeze({
        evidence: undefined,
        primaryFailure: retainContainmentFailure(cause, journal, identity),
      }),
    )
    const closeFailure = await closeInput(state.input)
    const cleanupOutcome = containedBrowserCleanupOutcome(state.input, closeFailure)
    return finalizeContainedBrowserOperation(
      collection,
      closeFailure,
      cleanupOutcome,
      journal,
      identity,
    )
  }

  async #collectEvidence(
    context: NetworkMatrixSampleExecutionContext,
    state: BrokerOperationState,
    journal: LinuxTopologyTraceJournal,
    identity: LinuxTopologyTraceIdentity,
  ): Promise<ContainedPlaywrightCandidateEvidence> {
    journal.progress(identity, 'input-acquisition-started', 'started')
    const acquisition = this.#options.inputs.acquire(context, state.controller.signal)
    state.acquisition = acquisition
    const input = await acquisition.result
    state.input = input
    journal.progress(identity, 'input-acquisition-completed', 'succeeded')
    const request = containmentRequest(context, input, state.controller.signal, {
      options: this.#options,
      childPath: this.#childPath,
      maximumCaptureBytes: this.#maximumCaptureBytes,
    })
    journal.progress(identity, 'containment-preflight-started', 'started')
    await this.#options.containment.preflight(request)
    journal.progress(identity, 'containment-preflight-completed', 'succeeded')
    journal.progress(identity, 'contained-process-started', 'started')
    const terminal = this.#options.containment.execute(request)
    state.backendTerminal = terminal
    const execution = await terminal
    replayContainmentTraces(journal, identity, execution.traces)
    journal.progress(identity, 'contained-process-settled', 'succeeded', {
      terminationReason: execution.terminationReason,
    })
    const evidence = candidateEvidence(
      context,
      execution,
      this.#maximumCaptureBytes,
      input.containsSensitiveValue,
    )
    journal.progress(identity, 'contained-evidence-accepted', 'succeeded')
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

function retainContainmentFailure(
  cause: unknown,
  journal: LinuxTopologyTraceJournal,
  identity: LinuxTopologyTraceIdentity,
): unknown {
  if (isProxyValue(cause) || !(cause instanceof BrowserSampleContainmentError)) return cause
  try {
    replayContainmentTraces(journal, identity, cause.traces)
    return cause
  } catch (traceCause) {
    return new AggregateError(
      [cause, traceCause],
      'contained browser failure trace could not be retained',
      { cause },
    )
  }
}

function containedBrowserCleanupOutcome(
  input: ContainedBrowserSampleInputAuthority | undefined,
  closeFailure: unknown,
): 'completed' | 'failed' | 'not-required' {
  if (input === undefined) return 'not-required'
  return closeFailure === undefined ? 'completed' : 'failed'
}

function finalizeContainedBrowserOperation(
  collection: ContainedBrowserCollection,
  closeFailure: unknown,
  cleanupOutcome: 'completed' | 'failed' | 'not-required',
  journal: LinuxTopologyTraceJournal,
  identity: LinuxTopologyTraceIdentity,
): ContainedPlaywrightCandidateEvidence {
  const primaryFailure = containedBrowserPrimaryFailure(collection.primaryFailure, closeFailure)
  let traceFailure = captureTraceFailure(() => journal.progress(
    identity,
    'input-cleanup-settled',
    cleanupOutcome === 'failed' ? 'failed' : 'succeeded',
    { cleanupOutcome },
  ))
  const outcome = primaryFailure === undefined && collection.evidence !== undefined
    ? 'succeeded'
    : 'failed'
  traceFailure = combineTraceFailures(
    traceFailure,
    captureTraceFailure(() => journal.terminal(
      identity,
      'contained-browser-terminal',
      outcome,
      cleanupOutcome,
    )),
  )
  if (primaryFailure !== undefined && traceFailure !== undefined) {
    throw new AggregateError(
      [primaryFailure, traceFailure],
      'contained browser operation and lifecycle trace both failed',
      { cause: primaryFailure },
    )
  }
  if (primaryFailure !== undefined) throw primaryFailure
  if (traceFailure !== undefined) throw traceFailure
  if (collection.evidence === undefined) throw sampleEvidenceError()
  return collection.evidence
}

function containedBrowserPrimaryFailure(
  primaryFailure: unknown,
  closeFailure: unknown,
): unknown {
  if (closeFailure === undefined) return primaryFailure
  return new AggregateError(
    [...(primaryFailure === undefined ? [] : [primaryFailure]), closeFailure],
    'contained browser sample input cleanup failed',
    { cause: primaryFailure ?? closeFailure },
  )
}

function containmentRequest(
  context: NetworkMatrixSampleExecutionContext,
  input: ContainedBrowserSampleInputAuthority,
  terminationSignal: AbortSignal,
  dependencies: {
    readonly options: ConcreteContainedBrowserProcessBrokerOptions
    readonly childPath: string
    readonly maximumCaptureBytes: number
  },
): BrowserSampleContainmentRequest {
  const childContext: ChildEvidenceContext = Object.freeze({
    runId: context.runId,
    operationId: context.operationId,
    scenario: 'network-matrix-browser',
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
    capture: Object.freeze({
      stdoutBytes: dependencies.maximumCaptureBytes,
      stderrBytes: dependencies.maximumCaptureBytes,
    }),
  })
}

function candidateEvidence(
  context: NetworkMatrixSampleExecutionContext,
  execution: BrowserSampleContainmentExecution,
  maximumCaptureBytes: number,
  containsSensitiveValue: (encoded: string) => boolean,
): ContainedPlaywrightCandidateEvidence {
  const stdout = decodeOutputSnapshot(execution.output.stdout, maximumCaptureBytes)
  const stderr = decodeOutputSnapshot(execution.output.stderr, maximumCaptureBytes)
  if (
    execution.terminationReason !== 'natural' || execution.processEvidence.terminal !== 'exited' ||
    execution.processEvidence.exitCode !== 0 || containsSensitiveValue(stdout) ||
    containsSensitiveValue(stderr)
  ) throw sampleEvidenceError()
  const output = parseSingleOutput(stdout)
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

function decodeOutputSnapshot(
  snapshot: BrowserSampleContainmentExecution['output']['stdout'],
  maximumCaptureBytes: number,
): string {
  if (
    !snapshot.completed || snapshot.truncated ||
    snapshot.observedBytes !== snapshot.capturedBytes ||
    snapshot.capturedBytes > maximumCaptureBytes
  ) throw sampleEvidenceError()
  const bytes = snapshot.bytes()
  if (!(bytes instanceof Uint8Array) || bytes.byteLength !== snapshot.capturedBytes) {
    throw sampleEvidenceError()
  }
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    throw sampleEvidenceError()
  }
}

function containedBrowserTraceIdentity(
  context: NetworkMatrixSampleExecutionContext,
): LinuxTopologyTraceIdentity {
  return Object.freeze({
    component: 'contained-browser-broker',
    scenario: 'contained-browser-sample',
    operationId: context.operationId,
    runId: context.runId,
    profileId: context.identity.profileId,
    browser: context.identity.browser,
    sampleOrdinal: context.identity.sampleOrdinal,
  })
}

function replayContainmentTraces(
  journal: LinuxTopologyTraceJournal,
  identity: LinuxTopologyTraceIdentity,
  snapshot: BrowserSampleContainmentExecution['traces'],
): void {
  if (isProxyValue(snapshot) || !plainRecord(snapshot)) throw sampleEvidenceError()
  const descriptors = Object.getOwnPropertyDescriptors(snapshot)
  const expected = ['events', 'observedEvents', 'capturedEvents', 'truncated', 'completed']
  if (
    !exactEnumerableDataKeys(snapshot, descriptors, expected) ||
    descriptors.completed?.value !== true ||
    descriptors.truncated?.value !== false ||
    !Number.isSafeInteger(descriptors.observedEvents?.value) ||
    descriptors.observedEvents?.value < 0 ||
    descriptors.observedEvents?.value !== descriptors.capturedEvents?.value
  ) throw sampleEvidenceError()
  const events = descriptors.events?.value
  if (
    isProxyValue(events) ||
    !Array.isArray(events) ||
    Object.getPrototypeOf(events) !== Array.prototype ||
    events.length !== descriptors.capturedEvents?.value
  ) throw sampleEvidenceError()
  for (let index = 0; index < events.length; index += 1) {
    if (!Object.hasOwn(events, index)) throw sampleEvidenceError()
    const projected = projectContainmentTrace(events[index])
    journal.progress(
      identity,
      projected.milestone,
      projected.outcome,
      projected.context,
    )
  }
}

function projectContainmentTrace(
  value: unknown,
): {
  readonly milestone: string
  readonly outcome: 'started' | 'succeeded' | 'failed'
  readonly context: Readonly<Record<string, unknown>>
} {
  if (isProxyValue(value) || !plainRecord(value)) throw sampleEvidenceError()
  const descriptors = Object.getOwnPropertyDescriptors(value)
  const keys = descriptors.context === undefined
    ? ['milestone', 'outcome']
    : ['milestone', 'outcome', 'context']
  if (!exactEnumerableDataKeys(value, descriptors, keys)) throw sampleEvidenceError()
  const milestone = descriptors.milestone?.value
  const outcome = descriptors.outcome?.value
  if (
    typeof milestone !== 'string' ||
    outcome !== 'started' && outcome !== 'succeeded' && outcome !== 'failed'
  ) throw sampleEvidenceError()
  return Object.freeze({
    milestone,
    outcome,
    context: projectContainmentContext(descriptors.context?.value),
  })
}

function projectContainmentContext(value: unknown): Readonly<Record<string, unknown>> {
  if (value === undefined) return Object.freeze({})
  if (isProxyValue(value) || !plainRecord(value)) throw sampleEvidenceError()
  const descriptors = Object.getOwnPropertyDescriptors(value)
  if (Reflect.ownKeys(value).some((key) => typeof key !== 'string')) throw sampleEvidenceError()
  const projected: Record<string, unknown> = {}
  for (const [key, descriptor] of Object.entries(descriptors)) {
    if (!descriptor.enumerable || !('value' in descriptor)) throw sampleEvidenceError()
    const entry = key === 'failure' ? 'opaque' : descriptor.value
    if (
      entry !== null &&
      typeof entry !== 'string' &&
      typeof entry !== 'boolean' &&
      (typeof entry !== 'number' || !Number.isSafeInteger(entry))
    ) throw sampleEvidenceError()
    Object.defineProperty(projected, key, {
      value: entry,
      enumerable: true,
      configurable: false,
      writable: false,
    })
  }
  return Object.freeze(projected)
}

function exactEnumerableDataKeys(
  value: object,
  descriptors: Record<string, PropertyDescriptor>,
  expected: readonly string[],
): boolean {
  const keys = Reflect.ownKeys(value)
  if (
    keys.length !== expected.length ||
    keys.some((key) => typeof key !== 'string' || !expected.includes(key))
  ) return false
  return expected.every((key) => {
    const descriptor = descriptors[key]
    return descriptor !== undefined && descriptor.enumerable && 'value' in descriptor
  })
}

function plainRecord(value: unknown): value is object {
  return typeof value === 'object' &&
    value !== null &&
    !Array.isArray(value) &&
    Object.getPrototypeOf(value) === Object.prototype
}

function isProxyValue(value: unknown): boolean {
  return (typeof value === 'object' || typeof value === 'function') &&
    value !== null &&
    isProxy(value)
}

function captureTraceFailure(action: () => void): unknown {
  try {
    action()
    return undefined
  } catch (cause) {
    return cause
  }
}

function combineTraceFailures(left: unknown, right: unknown): unknown {
  if (left === undefined) return right
  if (right === undefined) return left
  return new AggregateError(
    [left, right],
    'contained browser lifecycle trace publication failed',
    { cause: left },
  )
}
