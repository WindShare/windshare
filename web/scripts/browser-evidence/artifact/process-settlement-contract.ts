import type { BrowserSampleResult } from '../result.ts'
import type { BrowserRunPolicy } from '../run-policy.ts'
import type { BrowserEngine, BrowserSuite } from '../vocabulary.ts'

export const PROCESS_SETTLEMENT_SCHEMA_VERSION =
  'windshare.process-settlement/v2' as const
export const PROCESS_SETTLEMENT_MAXIMUM_LIFETIME_MS = 21_600_000 as const
export const PROCESS_SETTLEMENT_CLOCK_SKEW_MS = 300_000 as const

export type ProcessSettlementTerminal = 'exited' | 'signaled' | 'spawn-failed'
export type ProcessSettlementCleanupOutcome = 'completed' | 'failed'

export interface ProcessSettlementInputEvidence {
  readonly outcome: 'not-started' | 'not-requested' | 'delivered' | 'failed'
  readonly failureCode: string
  readonly failureMessage: string
}

export interface ProcessSettlementClientIoEvidence {
  readonly requestOutcome: 'delivered' | 'failed'
  readonly rawInputOutcome: 'not-requested' | 'delivered' | 'failed'
  readonly controlOutcome: 'not-requested' | 'delivered' | 'failed'
  readonly outputOutcome: 'delivered' | 'failed'
  readonly failureCode: string
  readonly failureMessage: string
}

export type ProcessSettlementOwnershipEvidence =
  | {
      readonly backend: 'linux-subreaper'
      readonly ownerPid: number
      readonly rootPid: number | null
      readonly rootStartTimeTicks: string
      readonly inventoryScans: number
      readonly maximumObservedDescendants: number
      readonly quietInventoryCount: number
      readonly controlOutcome: string
      readonly cleanupOutcome: ProcessSettlementCleanupOutcome
      readonly failureCode: string
      readonly failureMessage: string
    }
  | {
      readonly backend: 'windows-job'
      readonly supervisionOutcome: 'tree-empty' | 'spawn-failed'
      readonly terminationReason: 'natural' | 'target-spawn-failed' | 'deadline' | 'parent-request'
      readonly activeProcessCount: 0
      readonly root: { readonly pid: number; readonly exitCode: number } | null
      readonly spawnFailure: string | null
    }

export type ProcessSettlementEvidence =
  | {
      readonly terminal: 'exited'
      readonly timedOut: boolean
      readonly exitCode: number
    }
  | {
      readonly terminal: 'signaled'
      readonly timedOut: boolean
      readonly signal: string
    }
  | {
      readonly terminal: 'spawn-failed'
      readonly timedOut: boolean
      readonly errorCode: string
      readonly errorMessage: string
    }

export interface ProcessSettlementPayload {
  readonly schemaVersion: typeof PROCESS_SETTLEMENT_SCHEMA_VERSION
  readonly invocationId: string
  readonly sampleId: string
  readonly runId: string
  readonly runPolicy: BrowserRunPolicy
  readonly suite: BrowserSuite
  readonly browser: BrowserEngine
  readonly sampleIndex: number
  readonly checkoutSha: string
  readonly commandSha256: string
  readonly runtimeManifestSha256: string
  readonly resultSha256: string
  readonly resultByteLength: string
  readonly process: ProcessSettlementEvidence
  readonly launched: boolean
  readonly treeEmpty: boolean
  readonly input: ProcessSettlementInputEvidence
  readonly clientIo: ProcessSettlementClientIoEvidence
  readonly ownership: ProcessSettlementOwnershipEvidence
  readonly nonce: string
  readonly issuedAtUnixMs: string
  readonly expiresAtUnixMs: string
}

export interface ProcessSettlementAttestation {
  readonly payload: ProcessSettlementPayload
  readonly signatureBase64: string
}

export interface ProcessSettlementTrustAnchor {
  readonly invocationId: string
  readonly runtimeManifestSha256: string
  readonly publicKeySpkiBase64: string
  readonly publicKeySha256: string
}

export interface ProcessSettlementSampleExpectation {
  readonly sample: Pick<
    BrowserSampleResult,
    'runId' | 'runPolicy' | 'suite' | 'browser' | 'sampleIndex' | 'checkoutSha'
  >
  readonly resultBytes: Uint8Array
  readonly commandSha256: string
}

export interface VerifyProcessSettlementOptions {
  readonly trust: ProcessSettlementTrustAnchor
  readonly samples: readonly ProcessSettlementSampleExpectation[]
  readonly attestations: readonly unknown[]
  readonly nowUnixMs?: number
}

declare const VERIFIED_PROCESS_SETTLEMENT_BRAND: unique symbol

export interface VerifiedProcessSettlementSet {
  readonly invocationId: string
  readonly sampleKeys: readonly string[]
  readonly inventorySha256: string
  readonly [VERIFIED_PROCESS_SETTLEMENT_BRAND]: true
}
