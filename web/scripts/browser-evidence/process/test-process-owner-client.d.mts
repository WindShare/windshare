export interface TestProcessOwnerArtifact {
  readonly path: string
}

export class TestProcessOwnerDeadlineError extends Error {
  constructor(message: string)
}

export class TestProcessOwnerControlError extends Error {
  readonly settlement: TestProcessOwnerExecution
  constructor(message: string, settlement: TestProcessOwnerExecution, cause: unknown)
}

export interface TestProcessOwnerTerminalEvidence {
  readonly code: number | null
  readonly signal: NodeJS.Signals | null
}

export interface TestProcessOwnerStartEvidence {
  readonly schemaVersion: 'windshare.process-owner-start-evidence/v1'
  readonly runId: string
  readonly operationId: string
  readonly scenario: string
  readonly platform: 'windows_job' | 'linux_subreaper'
  readonly processId: number
  readonly processInstance: string
  readonly executable: Readonly<{
    readonly volume: string
    readonly object: string
  }>
}

export class TestProcessOwnerTransportError extends Error {
  readonly settlement: TestProcessOwnerExecution | undefined
  readonly terminal: TestProcessOwnerTerminalEvidence | undefined
  readonly output: TestProcessOwnerOutput
  readonly events: OwnedEventSnapshot<TestProcessOwnerEvent>
  constructor(
    message: string,
    settlement: TestProcessOwnerExecution | undefined,
    terminal: TestProcessOwnerTerminalEvidence | undefined,
    output: TestProcessOwnerOutput,
    events: OwnedEventSnapshot<TestProcessOwnerEvent>,
    cause: unknown,
  )
}

export type TestProcessOwnerProcessEvidence =
  | { readonly terminal: 'not-started' }
  | { readonly terminal: 'exited'; readonly exitCode: number }
  | { readonly terminal: 'signaled'; readonly signal: string }
  | {
      readonly terminal: 'spawn-failed'
      readonly errorCode: string
      readonly errorMessage: string
    }

export interface TestProcessOwnerInputEvidence {
  readonly outcome: 'not_requested' | 'delivered' | 'failed' | 'not_started' | 'evidence_lost'
  readonly failureCode: string
  readonly failureMessage: string
}

export type TestProcessOwnerTerminationReason =
  | 'natural'
  | 'deadline'
  | 'stop'
  | 'parent_lost'
  | 'initialization_failed'
  | 'start_rejected'
  | 'owner_failure'

export interface TestProcessOwnerEvent {
  readonly schemaVersion: 'windshare.test-event/v1'
  readonly runId: string
  readonly operationId: string
  readonly scenario: string
  readonly component: string
  readonly milestone: string
  readonly outcome: string
  readonly payload?: unknown
}

export type TestProcessOwnerStartDecision =
  | Readonly<{ readonly outcome: 'accepted' }>
  | Readonly<{
      readonly outcome: 'rejected'
      readonly failureCode: string
      readonly failureMessage: string
    }>

export interface TestProcessOwnerExecution {
  readonly processEvidence: TestProcessOwnerProcessEvidence
  readonly startEvidence?: TestProcessOwnerStartEvidence
  readonly treeEmpty: boolean
  readonly cleanupOutcome: 'completed' | 'failed'
  readonly inputEvidence: TestProcessOwnerInputEvidence
  readonly ownerFailure?: Readonly<{ readonly code: string; readonly message: string }>
  readonly output: TestProcessOwnerOutput
  readonly events: OwnedEventSnapshot<TestProcessOwnerEvent>
  readonly ownershipEvidence: {
    readonly kind: 'test-process-owner'
    readonly backend: 'windows_job' | 'linux_subreaper'
    readonly terminationReason: TestProcessOwnerTerminationReason
    readonly platform: Readonly<Record<string, unknown>>
  }
}

export interface TestProcessOwnerOutput {
  readonly stdout: OwnedByteSnapshot
  readonly stderr: OwnedByteSnapshot
}

export interface TestProcessOwnerRun {
  readonly stdout: OwnedByteChannel
  readonly stderr: OwnedByteChannel
  readonly events: OwnedEventChannel<TestProcessOwnerEvent>
  readonly completion: Promise<TestProcessOwnerExecution>
}

export type TestProcessOwnerFailureEvidence =
  | Readonly<{
      kind: 'transport-failed'
      settlement: TestProcessOwnerExecution | undefined
      transportEvidence: Readonly<{
        kind: 'test-process-owner-transport'
        terminal: TestProcessOwnerTerminalEvidence | null
      }>
    }>
  | Readonly<{
      kind: 'control-failed'
      settlement: TestProcessOwnerExecution
      transportEvidence: Readonly<{
        kind: 'test-process-owner-control'
        publication: 'failed'
      }>
    }>

export interface ExecuteTestProcessOwnerOptions {
  readonly owner: TestProcessOwnerArtifact
  readonly runId: string
  readonly operationId: string
  readonly scenario: string
  readonly command: {
    readonly executable: string
    readonly arguments: readonly string[]
    readonly cwd: string
    readonly stdin?: Uint8Array
  }
  readonly environment: Readonly<Record<string, string | undefined>>
  readonly deadlineMs: number
  readonly terminationGraceMs: number
  readonly terminationSignal?: AbortSignal
  readonly platform?: NodeJS.Platform
  readonly capture?: Partial<OwnedProcessCaptureLimits>
}

/** Pure framed, request-bound oracle used by protocol-vector tests. */
export function parseTestProcessOwnerStartEvidenceFrameForRequest(
  bytes: Uint8Array,
  request: Readonly<{
    run_id: string
    operation_id: string
    scenario: string
  }>,
  platform: 'win32' | 'linux',
): TestProcessOwnerStartEvidence | undefined

/** Encodes one exact request-bound decision frame for protocol vectors and live transport. */
export function encodeTestProcessOwnerStartDecisionFrame(
  evidence: TestProcessOwnerStartEvidence,
  decision: TestProcessOwnerStartDecision,
): Uint8Array

/** Pure request-bound oracle used by protocol-vector tests. */
export function parseTestProcessOwnerSettlementForRequest(
  value: unknown,
  request: Readonly<{
    run_id: string
    operation_id: string
    scenario: string
    command: Readonly<{ stdin: null | Readonly<{ byte_length: number }> }>
  }>,
  platform: 'win32' | 'linux',
): Readonly<Record<string, unknown>>

export function testProcessOwnerFailureEvidence(
  value: unknown,
): TestProcessOwnerFailureEvidence | undefined

export function executeTestProcessOwner(
  options: ExecuteTestProcessOwnerOptions,
): Promise<TestProcessOwnerExecution>

export function startTestProcessOwner(
  options: ExecuteTestProcessOwnerOptions,
): Promise<TestProcessOwnerRun>
import type {
  OwnedByteChannel,
  OwnedByteSnapshot,
  OwnedEventChannel,
  OwnedEventSnapshot,
  OwnedProcessCaptureLimits,
} from './owned-process-channel.mjs'
