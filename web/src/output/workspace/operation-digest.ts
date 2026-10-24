import { encodeBase64Url } from '../../crypto/bytes'
import type { ReceiveIntent } from '../../transfer/intent'
import type { WorkspaceMaterializationEvidence } from '../execution/persistent-execution'

export async function checkpointSetDigest(
  intent: ReceiveIntent,
  evidence: WorkspaceMaterializationEvidence,
): Promise<string> {
  const entries = evidence.entries
    .filter(entry => entry.kind === 'file')
    .map(entry => `${entry.fileId}:${entry.checkpoint.recordDigest}:${entry.exactSize.toString()}`)
    .sort()
  return digestText(`windshare/workspace-checkpoint-set/v1\n${intent.digest}\n${entries.join('\n')}`)
}

export async function operationDigest(intent: ReceiveIntent, purpose: string): Promise<string> {
  return digestText(`windshare/ui-operation-receipt/v1\n${intent.digest}\n${purpose}`)
}

export async function digestText(value: string): Promise<string> {
  const bytes = new TextEncoder().encode(value)
  return encodeBase64Url(new Uint8Array(await crypto.subtle.digest('SHA-256', bytes)))
}
