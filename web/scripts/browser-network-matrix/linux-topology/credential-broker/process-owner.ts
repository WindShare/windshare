import { randomBytes } from 'node:crypto'

import { inheritedSampleEnvironment } from '../../../browser-evidence/process/sample-environment.ts'
import {
  executeTestProcessOwner,
  type TestProcessOwnerArtifact,
  type TestProcessOwnerExecution,
} from '../../../browser-evidence/process/test-process-owner-client.mjs'
import type { ParentWorkloadIdentityAuthority } from '../parent-workload-identity.ts'
import {
  LinuxTopologyTraceJournal,
  settleLinuxTopologyTraceJournal,
  type LinuxTopologyTraceChannel,
  type LinuxTopologyTraceIdentity,
  type LinuxTopologyTraceOutcome,
} from '../trace/index.ts'
import type {
  CredentialBrokerExchange,
  CredentialBrokerPipeExchange,
  CredentialBrokerDispatchOutcome,
  InternalCredentialBrokerOptions,
} from './contracts.ts'
import {
  BoundedBrokerCapture,
  copyBrokerPipeResponse,
  encodeBrokerPipeFrame,
  MAXIMUM_BROKER_FRAME_BYTES,
} from './pipe-protocol.ts'

const BROKER_OPERATION_DEADLINE_MS = 15_000
const BROKER_TERMINATION_GRACE_MS = 5_000

interface CredentialBrokerTraceState {
  cleanupOutcome: 'completed' | 'failed' | 'not-required'
  response: Buffer | undefined
}

interface CredentialBrokerProcessOwnerOptions {
  readonly helperPath: string
  readonly workingDirectory: string
  readonly platform: NodeJS.Platform
  readonly processOwner: TestProcessOwnerArtifact
  readonly workloadIdentity: ParentWorkloadIdentityAuthority
  readonly pipeExchange?: CredentialBrokerPipeExchange
}

/**
 * Host authentication and helper containment share one owner so neither a
 * credential-bearing argv nor an environment channel can emerge between layers.
 */
export class CredentialBrokerProcessOwner {
  readonly #options: CredentialBrokerProcessOwnerOptions
  readonly #journal = new LinuxTopologyTraceJournal()

  readonly traces: LinuxTopologyTraceChannel = this.#journal.view

  constructor(options: InternalCredentialBrokerOptions) {
    this.#options = Object.freeze({
      helperPath: options.helperPath,
      workingDirectory: options.workingDirectory,
      platform: options.platform,
      processOwner: options.processOwner,
      workloadIdentity: options.workloadIdentity,
      ...(options.pipeExchange === undefined ? {} : { pipeExchange: options.pipeExchange }),
    })
  }

  readonly exchange: CredentialBrokerExchange = (
    request,
    scope,
    signal,
    ...legacyCallbackAuthority: readonly unknown[]
  ) => {
    if (legacyCallbackAuthority.length !== 0) {
      throw new Error('credential broker exchange does not accept callback authority')
    }
    const operationId = `credential-broker-${randomBytes(24).toString('hex')}`
    const trace = new CredentialBrokerTraceOperation(
      this.#journal,
      credentialBrokerTraceIdentity(operationId, scope),
    )
    const dispatch = new CredentialBrokerDispatchOwner()
    const state: CredentialBrokerTraceState = {
      cleanupOutcome: 'not-required',
      response: undefined,
    }
    const operation = this.#exchange(
      request,
      scope,
      signal,
      operationId,
      trace,
      dispatch,
      state,
    )
    const terminalized = operation.then(
      (response) => {
        dispatch.settleNotDispatched()
        return settleCredentialBrokerSuccess(response, trace, state)
      },
      (cause: unknown) => {
        dispatch.settleNotDispatched()
        return settleCredentialBrokerFailure(cause, trace, state)
      },
    )
    const result = settleLinuxTopologyTraceJournal(terminalized, trace.journal)
      .catch((cause: unknown) => {
        state.response?.fill(0)
        throw cause
      })
    return Object.freeze({
      result,
      traces: trace.traces,
      dispatchOutcome: dispatch.outcome,
    })
  }

  async #exchange(
    request: Readonly<Record<string, unknown>>,
    scope: Parameters<CredentialBrokerExchange>[1],
    signal: AbortSignal,
    operationId: string,
    trace: CredentialBrokerTraceOperation,
    dispatch: CredentialBrokerDispatchOwner,
    state: CredentialBrokerTraceState,
  ): Promise<Buffer> {
    requireActive(signal)
    trace.progress('workload-identity-requested', 'started')
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
    try {
      trace.progress('workload-identity-issued', 'succeeded')
      trace.progress('broker-request-prepared', 'succeeded')
      const pipeExchange = this.#options.pipeExchange
      if (pipeExchange !== undefined) {
        const response = await this.#exchangePipe(
          pipeExchange,
          operationId,
          stdin,
          signal,
          trace,
          dispatch,
        )
        return this.#acceptResponse(trace, response)
      }
      const stdout = new BoundedBrokerCapture()
      const stderr = new BoundedBrokerCapture()
      try {
        const terminal = this.#execute(operationId, stdin, signal, trace, dispatch)
        state.cleanupOutcome = 'failed'
        const execution = await terminal
        state.cleanupOutcome = execution.cleanupOutcome === 'completed' && execution.treeEmpty
          ? 'completed'
          : 'failed'
        stdout.consume(execution.output.stdout.bytes())
        stderr.consume(execution.output.stderr.bytes())
        if (
          execution.ownershipEvidence.terminationReason === 'deadline' ||
          !execution.treeEmpty || execution.cleanupOutcome !== 'completed' ||
          execution.inputEvidence.outcome !== 'delivered' ||
          execution.processEvidence.terminal !== 'exited' ||
          execution.processEvidence.exitCode !== 0 || stderr.byteLength !== 0
        ) throw new Error('credential broker process did not publish an authenticated response')
        const response = stdout.take()
        stderr.erase()
        if (signal.aborted) {
          response.fill(0)
          requireActive(signal)
        }
        return this.#acceptResponse(trace, response)
      } catch (cause) {
        stdout.erase()
        stderr.erase()
        throw cause
      }
    } finally {
      stdin.fill(0)
    }
  }

  #acceptResponse(trace: CredentialBrokerTraceOperation, response: Buffer): Buffer {
    try {
      trace.progress('broker-response-accepted', 'succeeded')
      return response
    } catch (cause) {
      response.fill(0)
      throw cause
    }
  }

  async closeIdentity(force: boolean): Promise<void> {
    const receipt = force
      ? await this.#options.workloadIdentity.forceTerminateAndWait()
      : await this.#options.workloadIdentity.closeAndWait()
    if (!exactClosedReceipt(receipt)) {
      throw new Error('parent workload identity did not publish its terminal receipt')
    }
    this.#journal.finish()
  }

  async #exchangePipe(
    pipeExchange: CredentialBrokerPipeExchange,
    operationId: string,
    stdin: Buffer,
    signal: AbortSignal,
    trace: CredentialBrokerTraceOperation,
    dispatch: CredentialBrokerDispatchOwner,
  ): Promise<Buffer> {
    trace.progress('broker-pipe-dispatched', 'started')
    dispatch.markDispatched()
    const output: unknown = await pipeExchange.exchange({ operationId, stdin, signal })
    return copyBrokerPipeResponse({ request: stdin, output, signal })
  }

  #execute(
    operationId: string,
    stdin: Uint8Array,
    signal: AbortSignal,
    trace: CredentialBrokerTraceOperation,
    dispatch: CredentialBrokerDispatchOwner,
  ): Promise<TestProcessOwnerExecution> {
    const command = Object.freeze({
      executable: this.#options.helperPath,
      arguments: Object.freeze([]),
      cwd: this.#options.workingDirectory,
      stdin,
    })
    trace.progress('test-process-owner-started', 'started')
    dispatch.markDispatched()
    return executeTestProcessOwner({
      owner: this.#options.processOwner,
      runId: operationId,
      operationId,
      scenario: 'credential-broker',
      command,
      environment: inheritedSampleEnvironment(),
      deadlineMs: BROKER_OPERATION_DEADLINE_MS,
      terminationGraceMs: BROKER_TERMINATION_GRACE_MS,
      terminationSignal: signal,
      platform: this.#options.platform,
      capture: Object.freeze({
        stdoutBytes: MAXIMUM_BROKER_FRAME_BYTES,
        stderrBytes: MAXIMUM_BROKER_FRAME_BYTES,
      }),
    })
  }
}

class CredentialBrokerDispatchOwner {
  readonly outcome: Promise<CredentialBrokerDispatchOutcome>

  #resolve: ((outcome: CredentialBrokerDispatchOutcome) => void) | undefined
  #published: CredentialBrokerDispatchOutcome | undefined

  constructor() {
    this.outcome = new Promise((resolve) => {
      this.#resolve = resolve
    })
  }

  markDispatched(): void {
    if (this.#published !== undefined) {
      throw new Error('credential broker dispatch outcome was published more than once')
    }
    this.#publish('dispatched')
  }

  settleNotDispatched(): void {
    if (this.#published === undefined) this.#publish('not-dispatched')
  }

  #publish(outcome: CredentialBrokerDispatchOutcome): void {
    this.#published = outcome
    const resolve = this.#resolve
    this.#resolve = undefined
    resolve?.(outcome)
  }
}

/**
 * Each helper invocation owns a complete channel while the process owner mirrors
 * the same canonical lifecycle into its authority-wide ledger. The registries can
 * await the operation without gaining synchronous write authority over either.
 */
class CredentialBrokerTraceOperation {
  readonly journal = new LinuxTopologyTraceJournal()
  readonly traces: LinuxTopologyTraceChannel = this.journal.view

  readonly #aggregate: LinuxTopologyTraceJournal
  readonly #identity: LinuxTopologyTraceIdentity

  constructor(
    aggregate: LinuxTopologyTraceJournal,
    identity: LinuxTopologyTraceIdentity,
  ) {
    this.#aggregate = aggregate
    this.#identity = this.journal.start(identity, 'credential-broker-started')
    this.#aggregate.start(this.#identity, 'credential-broker-started')
  }

  progress(
    milestone: string,
    outcome: LinuxTopologyTraceOutcome,
    context: Readonly<Record<string, unknown>> = Object.freeze({}),
  ): void {
    this.#publish(
      () => this.journal.progress(this.#identity, milestone, outcome, context),
      () => this.#aggregate.progress(this.#identity, milestone, outcome, context),
    )
  }

  terminal(
    outcome: Exclude<LinuxTopologyTraceOutcome, 'started'>,
    cleanupOutcome: CredentialBrokerTraceState['cleanupOutcome'],
  ): void {
    this.#publish(
      () => this.journal.terminal(
        this.#identity,
        'credential-broker-terminal',
        outcome,
        cleanupOutcome,
      ),
      () => this.#aggregate.terminal(
        this.#identity,
        'credential-broker-terminal',
        outcome,
        cleanupOutcome,
      ),
    )
  }

  assertHealthy(): void {
    this.#publish(
      () => this.journal.assertHealthy(),
      () => this.#aggregate.assertHealthy(),
    )
  }

  #publish(operation: () => void, aggregate: () => void): void {
    const failures: unknown[] = []
    try {
      operation()
    } catch (cause) {
      failures.push(cause)
    }
    try {
      aggregate()
    } catch (cause) {
      failures.push(cause)
    }
    if (failures.length === 1) throw failures[0]
    if (failures.length > 1) {
      throw new AggregateError(
        failures,
        'credential broker operation and aggregate trace publication both failed',
        { cause: failures[0] },
      )
    }
  }
}

function settleCredentialBrokerSuccess(
  response: Buffer,
  trace: CredentialBrokerTraceOperation,
  state: CredentialBrokerTraceState,
): Buffer {
  state.response = response
  try {
    trace.terminal('succeeded', state.cleanupOutcome)
    trace.assertHealthy()
    return response
  } catch (cause) {
    response.fill(0)
    state.response = undefined
    throw cause
  }
}

function settleCredentialBrokerFailure(
  cause: unknown,
  trace: CredentialBrokerTraceOperation,
  state: CredentialBrokerTraceState,
): never {
  let traceFailure: unknown
  try {
    trace.terminal('failed', state.cleanupOutcome)
    trace.assertHealthy()
  } catch (candidate) {
    traceFailure = candidate
  }
  if (traceFailure !== undefined) {
    throw new AggregateError(
      [cause, traceFailure],
      'credential broker operation and lifecycle trace both failed',
      { cause },
    )
  }
  throw cause
}

function credentialBrokerTraceIdentity(
  operationId: string,
  scope: Parameters<CredentialBrokerExchange>[1],
): LinuxTopologyTraceIdentity {
  return Object.freeze({
    component: 'credential-broker-process-owner',
    scenario: 'credential-broker-exchange',
    operationId,
    runId: scope.sampleAuthority.runId,
    profileId: scope.sampleAuthority.profileId,
    browser: scope.sampleAuthority.browser,
    sampleOrdinal: scope.sampleAuthority.sampleOrdinal,
  })
}

function exactClosedReceipt(value: unknown): value is { readonly terminal: 'closed' } {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false
  const receipt = value as Record<string, unknown>
  return Object.keys(receipt).length === 1 && Object.keys(receipt)[0] === 'terminal' &&
    receipt.terminal === 'closed'
}

function requireActive(signal: AbortSignal): void {
  if (signal.aborted) throw new Error('credential broker operation was terminated')
}
