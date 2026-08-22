import {
  checkedZipAdd,
  requiresZip64End,
  requireZipUint64,
} from '../../zip-layout/policy'
import { requireDirectZipFsaOffset } from './offset'
import {
  DIRECT_ZIP_CLASSIC_END_PROOF_BYTES,
  DIRECT_ZIP_ZIP64_END_PROOF_BYTES,
} from './tail'

export interface DirectZipClosingLayoutInputV2 {
  readonly entryCount: bigint
  readonly centralDirectoryOffset: bigint
  readonly centralDirectoryBytes: bigint
}

export interface DirectZipClosingLayoutV2 extends DirectZipClosingLayoutInputV2 {
  readonly zip64EndRequired: boolean
  readonly closingTailBytes: bigint
  readonly exactArchiveBytes: bigint
}

export function planDirectZipClosingLayoutV2(
  input: DirectZipClosingLayoutInputV2,
): DirectZipClosingLayoutV2 {
  if (input === null || typeof input !== 'object') {
    throw new TypeError('direct ZIP closing layout is invalid')
  }
  requireZipUint64(input.entryCount, 'direct ZIP entry count')
  if (input.entryCount === 0n) throw new TypeError('direct ZIP layout omitted its ownership root')
  requireDirectZipFsaOffset(input.centralDirectoryOffset, 'direct ZIP central-directory offset')
  requireDirectZipFsaOffset(input.centralDirectoryBytes, 'direct ZIP central-directory size')
  const zip64EndRequired = requiresZip64End(input)
  const closingTailBytes = BigInt(zip64EndRequired
    ? DIRECT_ZIP_ZIP64_END_PROOF_BYTES
    : DIRECT_ZIP_CLASSIC_END_PROOF_BYTES)
  const exactArchiveBytes = checkedZipAdd(
    input.centralDirectoryOffset,
    input.centralDirectoryBytes,
    closingTailBytes,
  )
  requireDirectZipFsaOffset(exactArchiveBytes, 'direct ZIP exact archive length')
  return Object.freeze({
    entryCount: input.entryCount,
    centralDirectoryOffset: input.centralDirectoryOffset,
    centralDirectoryBytes: input.centralDirectoryBytes,
    zip64EndRequired,
    closingTailBytes,
    exactArchiveBytes,
  })
}
