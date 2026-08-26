import type { ReceiveLifecycleState } from '../workspace/state'
import {
  createFSARecoveryCheckpointSnapshot,
  deriveFSARecoverySummary,
  type RecoverySummary,
} from './recovery-summary'
import type { FSAResumableCheckpointEvidence } from './settlement-ledger'
import type { DirectTreeIntent } from './settlement-proof'

export async function deriveSettledFSARecoverySummary(input: Readonly<{
  intent: DirectTreeIntent
  lifecycle: ReceiveLifecycleState
  checkpointEvidence: FSAResumableCheckpointEvidence
}>): Promise<RecoverySummary> {
  if (input.lifecycle.kind !== 'resumable-receive' ||
      input.lifecycle.payloadKind !== 'file-set') {
    throw new TypeError('FSA pause did not produce a file-set continuation')
  }
  const snapshot = await createFSARecoveryCheckpointSnapshot(
    input.intent,
    input.lifecycle.generation,
    input.checkpointEvidence.checkpoints,
  )
  if (snapshot.checkpointSetDigest !== input.checkpointEvidence.checkpointSetDigest) {
    throw new TypeError('FSA pause checkpoint evidence changed after its settlement scan')
  }
  return deriveFSARecoverySummary({
    intent: input.intent,
    lifecycle: input.lifecycle,
    snapshot,
  })
}
