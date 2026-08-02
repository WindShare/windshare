import type { OwnedEventSnapshot } from '../../process/owned-process-channel.mjs'
import type { GuardExecutionLease } from '../../execution/guard-execution-lease.ts'
import type { BrowserSampleResult } from '../../result.ts'
import type { BrowserEngine } from '../../vocabulary.ts'
import type { GuardUploadDirectoryPublisher } from '../directory-publisher.ts'
import type { ArtifactGuardResult, GuardMatchEvidence } from '../guard-result.ts'
import type {
  GuardUploadFaultCut,
  GuardUploadSelection,
  GuardUploadTopologySnapshots,
} from '../sealed-suite.ts'
import type { ProcessSettlementTrustAnchor } from '../settlement-receipt.ts'

export const ARTIFACT_GUARD_TRACE_SCHEMA_VERSION = 'windshare.artifact-guard-trace/v1' as const
export const ARTIFACT_GUARD_SCAN_FAULT_ACTIONS = Object.freeze([
  'fail-before-artifact-scan',
  'replace-artifact-before-scan',
] as const)

export interface ExplicitGuardSecret {
  readonly value: string
}

/**
 * Fault cuts are declarative so tests can select a frozen failure boundary without
 * receiving executable authority inside the production scan transaction.
 */
export type ArtifactGuardScanFaultCut =
  | Readonly<{
      action: 'fail-before-artifact-scan'
      relativePath: string
    }>
  | Readonly<{
      action: 'replace-artifact-before-scan'
      relativePath: string
      replacementUtf8: string
    }>

export interface ArtifactGuardSuiteScanFaultCut {
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly fault: ArtifactGuardScanFaultCut
}

export interface GuardArtifactSuiteSample {
  readonly sample: BrowserSampleResult
  readonly sampleResultBytes: Uint8Array
  readonly artifactRoot: string
  readonly commandSha256: string
  readonly settlementAttestation: unknown
}

export interface GuardArtifactSuiteOptions {
  readonly runId: string
  readonly runPolicy: BrowserSampleResult['runPolicy']
  readonly suite: BrowserSampleResult['suite']
  readonly checkoutSha: string
  readonly samples: readonly GuardArtifactSuiteSample[]
  readonly uploadParent: string
  readonly topology: GuardUploadTopologySnapshots
  readonly settlementTrust: ProcessSettlementTrustAnchor
  readonly directoryPublisher: GuardUploadDirectoryPublisher
  readonly executionLease?: GuardExecutionLease
  readonly explicitSecrets: readonly ExplicitGuardSecret[]
  readonly scanFaultCuts?: readonly ArtifactGuardSuiteScanFaultCut[]
  readonly uploadFaultCuts?: readonly GuardUploadFaultCut[]
}

export interface GuardArtifactSuiteOutcome {
  readonly guards: readonly ArtifactGuardResult[]
  readonly upload: GuardUploadSelection | null
}

export interface GuardArtifactSuiteResult extends GuardArtifactSuiteOutcome {
  readonly traces: ArtifactGuardTraceSnapshot
}

export interface GuardArtifactSuiteExecution {
  readonly result: Promise<GuardArtifactSuiteOutcome>
  readonly traces: ArtifactGuardTraceChannel
}

export interface ScanSampleArtifactsOptions {
  readonly sample: BrowserSampleResult
  readonly sampleResultBytes: Uint8Array
  readonly artifactRoot: string
  readonly explicitSecrets: readonly ExplicitGuardSecret[]
  readonly faultCut?: ArtifactGuardScanFaultCut
  readonly executionLease?: GuardExecutionLease
}

export interface ScanSampleArtifactsExecution {
  readonly result: Promise<ArtifactGuardResult>
  readonly traces: ArtifactGuardTraceChannel
}

export type ArtifactGuardTraceScenario = 'guard-suite' | 'artifact-scan'
export type ArtifactGuardTraceOutcome = 'started' | 'succeeded' | 'failed' | 'blocked'
export type ArtifactGuardTraceContextValue = string | number | boolean | null

export interface ArtifactGuardTraceIdentity {
  readonly operationId: string
  readonly runId: string
  readonly scenario: ArtifactGuardTraceScenario
  readonly suite: BrowserSampleResult['suite']
  readonly browser?: BrowserSampleResult['browser']
  readonly sampleIndex?: BrowserSampleResult['sampleIndex']
}

export interface ArtifactGuardTraceEvent extends ArtifactGuardTraceIdentity {
  readonly schemaVersion: typeof ARTIFACT_GUARD_TRACE_SCHEMA_VERSION
  readonly component: 'artifact-guard'
  readonly milestone: string
  readonly outcome: ArtifactGuardTraceOutcome
  readonly context: Readonly<Record<string, ArtifactGuardTraceContextValue>>
}

export interface ArtifactGuardTraceFailure {
  readonly name: string
  readonly message: string
}

export interface ArtifactGuardTraceSnapshot extends OwnedEventSnapshot<ArtifactGuardTraceEvent> {
  readonly observedBytes: number
  readonly capturedBytes: number
  readonly failure: ArtifactGuardTraceFailure | null
}

export interface ArtifactGuardTraceChannel extends AsyncIterable<ArtifactGuardTraceEvent> {
  snapshot(): ArtifactGuardTraceSnapshot
}

export interface ScanState {
  scannedFileCount: number
  scannedArchiveEntryCount: number
  observedArchiveBytes: number
  expandedArchiveBytes: number
  observedMaximumArchiveDepth: number
  readonly matches: GuardMatchEvidence[]
}

export type GuardFailureCode = NonNullable<ArtifactGuardResult['failureCode']>

const OWNED_GUARD_FAILURES = new WeakSet<object>()

export class GuardFailure extends Error {
  readonly code: GuardFailureCode

  constructor(code: GuardFailureCode, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'GuardFailure'
    this.code = code
    OWNED_GUARD_FAILURES.add(this)
    Object.freeze(this)
  }
}

export function isOwnedGuardFailure(value: unknown): value is GuardFailure {
  return (typeof value === 'object' || typeof value === 'function') &&
    value !== null &&
    OWNED_GUARD_FAILURES.has(value)
}

export class ArtifactGuardRecordedError extends Error {
  readonly traces: ArtifactGuardTraceSnapshot

  constructor(message: string, traces: ArtifactGuardTraceSnapshot, cause: unknown) {
    super(message, { cause })
    this.name = 'ArtifactGuardRecordedError'
    this.traces = traces
  }
}
