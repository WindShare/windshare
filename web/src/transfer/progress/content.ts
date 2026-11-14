import type { BlockRangeReader } from '../../content/broker'

interface TransferContentProgress {
  remainingWork?: import('../../content/lane-set').RemainingWorkObservation
  received(objectBytes: number): void
  updated(): void
}

/** One stable receipt observer lets shared block reads count traffic once per job. */
export function observeTransferContent(
  broker: BlockRangeReader,
  progress: TransferContentProgress,
): BlockRangeReader {
  const onReceive = (objectBytes: number) => {
    progress.received(objectBytes)
    progress.updated()
  }
  return {
    readRange: (descriptor, leaseId, range, options) => broker.readRange(
      descriptor, leaseId, range, { ...options, onReceive,
        ...(progress.remainingWork === undefined ? {} : { remainingWork: progress.remainingWork }) },
    ),
  }
}
