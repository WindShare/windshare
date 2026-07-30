import type { BrowserSampleStagingHooks } from '../process/attachment-staging.ts'
import type {
  BrowserSampleContainmentBackend,
  ContainedSampleCommand,
} from '../process/containment.ts'
import type { LinuxProcessOwnerArtifact } from '../process/linux-process-owner-client.ts'
import type { BrowserSampleResult } from '../result.ts'
import type { BrowserRunPolicy } from '../run-policy.ts'
import type { VerifiedTestIceTopologyLock } from '../test-ice-topology.ts'
import type { BrowserEngine, BrowserSuite } from '../vocabulary.ts'

export const RUNNER_MAXIMUM_CAPTURED_STREAM_BYTES = 16_777_216 as const
export const RUNNER_SAMPLE_PROCESS_DEADLINE_MS = 1_200_000 as const
export const RUNNER_PROCESS_TERMINATION_GRACE_MS = 5_000 as const

export interface BrowserSampleIdentity {
  readonly runId: string
  readonly runPolicy: BrowserRunPolicy
  readonly suite: BrowserSuite
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly checkoutSha: string
}

export type BrowserSampleCommand = ContainedSampleCommand

export interface BrowserSampleRunnerOptions extends BrowserSampleIdentity {
  readonly sampleDirectory: string
  readonly topologyLock: VerifiedTestIceTopologyLock
  readonly topologyProfilePath: string
  readonly topologyResolutionPath: string
  readonly command: BrowserSampleCommand
  readonly maximumCapturedStreamBytes?: number
  readonly processDeadlineMs?: number
  readonly windowsJobHelperPath?: string
  readonly linuxProcessOwner?: LinuxProcessOwnerArtifact
  readonly readOnlyInputRoots?: readonly string[]
  readonly containmentBackend?: BrowserSampleContainmentBackend
  /**
   * `inherited` is valid only when this runner is the sole child of an external
   * subreaper/Job. It also defers terminal result persistence to that owner.
   */
  readonly ownershipMode?: 'owned' | 'inherited'
  readonly stagingHooks?: BrowserSampleStagingHooks
  readonly trace?: BrowserSampleTraceSink
}

export interface BrowserSampleTrace {
  readonly operationId: string
  readonly milestone: string
  readonly suite: BrowserSuite
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly context?: Readonly<Record<string, unknown>>
}

export type BrowserSampleTraceSink = (trace: BrowserSampleTrace) => void

export interface BrowserSampleRunOutcome {
  readonly result: BrowserSampleResult
  readonly resultPath: string
  readonly sampleDirectory: string
  readonly artifactRoot: string
  readonly acceptedBeforeGuard: boolean
}
