import type { FileCheckpointV2 } from '../persistence/checkpoint'
import type { DurableCheckpointNamespaceIdentity } from '../persistence/namespace'

export const BROWSER_SAVE_POLICY_VERSION = 1 as const
export const BROWSER_DELIVERY_RECORD_VERSION = 1 as const
export const BROWSER_DELIVERY_PAGE_LIMIT = 128

export type BrowserRecoveryPreference = 'automatic' | 'direct'
export type BrowserFilePlacement = 'direct' | 'staged'

/** Receiving storage is browser-local authority; the receive intent still describes the final target. */
export interface BrowserSavePolicyV1 {
  readonly schemaVersion: typeof BROWSER_SAVE_POLICY_VERSION
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly preference: BrowserRecoveryPreference
  readonly target: DurableCheckpointNamespaceIdentity
  readonly staging?: DurableCheckpointNamespaceIdentity
  readonly digest: string
}

export interface BrowserDeliverySource {
  readonly fileId: string
  readonly fileRevision: string
  readonly canonicalPath: readonly string[]
  readonly exactSize: bigint
}

export interface BrowserDeliveryLocalMutation {
  readonly lifecycleGeneration: bigint
  readonly checkpointSetDigest: string
  readonly priorTargetCheckpoint?: FileCheckpointV2
}

export interface BrowserDeliveryCopyAttempt {
  readonly attemptId: string
  readonly targetOwnedObjectId?: string
}

export type BrowserDeliveryState =
  | Readonly<{ kind: 'receiving'; checkpoint?: FileCheckpointV2 }>
  | Readonly<{ kind: 'restart-authorized'; checkpoint: FileCheckpointV2; authorizationId: string }>
  | Readonly<{ kind: 'staged-complete'; stage: FileCheckpointV2; failureReason?: string }>
  | Readonly<{ kind: 'copying'; stage: FileCheckpointV2; attempt: BrowserDeliveryCopyAttempt }>
  | Readonly<{ kind: 'target-saved'; target: FileCheckpointV2; stage?: FileCheckpointV2 }>
  | Readonly<{ kind: 'cleanup-pending'; target: FileCheckpointV2; stage: FileCheckpointV2; failureReason?: string }>
  | Readonly<{ kind: 'cleaned'; target: FileCheckpointV2 }>
  | Readonly<{ kind: 'discarding'; checkpoint?: FileCheckpointV2 }>
  | Readonly<{ kind: 'discarded' }>

export interface BrowserDeliveryRecordV1 {
  readonly schemaVersion: typeof BROWSER_DELIVERY_RECORD_VERSION
  readonly operationId: string
  readonly policyDigest: string
  readonly fileId: string
  readonly source: BrowserDeliverySource
  readonly materializationRelativePath: readonly string[]
  readonly localMutation?: BrowserDeliveryLocalMutation
  readonly placement: BrowserFilePlacement
  readonly placementReason: string
  readonly generation: bigint
  readonly state: BrowserDeliveryState
  readonly digest: string
}

export interface BrowserDeliveryTraceEvent {
  readonly name: 'browser.delivery.policy_committed' | 'browser.delivery.file_committed'
  readonly operation_id: string
  readonly policy_digest: string
  readonly file_id?: string
  readonly generation?: bigint
  readonly placement?: BrowserFilePlacement
  readonly placement_reason?: string
  readonly prior_state?: BrowserDeliveryState['kind']
  readonly state?: BrowserDeliveryState['kind']
}
export type BrowserDeliveryTrace = (event: BrowserDeliveryTraceEvent) => void
