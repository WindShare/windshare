import {
  canonicalDigest,
  canonicalFrame,
  canonicalRecord,
  canonicalU64,
  type CanonicalBytes,
} from '../../workspace/canonical'
import type { DirectZipJournalBudgetUsageV1 } from './model'

export const DIRECT_ZIP_JOURNAL_BUDGET_DOMAIN =
  'windshare/direct-zip-journal-budget/v1' as const
export const DIRECT_ZIP_MAXIMUM_MEMBER_COUNT = 1_000_000n
export const DIRECT_ZIP_MAXIMUM_CANONICAL_METADATA_BYTES = 268_435_456n
export const DIRECT_ZIP_MAXIMUM_RECORDS_PER_PAGE = 256
export const DIRECT_ZIP_MAXIMUM_CANONICAL_BYTES_PER_PAGE = 262_144
export const DIRECT_ZIP_MAXIMUM_PAGE_BODIES_PER_STAGING_TRANSACTION = 1
export const DIRECT_ZIP_MAXIMUM_PAGE_BODIES_PER_AUTHORITY_TRANSACTION = 0

const MAXIMUM_U64 = 0xffff_ffff_ffff_ffffn

export interface DirectZipJournalPageAdmissionV1 {
  readonly usage: DirectZipJournalBudgetUsageV1
  readonly pageCanonicalBytes: number
  readonly pageRecordCount: number
  readonly memberCountDelta: bigint
}

export function directZipJournalBudgetCanonicalV1(): CanonicalBytes {
  return canonicalRecord(DIRECT_ZIP_JOURNAL_BUDGET_DOMAIN, 1, [
    canonicalFrame(canonicalU64(DIRECT_ZIP_MAXIMUM_MEMBER_COUNT)),
    canonicalFrame(canonicalU64(DIRECT_ZIP_MAXIMUM_CANONICAL_METADATA_BYTES)),
    canonicalFrame(canonicalU64(BigInt(DIRECT_ZIP_MAXIMUM_RECORDS_PER_PAGE))),
    canonicalFrame(canonicalU64(BigInt(DIRECT_ZIP_MAXIMUM_CANONICAL_BYTES_PER_PAGE))),
    canonicalFrame(canonicalU64(BigInt(
      DIRECT_ZIP_MAXIMUM_PAGE_BODIES_PER_STAGING_TRANSACTION,
    ))),
    canonicalFrame(canonicalU64(BigInt(
      DIRECT_ZIP_MAXIMUM_PAGE_BODIES_PER_AUTHORITY_TRANSACTION,
    ))),
  ])
}

export function directZipJournalBudgetDigestV1(): Promise<string> {
  return canonicalDigest(directZipJournalBudgetCanonicalV1())
}

export function admitDirectZipJournalPageV1(
  input: DirectZipJournalPageAdmissionV1,
): DirectZipJournalBudgetUsageV1 {
  const current = validateDirectZipJournalBudgetUsageV1(input.usage)
  if (!Number.isInteger(input.pageRecordCount) || input.pageRecordCount < 1 ||
      input.pageRecordCount > DIRECT_ZIP_MAXIMUM_RECORDS_PER_PAGE) {
    throw journalQuotaExceeded('Direct ZIP journal page record count exceeds its named bound')
  }
  if (!Number.isSafeInteger(input.pageCanonicalBytes) || input.pageCanonicalBytes < 1 ||
      input.pageCanonicalBytes > DIRECT_ZIP_MAXIMUM_CANONICAL_BYTES_PER_PAGE) {
    throw journalQuotaExceeded('Direct ZIP journal page metadata exceeds its named page bound')
  }
  const memberCount = checkedBudgetAddition(
    current.memberCount,
    input.memberCountDelta,
    'Direct ZIP journal member accounting overflowed',
  )
  const canonicalMetadataBytes = checkedBudgetAddition(
    current.canonicalMetadataBytes,
    BigInt(input.pageCanonicalBytes),
    'Direct ZIP journal metadata accounting overflowed',
  )
  if (memberCount > DIRECT_ZIP_MAXIMUM_MEMBER_COUNT) {
    throw journalQuotaExceeded('Direct ZIP journal member budget is exhausted')
  }
  if (canonicalMetadataBytes > DIRECT_ZIP_MAXIMUM_CANONICAL_METADATA_BYTES) {
    throw journalQuotaExceeded('Direct ZIP journal canonical metadata budget is exhausted')
  }
  return Object.freeze({ memberCount, canonicalMetadataBytes })
}

export function validateDirectZipJournalBudgetUsageV1(
  input: DirectZipJournalBudgetUsageV1,
): DirectZipJournalBudgetUsageV1 {
  if (input === null || typeof input !== 'object') {
    throw new TypeError('Direct ZIP journal budget usage is invalid')
  }
  const memberCount = checkedBudgetValue(input.memberCount, 'Direct ZIP journal member count')
  const canonicalMetadataBytes = checkedBudgetValue(
    input.canonicalMetadataBytes,
    'Direct ZIP journal metadata bytes',
  )
  if (memberCount > DIRECT_ZIP_MAXIMUM_MEMBER_COUNT ||
      canonicalMetadataBytes > DIRECT_ZIP_MAXIMUM_CANONICAL_METADATA_BYTES) {
    throw journalQuotaExceeded('Direct ZIP journal usage exceeds its canonical budget')
  }
  return Object.freeze({ memberCount, canonicalMetadataBytes })
}

function checkedBudgetAddition(left: bigint, right: bigint, message: string): bigint {
  const delta = checkedBudgetValue(right, 'Direct ZIP journal admission delta')
  if (left > MAXIMUM_U64 - delta) throw journalQuotaExceeded(message)
  return left + delta
}

function checkedBudgetValue(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > MAXIMUM_U64) {
    throw new TypeError(`${label} must be a canonical u64`)
  }
  return value
}

function journalQuotaExceeded(message: string): DOMException {
  return new DOMException(message, 'QuotaExceededError')
}
