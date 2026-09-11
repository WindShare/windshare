export function summarizeEvents(events) {
  for (const event of events) {
    if (['source-read', 'target-write'].includes(event.kind) && (!Number.isSafeInteger(event.bytes) || event.bytes < 0)) {
      throw new Error('Byte observations must be non-negative safe integers')
    }
    if (event.kind === 'inventory' && [event.stageBytes, event.targetBytes].some(bytes => !Number.isSafeInteger(bytes) || bytes < 0)) {
      throw new Error('Inventory observations must be non-negative safe integers')
    }
  }
  const writes = events.filter(event => event.kind === 'target-write')
  const copies = writes.filter(event => event.origin === 'staging')
  const inventories = events.filter(event => event.kind === 'inventory')
  return {
    receivedSourceBytes: events.filter(event => event.kind === 'source-read').reduce((sum, event) => sum + event.bytes, 0),
    cumulativeTargetWriteBytes: writes.reduce((sum, event) => sum + event.bytes, 0),
    cumulativeLocalCopyBytes: copies.reduce((sum, event) => sum + event.bytes, 0),
    sampledPeakStageFileBytes: Math.max(0, ...inventories.map(event => event.stageBytes)),
    sampledPeakTargetFileBytes: Math.max(0, ...inventories.map(event => event.targetBytes)),
    sampledPeakCombinedFileBytes: Math.max(0, ...inventories.map(event => event.stageBytes + event.targetBytes)),
    inventoryBasis: 'browser-enumerated file lengths, including visible FSA temporary entries; other native temporary storage appears only in host samples',
    copyFailures: events.filter(event => event.kind === 'copy-failed').length,
    stageRemovals: events.filter(event => event.kind === 'stage-removed').length,
  }
}

export function timingComparison(baseline, candidate) {
  function median(values) {
    if (values.length === 0 || values.some(value => !Number.isFinite(value) || value <= 0)) throw new Error('Positive timing samples required')
    const ordered = [...values].sort((a, b) => a - b)
    const middle = Math.floor(ordered.length / 2)
    return ordered.length % 2 ? ordered[middle] : (ordered[middle - 1] + ordered[middle]) / 2
  }
  const baselineMedianMilliseconds = median(baseline)
  const candidateMedianMilliseconds = median(candidate)
  return {
    baselineMedianMilliseconds, candidateMedianMilliseconds,
    candidateToBaselineRatio: candidateMedianMilliseconds / baselineMedianMilliseconds,
    basis: 'sequential alternated warmup and measured cases in the same browser/profile; picker and digest verification excluded',
  }
}
