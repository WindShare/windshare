import type { GuardExecutionLease } from '../../execution/guard-execution-lease.ts'
import type { ArtifactIndexEntry, BrowserSampleResult } from '../../result.ts'
import type { GuardUploadDirectoryPublisher } from '../directory-publisher.ts'
import type { ArtifactGuardResult, GuardMatchEvidence } from '../guard-result.ts'
import type {
  GuardUploadHooks,
  GuardUploadSelection,
  GuardUploadTopologySnapshots,
} from '../sealed-suite.ts'
import type { ProcessSettlementTrustAnchor } from '../settlement-receipt.ts'

export interface ExplicitGuardSecret {
  readonly value: string
}

export interface ArtifactGuardScanHooks {
  readonly beforeArtifactScan?: (artifact: ArtifactIndexEntry) => void | Promise<void>
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
  readonly hooks?: ArtifactGuardSuiteHooks
  readonly trace?: ArtifactGuardTraceSink
}

export interface ArtifactGuardSuiteHooks {
  readonly beforeArtifactScan?: (
    sample: BrowserSampleResult,
    artifact: ArtifactIndexEntry,
  ) => void | Promise<void>
  readonly upload?: GuardUploadHooks
}

export interface GuardArtifactSuiteResult {
  readonly guards: readonly ArtifactGuardResult[]
  readonly upload: GuardUploadSelection | null
}

export interface ScanSampleArtifactsOptions extends ArtifactGuardScanHooks {
  readonly sample: BrowserSampleResult
  readonly sampleResultBytes: Uint8Array
  readonly artifactRoot: string
  readonly explicitSecrets: readonly ExplicitGuardSecret[]
  readonly trace?: ArtifactGuardTraceSink
  readonly executionLease?: GuardExecutionLease
}

export interface ArtifactGuardTraceEvent {
  readonly component: 'artifact-guard'
  readonly operationId: string
  readonly milestone: string
  readonly context: Readonly<Record<string, string | number | boolean | null>>
}

export type ArtifactGuardTraceSink = (event: ArtifactGuardTraceEvent) => void

export interface ScanState {
  scannedFileCount: number
  scannedArchiveEntryCount: number
  observedArchiveBytes: number
  expandedArchiveBytes: number
  observedMaximumArchiveDepth: number
  readonly matches: GuardMatchEvidence[]
}

export type GuardFailureCode = NonNullable<ArtifactGuardResult['failureCode']>

export class GuardFailure extends Error {
  readonly code: GuardFailureCode

  constructor(code: GuardFailureCode, message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'GuardFailure'
    this.code = code
  }
}
