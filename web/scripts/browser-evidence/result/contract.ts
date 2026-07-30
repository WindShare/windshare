import type { BrowserSelectedPairEvidence, PionSelectedPairEvidence } from '../attempt-evidence.ts'
import type { LogicalAttempt } from '../attempt-collector.ts'
import type { CapabilityEvidence } from '../capability.ts'
import type { ExecutionEvidence } from '../execution-evidence.ts'
import type { MainRouteEvidence } from '../route-evidence.ts'
import type { BrowserRunPolicy } from '../run-policy.ts'
import { PR_TEST_ICE_TOPOLOGY_ID } from '../test-ice-topology.ts'
import {
  BROWSER_EVIDENCE_SCHEMA_VERSION,
  type BrowserEngine,
  type DeliveryOutcome,
  type ExecutionOutcome,
  type PeerAttemptOutcome,
  type ResultStatus,
  type RtcCapability,
} from '../vocabulary.ts'

export const PLAYWRIGHT_OUTCOMES = Object.freeze(['not-started', 'passed', 'failed'] as const)
export const PION_APPLICABILITY = Object.freeze(['unknown', 'applicable', 'not-applicable'] as const)
export const NATIVE_INTEROP_OUTCOMES = Object.freeze(['not-started', 'succeeded', 'failed'] as const)
export const NATIVE_INTEROP_FAILURE_CODES = Object.freeze([
  'peer-construction',
  'negotiation',
  'datachannel',
  'interop-deadline',
  'selected-pair',
  'protocol',
  'unexpected',
] as const)
export const DELIVERY_TERMINALS = Object.freeze(['succeeded', 'failed'] as const)
export const MAIN_TRANSFER_BYTES = 16_777_216 as const
export const MAIN_TRANSFER_SHA256 =
  '25e349f1212bb99491944eb8e885665bb71edc5d5db49d1cd2ef1ffafac1dd5d' as const
export const ARTIFACT_KINDS = Object.freeze([
  'trace',
  'video',
  'screenshot',
  'error-context',
  'console-log',
  'runner-stdout',
  'runner-stderr',
  'process-log',
  'attempt-evidence',
  'native-interop-evidence',
  'result-diagnostic',
] as const)

export type PlaywrightOutcome = (typeof PLAYWRIGHT_OUTCOMES)[number]
export type PionApplicability = (typeof PION_APPLICABILITY)[number]
export type NativeInteropOutcome = (typeof NATIVE_INTEROP_OUTCOMES)[number]
export type NativeInteropFailureCode = (typeof NATIVE_INTEROP_FAILURE_CODES)[number]
export type ArtifactKind = (typeof ARTIFACT_KINDS)[number]

export interface ArtifactIndexEntry {
  readonly artifactId: string
  readonly kind: ArtifactKind
  readonly relativePath: string
  readonly mediaType: string
  readonly byteLength: number
  readonly sha256: string
}

export interface DeliveryEvidence {
  readonly expectedBytes: number
  readonly receivedBytes: number
  readonly expectedSha256: string
  readonly receivedSha256: string | null
  readonly terminal: (typeof DELIVERY_TERMINALS)[number]
}

export interface NativeInteropEvidence {
  readonly browser: NativeInteropSideEvidence<BrowserSelectedPairEvidence>
  readonly pion: NativeInteropSideEvidence<PionSelectedPairEvidence>
  readonly failureCode?: NativeInteropFailureCode
  readonly failureMessage?: string
}

export interface NativeInteropSideEvidence<TSelectedPair> {
  readonly attemptId: string
  readonly selectedPair: TSelectedPair | null
}

export interface SampleResultCommon {
  readonly schemaVersion: typeof BROWSER_EVIDENCE_SCHEMA_VERSION
  readonly resultStatus: ResultStatus
  readonly runId: string
  readonly runPolicy: BrowserRunPolicy
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly checkoutSha: string
  readonly topologyId: typeof PR_TEST_ICE_TOPOLOGY_ID
  readonly topologyProfileSha256: string
  readonly topologyResolutionSha256: string
  readonly rtcCapability: RtcCapability
  readonly capabilityEvidence: CapabilityEvidence
  readonly executionOutcome: ExecutionOutcome
  readonly executionEvidence: ExecutionEvidence
  readonly playwrightOutcome: PlaywrightOutcome
  readonly artifacts: readonly ArtifactIndexEntry[]
  readonly integrityViolations: readonly string[]
}

export interface MainBrowserSampleResult extends SampleResultCommon {
  readonly suite: 'main'
  readonly peerAttemptOutcome: PeerAttemptOutcome
  readonly deliveryOutcome: DeliveryOutcome
  readonly attempts: readonly LogicalAttempt[]
  readonly deliveryEvidence: DeliveryEvidence | null
  readonly routeEvidence: MainRouteEvidence | null
}

export interface PionBrowserSampleResult extends SampleResultCommon {
  readonly suite: 'pion'
  readonly applicability: PionApplicability
  readonly nativeInteropOutcome: NativeInteropOutcome
  readonly nativeInteropEvidence: NativeInteropEvidence | null
}

export type BrowserSampleResult = MainBrowserSampleResult | PionBrowserSampleResult
export type PionAcceptanceDisposition = 'accepted' | 'requires-main-relay-fallback'
