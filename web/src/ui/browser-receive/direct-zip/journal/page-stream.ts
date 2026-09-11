import { directZipEpochGenesisRoot } from '../../../../output/direct-zip/format'
import type {
  DirectZipJournalRepository, DirectZipPageChainV1, DirectZipPageKind, DirectZipRetainedEpochProofV1,
} from '../../../../output/direct-zip/journal'
import type { DirectZipEpochProofV1, DirectZipWriterCheckpointV1 } from '../../../../output/direct-zip/writer'
import { equalCanonicalBytes } from '../../../../output/workspace/canonical'
import { ZERO_DIGEST } from './bootstrap'
import { decodeEpoch, digestBytes, encodeEpoch } from './records'

export function retainedEpochProof(proof: DirectZipRetainedEpochProofV1): DirectZipEpochProofV1 {
  return decodeEpoch(encodeEpoch({
    start: proof.start, end: proof.end, contentDigest: digestBytes(proof.contentDigest),
    predecessorRoot: digestBytes(proof.predecessorRootDigest), epochRoot: digestBytes(proof.epochRootDigest),
  }))
}

export async function* epochProofsForAuthority(
  repository: DirectZipJournalRepository,
  authority: Readonly<{
    epochPages: DirectZipPageChainV1
    retainedEpochProof?: DirectZipRetainedEpochProofV1
  }>,
  checkpoint: Pick<DirectZipWriterCheckpointV1, 'operationId' | 'committedLength' | 'epochRoot'>,
): AsyncIterable<DirectZipEpochProofV1> {
  let end = 0n
  let root: Uint8Array = directZipEpochGenesisRoot()
  const proofs = async function* () {
    for await (const bytes of streamEntries(repository, checkpoint.operationId, 'epoch', authority.epochPages)) {
      yield decodeEpoch(bytes)
    }
    if (authority.retainedEpochProof !== undefined) yield retainedEpochProof(authority.retainedEpochProof)
  }
  for await (const proof of proofs()) {
    if (proof.start !== end || !equalCanonicalBytes(proof.predecessorRoot, root)) {
      throw new TypeError('Direct ZIP epoch pages lost their contiguous lineage')
    }
    end = proof.end
    root = proof.epochRoot
    yield proof
  }
  if (end !== checkpoint.committedLength || !equalCanonicalBytes(root, checkpoint.epochRoot)) {
    throw new TypeError('Direct ZIP epoch pages disagree with committed target bytes')
  }
}

export async function* streamEntries(
  repository: DirectZipJournalRepository,
  operationId: string,
  kind: DirectZipPageKind,
  chain: DirectZipPageChainV1,
): AsyncIterable<Uint8Array> {
  let pageCount = 0n
  let recordCount = 0n
  let metadataBytes = 0n
  let root = ZERO_DIGEST
  for await (const page of repository.streamPages({ operationId, pageKind: kind, chainId: chain.chainId })) {
    if (pageCount === chain.pageCount) break
    if (BigInt(page.pageOrdinal) !== pageCount || page.predecessorRootDigest !== root ||
        page.chainRecordCount !== recordCount + BigInt(page.entryCount) ||
        page.chainCanonicalMetadataBytes !== metadataBytes + BigInt(page.canonicalBytes.byteLength)) {
      throw new TypeError('Direct ZIP immutable page chain changed')
    }
    root = page.chainRootDigest
    pageCount += 1n
    recordCount = page.chainRecordCount
    metadataBytes = page.chainCanonicalMetadataBytes
    for (const bytes of page.canonicalEntries) yield bytes
  }
  if (pageCount !== chain.pageCount || root !== chain.rootDigest ||
      recordCount !== chain.recordCount || metadataBytes !== chain.canonicalMetadataBytes) {
    throw new TypeError('Direct ZIP immutable page chain is incomplete')
  }
}
