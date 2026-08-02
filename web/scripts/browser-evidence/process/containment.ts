import type { ChildEvidenceContext } from '../child-evidence.ts'
import type { RunnerProcessEvidence } from '../execution-evidence.ts'
import type { OwnedByteSnapshot, OwnedEventSnapshot } from './owned-process-channel.mjs'

export interface ContainedSampleCommand {
  readonly executable: string
  readonly arguments: readonly string[]
  readonly cwd?: string
  readonly environment?: Readonly<Record<string, string>>
  /** Delivered once over the child's anonymous standard-input pipe; never serialized to process metadata. */
  readonly stdin?: Uint8Array
}

export interface BrowserSampleContainmentPreflight {
  readonly operationId: string
  readonly topologyProfilePath: string
  readonly topologyProfileSha256: string
  readonly topologyResolutionPath: string
  readonly topologyResolutionSha256: string
  readonly readOnlyInputRoots: readonly string[]
  /** External owners may request immediate backend retirement independently of its deadline. */
  readonly terminationSignal?: AbortSignal
}

export interface BrowserSampleContainmentRequest extends BrowserSampleContainmentPreflight {
  readonly command: ContainedSampleCommand
  readonly sampleDirectory: string
  readonly childAttachmentStagingRoot: string
  readonly childContext: ChildEvidenceContext
  readonly deadlineMs: number
  readonly terminationGraceMs: number
  readonly capture: Readonly<{
    readonly stdoutBytes: number
    readonly stderrBytes: number
  }>
}

export interface BrowserSampleContainmentTrace {
  readonly milestone: string
  readonly outcome: 'started' | 'succeeded' | 'failed'
  readonly context?: Readonly<Record<string, unknown>>
}

export type BrowserSampleContainmentTerminationReason =
  | 'natural'
  | 'deadline'
  | 'stop'
  | 'parent_lost'
  | 'initialization_failed'
  | 'start_rejected'
  | 'owner_failure'

export interface InheritedProcessAuthority {
  readonly kind: 'test-process-owner'
  readonly backend: 'windows_job' | 'linux_subreaper'
  readonly operationId: string
}

export interface BrowserSampleContainmentExecution {
  readonly processEvidence: RunnerProcessEvidence
  readonly terminationReason: BrowserSampleContainmentTerminationReason
  readonly treeEmpty?: boolean
  readonly cleanupOutcome?: 'completed' | 'failed'
  readonly inputEvidence?: unknown
  readonly ownershipEvidence?: unknown
  readonly output: Readonly<{
    readonly stdout: OwnedByteSnapshot
    readonly stderr: OwnedByteSnapshot
  }>
  readonly traces: OwnedEventSnapshot<BrowserSampleContainmentTrace>
}

export class BrowserSampleContainmentError extends Error {
  readonly output?: BrowserSampleContainmentExecution['output']
  readonly traces: BrowserSampleContainmentExecution['traces']

  constructor(
    message: string,
    traces: BrowserSampleContainmentExecution['traces'],
    output: BrowserSampleContainmentExecution['output'] | undefined,
    cause: unknown,
  ) {
    super(message, { cause })
    this.name = 'BrowserSampleContainmentError'
    this.traces = traces
    if (output !== undefined) this.output = output
  }
}

export interface BrowserSampleContainmentBackend<
  Preflight extends BrowserSampleContainmentPreflight = BrowserSampleContainmentPreflight,
> {
  readonly kind: 'test-process-owner' | 'inherited' | 'test'
  readonly outerAuthority?: InheritedProcessAuthority
  preflight(request: Preflight): Promise<void>
  execute(
    request: BrowserSampleContainmentRequest & Preflight,
  ): Promise<BrowserSampleContainmentExecution>
}
