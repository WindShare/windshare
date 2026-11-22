import { fileCheckpointIsFinalized, type FileCheckpointV2 } from '../persistence/checkpoint'
import { finalFileCheckpointProof, type FinalFileCheckpointProof } from '../persistence/journal'
import { BrowserDeliveryConcurrencyError } from './repository'

export function validateDirectCheckpointProof(input: FinalFileCheckpointProof, checkpoint: FileCheckpointV2): void {
  const expected = finalFileCheckpointProof(checkpoint)
  const keys = Object.keys(expected) as (keyof FinalFileCheckpointProof)[]
  for (const key of keys) {
    const value = input[key]
    const canonical = expected[key]
    const equal = Array.isArray(value)
      ? JSON.stringify(value) === JSON.stringify(canonical) : value === canonical
    if (!equal) throw new BrowserDeliveryConcurrencyError('Direct final checkpoint proof changed')
  }
}

export function validateBrowserTargetCheckpoint(checkpoint: FileCheckpointV2): void {
  if (!fileCheckpointIsFinalized(checkpoint)) {
    throw new BrowserDeliveryConcurrencyError('Saved target requires a finalized committed checkpoint')
  }
}
