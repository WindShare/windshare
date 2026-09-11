import type {
  DirectZipBootstrapCandidateV1, DirectZipCheckpointV1, DirectZipImmutablePageV1,
} from '../model'
import { validateDirectZipImmutablePageV1 } from '../records'
import { samePageChain } from './authority'

/** Initial pages and their first accounting edge are owned by the persisted bootstrap candidate. */
export async function validateBootstrapPages(
  candidate: DirectZipBootstrapCandidateV1,
  checkpoint: DirectZipCheckpointV1,
  input: readonly DirectZipImmutablePageV1[],
): Promise<readonly DirectZipImmutablePageV1[]> {
  if (input.length === 0) {
    if (checkpoint.entryOrdinal !== 0n || checkpoint.journalUsage.memberCount !== 0n ||
        checkpoint.journalUsage.canonicalMetadataBytes !== 0n ||
        checkpoint.layoutPages.pageCount !== 0n || checkpoint.centralPages.pageCount !== 0n ||
        checkpoint.epochPages.pageCount !== 0n) {
      throw new TypeError('Direct ZIP bootstrap omitted its initial pages')
    }
    return Object.freeze([])
  }
  if (input.length !== 3 || checkpoint.entryOrdinal !== 1n ||
      checkpoint.committedSelectedPayloadBytes !== 0n) {
    throw new TypeError('Direct ZIP bootstrap must contain exactly one owned root')
  }
  const pages = await Promise.all(input.map(validateDirectZipImmutablePageV1))
  const chains = [checkpoint.layoutPages, checkpoint.centralPages, checkpoint.epochPages]
  const kinds = ['layout', 'central', 'epoch'] as const
  let previous: DirectZipImmutablePageV1 | undefined
  for (let index = 0; index < pages.length; index += 1) {
    const page = pages[index]!
    const accounting = page.accountingPredecessor
    if (page.operationId !== candidate.operationId || page.pageKind !== kinds[index] ||
        page.pageOrdinal !== 0 || page.entryCount !== 1 ||
        !samePageChain(chains[index]!, {
          chainId: page.chainId, rootDigest: page.chainRootDigest, pageCount: 1n,
          recordCount: page.chainRecordCount, canonicalMetadataBytes: page.chainCanonicalMetadataBytes,
        }) ||
        (previous === undefined
          ? accounting.kind !== 'checkpoint' || accounting.checkpointGeneration !== 1n ||
            accounting.checkpointDigest !== candidate.digest ||
            page.budgetUsage.memberCount !== 1n ||
            page.budgetUsage.canonicalMetadataBytes !== BigInt(page.canonicalBytes.byteLength)
          : accounting.kind !== 'page' || accounting.pageId !== previous.id ||
            accounting.pageKind !== previous.pageKind || accounting.pageDigest !== previous.digest ||
            page.budgetUsage.memberCount !== previous.budgetUsage.memberCount ||
            page.budgetUsage.canonicalMetadataBytes !== previous.budgetUsage.canonicalMetadataBytes +
              BigInt(page.canonicalBytes.byteLength))) {
      throw new TypeError('Direct ZIP bootstrap page lineage is inconsistent')
    }
    previous = page
  }
  if (checkpoint.accountingTailPageId !== previous!.id ||
      checkpoint.journalUsage.memberCount !== previous!.budgetUsage.memberCount ||
      checkpoint.journalUsage.canonicalMetadataBytes !== previous!.budgetUsage.canonicalMetadataBytes) {
    throw new TypeError('Direct ZIP bootstrap accounting tail is inconsistent')
  }
  return Object.freeze(pages)
}
