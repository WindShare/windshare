import type { ReceiveIntent } from '../../../transfer/intent'
import {
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalU64,
  equalCanonicalBytes,
  snapshotCanonicalBytes,
  type CanonicalBytes,
} from '../canonical'
import { decodeWorkspaceBudgetV1, type WorkspaceBudgetV1 } from '../budget'
import { RECEIVE_RECORD_RECEIPT, type PersistedReceiveRecord } from '../records'
import {
  RECEIVE_RECEIPT_PREFIX,
  ReceiptReader,
  canonicalOptionalDigest,
  completeReceipt,
  optionalDigest,
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
  readonly preparationManifestDigest?: string
  readonly sealedZipLayoutDigest?: string
  readonly workspaceBudget: WorkspaceBudgetV1
  readonly contentRequestCountAtAdmission: bigint
  readonly jobLimitBytes: bigint
  readonly processLimitBytes: bigint
  readonly estimatedQuotaBytes: bigint
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
  const preparationManifestDigest = optionalDigest(
    input.preparationManifestDigest,
    'preparation manifest digest',
  )
  const sealedZipLayoutDigest = optionalDigest(input.sealedZipLayoutDigest, 'ZIP layout digest')
  if ((preparationManifestDigest === undefined) !== (sealedZipLayoutDigest === undefined)) {
    throw new TypeError('workspace ZIP admission evidence is incomplete')
  }
  if (input.workspaceBudget.evidence.kind === 'prepared-zip') {
    if (preparationManifestDigest !== input.workspaceBudget.evidence.preparationManifestDigest ||
        sealedZipLayoutDigest !== input.workspaceBudget.evidence.sealedZipLayoutDigest) {
      throw new TypeError('workspace ZIP admission evidence changed')
    }
  } else if (preparationManifestDigest !== undefined) {
    throw new TypeError('single-file workspace budget cannot bind ZIP preparation')
  }
  const limits = snapshotAdmissionLimits(input)
  const variantFields = [
    canonicalFrame(canonicalOptionalDigest(preparationManifestDigest)),
    canonicalFrame(canonicalOptionalDigest(sealedZipLayoutDigest)),
    canonicalFrame(input.workspaceBudget.canonicalBytes),
    canonicalFrame(canonicalIdentity(input.workspaceBudget.digest, 32, 'workspace budget digest')),
    canonicalFrame(canonicalU64(0n)),
    canonicalFrame(canonicalU64(limits.jobLimitBytes)),
    canonicalFrame(canonicalU64(limits.processLimitBytes)),
    canonicalFrame(canonicalU64(limits.estimatedQuotaBytes)),
    canonicalFrame(canonicalU64(limits.currentUsageBytes)),
    canonicalFrame(canonicalU64(limits.minimumReserveBytes)),
    canonicalFrame(canonicalU64(limits.incrementalPhysicalPeakBytes)),
  ]
  const completed = await completeReceipt(identity, RECEIPT_PREPARATION_ADMISSION, variantFields)
  return Object.freeze({
    ...completed,
    kind: 'preparation-admission',
    ...(preparationManifestDigest === undefined ? {} : { preparationManifestDigest }),
    ...(sealedZipLayoutDigest === undefined ? {} : { sealedZipLayoutDigest }),
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
  const preparationManifestDigest = reader.optionalDigest('preparation manifest digest')
  const sealedZipLayoutDigest = reader.optionalDigest('sealed ZIP layout digest')
  const budgetBytes = reader.frame()
  const workspaceBudgetDigest = reader.identity(32, 'workspace budget digest')
  const contentRequestCountAtAdmission = reader.u64('content request count')
  const jobLimitBytes = reader.u64('job workspace limit')
  const processLimitBytes = reader.u64('process workspace limit')
  const estimatedQuotaBytes = reader.u64('estimated quota')
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
  if (workspaceBudget.evidence.kind === 'single-file') {
    if (preparationManifestDigest !== undefined || sealedZipLayoutDigest !== undefined) {
      throw new TypeError('single-file admission unexpectedly binds ZIP preparation')
    }
  } else if (preparationManifestDigest !== workspaceBudget.evidence.preparationManifestDigest ||
      sealedZipLayoutDigest !== workspaceBudget.evidence.sealedZipLayoutDigest) {
    throw new TypeError('ZIP admission receipt changed its preparation evidence')
  }
  return Object.freeze({
    schemaVersion: RECEIPT_SCHEMA_VERSION,
    operationId,
    receiveIntentDigest,
    kind: 'preparation-admission',
    ...(preparationManifestDigest === undefined ? {} : { preparationManifestDigest }),
    ...(sealedZipLayoutDigest === undefined ? {} : { sealedZipLayoutDigest }),
    workspaceBudgetDigest,
    contentRequestCountAtAdmission: 0n,
    jobLimitBytes,
    processLimitBytes,
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
  reader.optionalDigest('preparation manifest digest')
  reader.optionalDigest('sealed ZIP layout digest')
  return reader.frame()
}
