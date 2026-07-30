import { randomBytes } from 'node:crypto'

import type { BrowserSampleContainmentExecution } from '../../../browser-evidence/process/containment.ts'
import { executeNativeProcessGroupCommand } from '../../../browser-evidence/process/native-process-group-backend.ts'
import { inheritedSampleEnvironment } from '../../../browser-evidence/process/sample-environment.ts'
import { executeWindowsJob } from '../../../browser-evidence/process/windows-job-client.ts'
import type { ParentWorkloadIdentityAuthority } from '../parent-workload-identity.ts'
import type {
  CredentialBrokerExchange,
  CredentialBrokerPipeExchange,
  InternalCredentialBrokerOptions,
} from './contracts.ts'
import {
  BoundedBrokerCapture,
  copyBrokerPipeResponse,
  encodeBrokerPipeFrame,
} from './pipe-protocol.ts'

const BROKER_OPERATION_DEADLINE_MS = 15_000
const BROKER_TERMINATION_GRACE_MS = 5_000

interface CredentialBrokerProcessOwnerOptions {
  readonly helperPath: string
  readonly workingDirectory: string
  readonly platform: NodeJS.Platform
  readonly windowsJobHelperPath?: string
  readonly workloadIdentity: ParentWorkloadIdentityAuthority
  readonly pipeExchange?: CredentialBrokerPipeExchange
  readonly trace?: (event: Readonly<Record<string, unknown>>) => void
}

/**
 * Host authentication and helper containment share one owner so neither a
 * credential-bearing argv nor an environment channel can emerge between layers.
 */
export class CredentialBrokerProcessOwner {
  readonly #options: CredentialBrokerProcessOwnerOptions

  constructor(options: InternalCredentialBrokerOptions) {
    this.#options = Object.freeze({
      helperPath: options.helperPath,
      workingDirectory: options.workingDirectory,
      platform: options.platform,
      ...(options.windowsJobHelperPath === undefined
        ? {}
        : { windowsJobHelperPath: options.windowsJobHelperPath }),
      workloadIdentity: options.workloadIdentity,
      ...(options.pipeExchange === undefined ? {} : { pipeExchange: options.pipeExchange }),
      ...(options.trace === undefined ? {} : { trace: options.trace }),
    })
  }

  readonly exchange: CredentialBrokerExchange = async (
    request,
    scope,
    signal,
    onDispatch,
  ) => {
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
        return this.#exchangePipe(pipeExchange, operationId, stdin, signal, onDispatch)
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

  async closeIdentity(force: boolean): Promise<void> {
    const receipt = force
      ? await this.#options.workloadIdentity.forceTerminateAndWait()
      : await this.#options.workloadIdentity.closeAndWait()
    if (!exactClosedReceipt(receipt)) {
      throw new Error('parent workload identity did not publish its terminal receipt')
    }
  }

  async #exchangePipe(
    pipeExchange: CredentialBrokerPipeExchange,
    operationId: string,
    stdin: Buffer,
    signal: AbortSignal,
    onDispatch: (() => void) | undefined,
  ): Promise<Buffer> {
    onDispatch?.()
    const output: unknown = await pipeExchange.exchange({ operationId, stdin, signal })
    return copyBrokerPipeResponse({ request: stdin, output, signal })
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

function exactClosedReceipt(value: unknown): value is { readonly terminal: 'closed' } {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false
  const receipt = value as Record<string, unknown>
  return Object.keys(receipt).length === 1 && Object.keys(receipt)[0] === 'terminal' &&
    receipt.terminal === 'closed'
}

function requireActive(signal: AbortSignal): void {
  if (signal.aborted) throw new Error('credential broker operation was terminated')
}
