import type { TransferProgress } from '../job/contract'

export type ProgressChange = 'counters' | 'transition'

/** A job owns current facts; readers choose when to construct their presentation. */
export interface TransferProgressSource {
  readonly transferJobId: string
  read(): TransferProgress
}

export type TransferProgressObserver = (source: TransferProgressSource, change: ProgressChange) => void
