import type { ChildEvidenceContext } from '../child-evidence.ts'
import type { RunnerProcessEvidence } from '../execution-evidence.ts'

export interface ContainedSampleCommand {
  readonly executable: string
  readonly arguments: readonly string[]
  readonly cwd?: string
  readonly environment?: Readonly<Record<string, string>>
  /** Delivered once over the child's anonymous standard-input pipe; never serialized to process metadata. */
  readonly stdin?: Uint8Array
  /** Nonsecret correlation scope authenticated independently from the raw pipe bytes. */
  readonly stdinAuthority?: ContainedSampleStdinAuthority
}

export interface ContainedSampleStdinAuthority {
  readonly channelId: string
  readonly runId: string
  readonly profileId: string
  readonly attemptId: string
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
  readonly stdout: (chunk: Uint8Array) => void
  readonly stderr: (chunk: Uint8Array) => void
  readonly trace: BrowserSampleContainmentTraceSink
}

export interface BrowserSampleContainmentTrace {
  readonly milestone: string
  readonly context?: Readonly<Record<string, unknown>>
}

export type BrowserSampleContainmentTraceSink = (trace: BrowserSampleContainmentTrace) => void

export interface BrowserSampleContainmentExecution {
  readonly processEvidence: RunnerProcessEvidence
  readonly timedOut: boolean
}

export interface BrowserSampleContainmentBackend<
  Preflight extends BrowserSampleContainmentPreflight = BrowserSampleContainmentPreflight,
> {
  readonly kind: 'windows-job' | 'linux-process-owner' | 'native-process-group' | 'inherited' | 'test'
  preflight(request: Preflight): Promise<void>
  execute(
    request: BrowserSampleContainmentRequest & Preflight,
  ): Promise<BrowserSampleContainmentExecution>
}
