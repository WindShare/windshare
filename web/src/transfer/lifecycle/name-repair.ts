/** User-visible restoration evidence. Storage handles, ledger records and repair algorithms remain output-owned. */
export type CompatibleNamePairPlacement = 'inside-logical-root' | 'beside-mapped-root'

export type CompatibleNameFooterState = 'active' | 'completed' | 'stopped' | 'failed'

export interface CompatibleNameFooterObservationV1 {
  readonly committedCount: number
  readonly state: CompatibleNameFooterState
}

export interface CompatibleNameRepairSummary {
  readonly committedCount: number
  readonly logicalPathSample: readonly (readonly string[])[]
  readonly pairDisplayNames: Readonly<{
    script?: string
    sidecar: string
  }>
  readonly placement: CompatibleNamePairPlacement
  readonly latestObservedFooter?: CompatibleNameFooterObservationV1
  readonly sidecarSync: 'pending' | 'current'
  readonly terminalSettlement: 'none' | 'pending' | 'complete'
}
