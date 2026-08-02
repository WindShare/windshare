import type { BrowserSampleResult } from '../result.ts'
import type { BrowserRunPolicy } from '../run-policy.ts'
import type { BrowserEngine, BrowserSuite } from '../vocabulary.ts'

export const PROCESS_SETTLEMENT_SCHEMA_VERSION =
  'windshare.process-settlement/v5' as const
export const PROCESS_SETTLEMENT_MAXIMUM_LIFETIME_MS = 21_600_000 as const
export const PROCESS_SETTLEMENT_CLOCK_SKEW_MS = 300_000 as const

export type ProcessSettlementTerminal = 'exited' | 'signaled' | 'spawn-failed'
export type ProcessSettlementCleanupOutcome = 'completed' | 'failed'

export interface ProcessSettlementInputEvidence {
  readonly outcome: 'not_started' | 'not_requested' | 'delivered' | 'failed'
  readonly failureCode: string
  readonly failureMessage: string
}

export interface ProcessSettlementOwnershipEvidence {
  readonly kind: 'test-process-owner'
  readonly backend: 'linux_subreaper' | 'windows_job'
  readonly terminationReason:
    | 'natural'
    | 'deadline'
    | 'stop'
    | 'parent_lost'
    | 'initialization_failed'
    | 'start_rejected'
    | 'owner_failure'
}

export type ProcessSettlementEvidence =
  | {
      readonly terminal: 'exited'
      readonly exitCode: number
    }
  | {
      readonly terminal: 'signaled'
      readonly signal: string
    }
  | {
      readonly terminal: 'spawn-failed'
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
  readonly resultSha256: string
  readonly resultByteLength: string
  readonly process: ProcessSettlementEvidence
  readonly treeEmpty: boolean
  readonly cleanupOutcome: ProcessSettlementCleanupOutcome
  readonly input: ProcessSettlementInputEvidence
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
