import type { ReceiveIntent } from '../../../transfer/intent'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalU64,
  canonicalU8,
  equalCanonicalBytes,
  snapshotCanonicalBytes,
  type CanonicalBytes,
} from '../canonical'
import { decodeWorkspaceBudgetV1, type WorkspaceBudgetV1 } from '../budget'
import { RECEIVE_RECORD_RECEIPT, type PersistedReceiveRecord } from '../records'
import {
  RECEIVE_RECEIPT_PREFIX,
  ReceiptReader,
  completeReceipt,
  receiptIdentity,
  snapshotAdmissionLimits,
} from './codec'
import {
  RECEIPT_PREPARATION_ADMISSION,
  RECEIPT_SCHEMA_VERSION,
  type PreparationAdmissionReceiptV1,
} from './model'

export async function createPreparationAdmissionReceipt(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly workspaceBudget: WorkspaceBudgetV1
  readonly contentRequestCountAtAdmission: bigint
  readonly estimatedQuotaBytes: bigint | null | undefined
  readonly currentUsageBytes: bigint
  readonly minimumReserveBytes: bigint
  readonly incrementalPhysicalPeakBytes: bigint
}): Promise<PreparationAdmissionReceiptV1> {
  const identity = receiptIdentity(input)
  if (input.contentRequestCountAtAdmission !== 0n) {
    throw new TypeError('workspace admission occurred after a content request')
  }
  if (input.workspaceBudget.operationId !== identity.operationId ||
      input.workspaceBudget.receiveIntentDigest !== identity.receiveIntentDigest) {
    throw new TypeError('workspace budget escaped its admission receipt')
  }
  const limits = snapshotAdmissionLimits(input)
  const variantFields = [
    canonicalFrame(input.workspaceBudget.canonicalBytes),
    canonicalFrame(canonicalIdentity(input.workspaceBudget.digest, 32, 'workspace budget digest')),
    canonicalFrame(canonicalU64(0n)),
    canonicalFrame(limits.estimatedQuotaBytes === null ? canonicalU8(0) :
      new Uint8Array([1, ...canonicalU64(limits.estimatedQuotaBytes)])),
    canonicalFrame(canonicalU64(limits.currentUsageBytes)),
    canonicalFrame(canonicalU64(limits.minimumReserveBytes)),
    canonicalFrame(canonicalU64(limits.incrementalPhysicalPeakBytes)),
  ]
  const completed = await completeReceipt(identity, RECEIPT_PREPARATION_ADMISSION, variantFields)
  return Object.freeze({
    ...completed,
    kind: 'preparation-admission',
    workspaceBudgetDigest: input.workspaceBudget.digest,
    contentRequestCountAtAdmission: 0n,
    ...limits,
  })
}

/** Decodes only the durable admission receipt needed to reissue a post-crash content gate. */
export async function decodePreparationAdmissionReceipt(
  record: PersistedReceiveRecord,
  workspaceBudget: WorkspaceBudgetV1,
): Promise<PreparationAdmissionReceiptV1 | undefined> {
  if (record.kind !== RECEIVE_RECORD_RECEIPT) return undefined
  const reader = new ReceiptReader(record.canonicalBytes)
  reader.prefix(RECEIVE_RECEIPT_PREFIX)
  const discriminant = reader.byte()
  if (discriminant !== RECEIPT_PREPARATION_ADMISSION) return undefined
  const operationId = reader.identity(16, 'operation ID')
  const receiveIntentDigest = reader.identity(32, 'receive intent digest')
  const budgetBytes = reader.frame()
  const workspaceBudgetDigest = reader.identity(32, 'workspace budget digest')
  const contentRequestCountAtAdmission = reader.u64('content request count')
  const quotaField = reader.frame()
  if ((quotaField.length !== 1 || quotaField[0] !== 0) &&
      (quotaField.length !== 9 || quotaField[0] !== 1)) {
    throw new TypeError('Admission quota estimate is invalid')
  }
  const estimatedQuotaBytes = quotaField[0] === 0 ? null :
    new DataView(quotaField.buffer, quotaField.byteOffset + 1, 8).getBigUint64(0, false)
  const currentUsageBytes = reader.u64('current quota usage')
  const minimumReserveBytes = reader.u64('quota reserve')
  const incrementalPhysicalPeakBytes = reader.u64('incremental physical peak')
  reader.end()

  const digest = await canonicalDigest(record.canonicalBytes)
  if (record.operationId !== operationId || record.digest !== digest ||
      workspaceBudget.operationId !== operationId ||
      workspaceBudget.receiveIntentDigest !== receiveIntentDigest ||
      workspaceBudget.digest !== workspaceBudgetDigest ||
      !equalCanonicalBytes(workspaceBudget.canonicalBytes, budgetBytes) ||
      contentRequestCountAtAdmission !== 0n) {
    throw new TypeError('preparation admission receipt authority changed')
  }
  return Object.freeze({
    schemaVersion: RECEIPT_SCHEMA_VERSION,
    operationId,
    receiveIntentDigest,
    kind: 'preparation-admission',
    workspaceBudgetDigest,
    contentRequestCountAtAdmission: 0n,
    estimatedQuotaBytes,
    currentUsageBytes,
    minimumReserveBytes,
    incrementalPhysicalPeakBytes,
    canonicalBytes: snapshotCanonicalBytes(record.canonicalBytes),
    digest,
  })
}

export async function decodePreparationAdmissionAuthority(
  record: PersistedReceiveRecord,
  receiveIntent: ReceiveIntent,
): Promise<Readonly<{
  budget: WorkspaceBudgetV1
  receipt: PreparationAdmissionReceiptV1
}> | undefined> {
  const budgetBytes = preparationAdmissionBudgetBytes(record)
  if (budgetBytes === undefined) return undefined
  const budget = await decodeWorkspaceBudgetV1(budgetBytes, receiveIntent)
  const receipt = await decodePreparationAdmissionReceipt(record, budget)
  if (receipt === undefined) throw new TypeError('admission budget lacks its receipt authority')
  return Object.freeze({ budget, receipt })
}

function preparationAdmissionBudgetBytes(
  record: PersistedReceiveRecord,
): CanonicalBytes | undefined {
  if (record.kind !== RECEIVE_RECORD_RECEIPT) return undefined
  const reader = new ReceiptReader(record.canonicalBytes)
  reader.prefix(RECEIVE_RECEIPT_PREFIX)
  if (reader.byte() !== RECEIPT_PREPARATION_ADMISSION) return undefined
  reader.identity(16, 'operation ID')
  reader.identity(32, 'receive intent digest')
  return reader.frame()
}
