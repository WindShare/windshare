import type { PreservingWriterCapacityPurpose, PreservingWriterCost } from '../../output/persistent-tree/contracts'

export interface WorkspaceUsage {
  readonly ownedBytes: bigint
  readonly maximumBytes?: bigint
}


export type LifecycleUserAction =
  | 'pause'
  | 'stop'
  | 'continue'
  | 'save'
  | 'save-staged-files'
  | 'cleanup-staging'
  | 'discard-incomplete-staging'
  | 'redownload'
  | 'change-location'
  | 'discard'
  | 'delete'

export type V2ActiveReceiveControl = Extract<LifecycleUserAction, 'pause' | 'stop'>

export type ReceiveSettlementWait = Readonly<{
  readonly operation: V2ActiveReceiveControl | 'finish'
  readonly phase: 'waiting' | 'background'
}>


export interface PersistentWriterOpenPauseFact {
  readonly materializationRelativePath: readonly string[]
  readonly cost: PreservingWriterCost
  readonly purpose: PreservingWriterCapacityPurpose
}
