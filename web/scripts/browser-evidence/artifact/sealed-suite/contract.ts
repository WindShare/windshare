import type { GuardExecutionLease } from '../../execution/guard-execution-lease.ts'
import type { ArtifactIndexEntry, BrowserSampleResult } from '../../result.ts'
import type { BrowserRunPolicy } from '../../run-policy.ts'
import type { BrowserEngine, BrowserSuite } from '../../vocabulary.ts'
import type { GuardUploadDirectoryPublisher } from '../directory-publisher.ts'
import type { ArtifactGuardResult } from '../guard-result.ts'
import type { VerifiedProcessSettlementSet } from '../settlement-receipt.ts'

export const GUARD_UPLOAD_MANIFEST_SCHEMA_VERSION = 2 as const
export const GUARD_UPLOAD_MANIFEST_FILENAME = 'manifest.json' as const
export const GUARD_UPLOAD_RESULT_FILENAME = 'result.json' as const
export const GUARD_UPLOAD_GUARD_FILENAME = 'guard.json' as const
export const GUARD_UPLOAD_SAMPLES_DIRECTORY = 'samples' as const
export const GUARD_UPLOAD_ATTACHMENTS_DIRECTORY = 'attachments' as const
export const GUARD_UPLOAD_TOPOLOGY_DIRECTORY = 'topology' as const
export const GUARD_UPLOAD_TOPOLOGY_PROFILE_PATH = 'topology/profile.json' as const
export const GUARD_UPLOAD_TOPOLOGY_RESOLUTION_PATH = 'topology/resolution.json' as const
export const GUARD_UPLOAD_OUTPUT_NAME = 'sealed' as const
export const GUARD_UPLOAD_FAULT_ACTIONS = Object.freeze([
  'add-foreign-file-before-publication',
  'fail-before-artifact-copy',
] as const)

export const MAXIMUM_UPLOAD_MANIFEST_BYTES = 8 * 1_024 * 1_024
export const MAXIMUM_SAMPLE_RESULT_BYTES = 16 * 1_024 * 1_024
export const MAXIMUM_GUARD_RESULT_BYTES = 1 * 1_024 * 1_024
export const MAXIMUM_TOPOLOGY_BYTES = 1 * 1_024 * 1_024

export interface GuardUploadFileAuthority {
  readonly relativePath: string
  readonly byteLength: string
  readonly sha256: string
}

export interface GuardUploadTopologyManifest {
  readonly profile: GuardUploadFileAuthority & {
    readonly relativePath: typeof GUARD_UPLOAD_TOPOLOGY_PROFILE_PATH
  }
  readonly resolution: GuardUploadFileAuthority & {
    readonly relativePath: typeof GUARD_UPLOAD_TOPOLOGY_RESOLUTION_PATH
  }
}

export interface GuardUploadArtifactManifest {
  readonly artifactId: string
  readonly kind: ArtifactIndexEntry['kind']
  readonly relativePath: string
  readonly mediaType: string
  readonly byteLength: string
  readonly sha256: string
}

export interface GuardUploadManifest {
  readonly schemaVersion: typeof GUARD_UPLOAD_MANIFEST_SCHEMA_VERSION
  readonly runId: string
  readonly runPolicy: BrowserRunPolicy
  readonly suite: BrowserSuite
  readonly checkoutSha: string
  readonly topology: GuardUploadTopologyManifest
  readonly samples: readonly GuardUploadSampleManifest[]
}

export interface GuardUploadSampleManifest {
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly sampleResultByteLength: string
  readonly sampleResultSha256: string
  readonly guardResultByteLength: string
  readonly guardResultSha256: string
  readonly artifactManifestSha256: string
  readonly artifacts: readonly GuardUploadArtifactManifest[]
}

export interface GuardUploadSelection {
  readonly uploadDirectory: string
  readonly manifestSha256: string
  readonly manifestByteLength: string
  readonly manifest: GuardUploadManifest
  readonly topologySnapshots: GuardUploadTopologySnapshots
  readonly guards: readonly ArtifactGuardResult[]
  readonly sampleSnapshots: readonly GuardUploadSampleSnapshot[]
}

export interface GuardUploadTopologySnapshots {
  readonly profileBytes: Uint8Array
  readonly resolutionBytes: Uint8Array
}

export interface GuardUploadSampleSnapshot {
  readonly manifest: GuardUploadSampleManifest
  readonly resultBytes: Uint8Array
  readonly guardBytes: Uint8Array
  readonly guard: ArtifactGuardResult
}

export interface GuardUploadSampleContractPaths {
  readonly sampleDirectory: string
  readonly resultPath: string
  readonly guardPath: string
  readonly attachmentsDirectory: string
}

export interface GuardUploadSampleInput {
  readonly sample: BrowserSampleResult
  readonly sampleResultBytes: Uint8Array
  readonly artifactRoot: string
  readonly guard: ArtifactGuardResult
  readonly commandSha256: string
}

export type GuardUploadFaultCut =
  | Readonly<{
      action: 'add-foreign-file-before-publication'
    }>
  | Readonly<{
      action: 'fail-before-artifact-copy'
      browser: BrowserEngine
      sampleIndex: number
      relativePath: string
    }>

export interface SealGuardUploadSuiteOptions {
  readonly uploadParent: string
  readonly runId: string
  readonly runPolicy: BrowserRunPolicy
  readonly suite: BrowserSuite
  readonly checkoutSha: string
  readonly samples: readonly GuardUploadSampleInput[]
  readonly topology: GuardUploadTopologySnapshots
  readonly settlement: VerifiedProcessSettlementSet
  readonly settlementInvocationId: string
  readonly directoryPublisher: GuardUploadDirectoryPublisher
  readonly executionLease?: GuardExecutionLease
  readonly faultCuts?: readonly GuardUploadFaultCut[]
}
