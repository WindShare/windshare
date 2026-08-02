import type { BrowserSampleStagingFaultCut } from '../process/attachment-staging.ts'
import type {
  BrowserSampleContainmentBackend,
  ContainedSampleCommand,
  InheritedProcessAuthority,
} from '../process/containment.ts'
import type { TestProcessOwnerArtifact } from '../process/test-process-owner-client.mjs'
import type { OwnedEventChannel } from '../process/owned-process-channel.mjs'
import type { BrowserSampleResult } from '../result.ts'
import type { BrowserRunPolicy } from '../run-policy.ts'
import type { VerifiedTestIceTopologyLock } from '../test-ice-topology.ts'
import type { BrowserEngine, BrowserSuite } from '../vocabulary.ts'

export const RUNNER_MAXIMUM_CAPTURED_STREAM_BYTES = 16_777_216 as const
export const RUNNER_SAMPLE_PROCESS_DEADLINE_MS = 1_200_000 as const
export const RUNNER_PROCESS_TERMINATION_GRACE_MS = 5_000 as const
export const BROWSER_SAMPLE_TRACE_SCHEMA_VERSION =
  'windshare.browser-sample-trace/v1' as const

export interface BrowserSampleIdentity {
  readonly runId: string
  readonly operationId: string
  readonly scenario: string
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
  readonly processOwner?: TestProcessOwnerArtifact
  readonly readOnlyInputRoots?: readonly string[]
  readonly containmentBackend?: BrowserSampleContainmentBackend
  /**
   * `inherited` is valid only when this runner is the sole child of an external
   * subreaper/Job. It also defers terminal result persistence to that owner.
   */
  readonly ownershipMode?: 'owned' | 'inherited'
  readonly outerProcessAuthority?: InheritedProcessAuthority
  readonly stagingFaultCut?: BrowserSampleStagingFaultCut
}

export interface BrowserSampleTrace {
  readonly schemaVersion: typeof BROWSER_SAMPLE_TRACE_SCHEMA_VERSION
  readonly component: 'browser-evidence-runner'
  readonly runId: string
  readonly operationId: string
  readonly scenario: string
  readonly outcome: 'started' | 'succeeded' | 'failed'
  readonly milestone: string
  readonly suite: BrowserSuite
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly context?: Readonly<Record<string, unknown>>
}

export interface BrowserSampleRunOutcome {
  readonly result: BrowserSampleResult
  readonly resultPath: string
  readonly sampleDirectory: string
  readonly artifactRoot: string
  readonly acceptedBeforeGuard: boolean
}

export interface BrowserSampleRunExecution {
  readonly result: Promise<BrowserSampleRunOutcome>
  readonly traces: OwnedEventChannel<BrowserSampleTrace>
}
