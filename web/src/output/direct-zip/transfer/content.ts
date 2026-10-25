import { snapshotContentAvailability, type ContentAvailability } from '../../../transfer/output/content-availability'
import type { DirectZipWriterCheckpointV1 } from '../writer/model'

/** The promoted writer checkpoint is the only owner of a member's reusable source prefix. */
export function directZipMemberContent(
  checkpoint: DirectZipWriterCheckpointV1,
  binding: Readonly<{ shareInstance: string; objectId: string; backend: string }>,
): ContentAvailability | undefined {
  const member = checkpoint.member
  if (checkpoint.phase !== 'inside-member' || member === undefined) return undefined
  const payloadOffset = member.plan.zipEntry.localHeaderOffset + member.plan.localHeaderBytes
  if (payloadOffset + member.payloadOffset > checkpoint.committedLength) {
    throw new TypeError('Direct ZIP content exceeds its committed archive')
  }
  return snapshotContentAvailability({
    source: { shareInstance: binding.shareInstance, fileId: member.source.fileId, fileRevision: member.source.revision },
    exactSize: member.source.exactSize,
    replica: { backend: binding.backend, operationId: checkpoint.operationId, objectId: binding.objectId,
      checkpointId: `${checkpoint.operationId}:${member.plan.ordinal}`, generation: checkpoint.generation, payloadOffset },
    ranges: member.payloadOffset === 0n ? [] : [{ start: 0n, end: member.payloadOffset }],
  })
}
