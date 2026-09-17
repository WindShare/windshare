import type { V2BlockRangeReader } from '../../content/v2-broker'

const RECEIPT_PROGRESS_INTERVAL_MILLISECONDS = 250

interface TransferContentProgress {
  received(objectBytes: number): void
  updated(): void
}

/** One stable receipt observer lets shared block reads count traffic once per job. */
export function observeTransferContent(
  broker: V2BlockRangeReader,
  progress: TransferContentProgress,
  now: () => number = () => performance.now(),
): V2BlockRangeReader {
  let lastUpdate = -Infinity
  const onReceive = (objectBytes: number) => {
    progress.received(objectBytes)
    const current = now()
    // Count every fragment without building snapshots or rendering at wire rate.
    if (current - lastUpdate < RECEIPT_PROGRESS_INTERVAL_MILLISECONDS) return
    lastUpdate = current
    progress.updated()
  }
  return {
    readRange: (descriptor, leaseId, range, options) => broker.readRange(
      descriptor, leaseId, range, { ...options, onReceive },
    ),
  }
}
