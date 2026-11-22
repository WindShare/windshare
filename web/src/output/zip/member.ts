import {
  checkedZipAdd, encodeZipCentralDirectoryRecord, encodeZipDataDescriptor, type ZipEntryPlanV1,
} from './policy'

/** Offsets describe the artifact; content coverage remains relative to the source file. */
export function zipMemberCoordinates(plan: ZipEntryPlanV1) {
  const payloadOffset = checkedZipAdd(plan.localHeaderOffset, plan.localHeaderBytes)
  const descriptorOffset = checkedZipAdd(payloadOffset, plan.exactSize)
  const endOffset = checkedZipAdd(descriptorOffset, plan.descriptorBytes)
  if (endOffset !== checkedZipAdd(plan.localHeaderOffset, plan.entryStreamBytes)) {
    throw new TypeError('ZIP member accounting disagrees with its encoding plan')
  }
  return { payloadOffset, descriptorOffset, endOffset }
}

/** Storage decides when completion is durable; all writers require the same exact payload boundary. */
export function completeZipMember(plan: ZipEntryPlanV1, payloadBytes: bigint, crc32: number) {
  if (payloadBytes !== plan.exactSize) throw new Error('ZIP member payload is incomplete')
  return {
    ...zipMemberCoordinates(plan),
    descriptor: encodeZipDataDescriptor(plan, crc32),
    centralRecord: encodeZipCentralDirectoryRecord(plan, crc32),
  }
}
