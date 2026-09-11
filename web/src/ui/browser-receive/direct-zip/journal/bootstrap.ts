import { encodeBase64Url } from '../../../../crypto/bytes'
import {
  chainDirectZipEpochDigestV1, digestDirectZipArchiveBytes, directZipEpochGenesisRoot,
  encodeDirectZipBootstrapPrefixV1, encodeDirectZipCentralDirectoryRecordV2,
  planDirectZipEntryV2, type DirectZipOwnershipMarkerV1,
} from '../../../../output/direct-zip/format'
import {
  createDirectZipCheckpointV1, createDirectZipImmutablePageV1,
  type DirectZipBootstrapCandidateV1, type DirectZipCheckpointV1,
  type DirectZipImmutablePageV1, type DirectZipPageChainV1,
  type DirectZipPageKind, type DirectZipTargetObservationV1,
} from '../../../../output/direct-zip/journal'
import {
  canonicalDigest, canonicalFrame, canonicalIdentity, canonicalRecord,
} from '../../../../output/workspace/canonical'
import { encodeCentral, encodeEpoch, encodeLayout, UNBOUND_DISCOVERY } from './records'

export interface InitialBrowserDirectZipCheckpointOptions {
  readonly candidate: DirectZipBootstrapCandidateV1
  readonly receiveIntentDigest: string
  readonly parentBindingDigest: string
  readonly fileBindingDigest: string
  readonly ownershipMarker: DirectZipOwnershipMarkerV1
  readonly rootComponent: string
  readonly expectedRootDirectoryId: string
  readonly observeTarget: (epochRoot: Uint8Array) => Promise<DirectZipTargetObservationV1>
  readonly randomId?: () => string
}

export async function createInitialBrowserDirectZipCheckpoint(
  options: InitialBrowserDirectZipCheckpointOptions,
): Promise<Readonly<{ checkpoint: DirectZipCheckpointV1; pages: readonly DirectZipImmutablePageV1[] }>> {
  const { candidate, ownershipMarker, rootComponent } = options
  if (encodeBase64Url(ownershipMarker.operationId) !== candidate.operationId ||
      encodeBase64Url(ownershipMarker.candidateId) !== candidate.candidateId ||
      encodeBase64Url(ownershipMarker.ownershipNonce) !== candidate.ownershipNonce ||
      encodeBase64Url(ownershipMarker.bindingDigest) !== candidate.targetBindingDigest) {
    throw new TypeError('Direct ZIP bootstrap marker escaped its frozen candidate')
  }
  const rootEvidence = rootDirectoryEvidence(options.expectedRootDirectoryId)
  const rootDigest = await canonicalDigest(rootEvidence)
  const prefix = encodeDirectZipBootstrapPrefixV1(rootComponent, ownershipMarker)
  const contentDigest = digestDirectZipArchiveBytes(prefix)
  const proof = {
    start: 0n, end: BigInt(prefix.byteLength), contentDigest,
    predecessorRoot: directZipEpochGenesisRoot(),
    epochRoot: chainDirectZipEpochDigestV1({
      start: 0n, end: BigInt(prefix.byteLength), contentDigest,
      predecessorRoot: directZipEpochGenesisRoot(),
    }),
  }
  const rootPlan = planDirectZipEntryV2({
    ordinal: 0n, localHeaderOffset: 0n,
    entry: { kind: 'directory', path: [rootComponent] }, ownershipMarker,
  })
  const entries = [
    encodeLayout({ plan: rootPlan, layoutEvidence: rootEvidence, discoveryEvidence: UNBOUND_DISCOVERY }),
    encodeCentral(0n, encodeDirectZipCentralDirectoryRecordV2(rootPlan, 0)),
    encodeEpoch(proof),
  ]
  const kinds = ['layout', 'central', 'epoch'] as const
  const randomId = options.randomId ?? browserDirectZipRandomId
  const pages: DirectZipImmutablePageV1[] = []
  for (let index = 0; index < kinds.length; index += 1) {
    const previous = pages.at(-1)
    pages.push(await createDirectZipImmutablePageV1({
      operationId: candidate.operationId, pageKind: kinds[index]!,
      chainId: randomId(), pageOrdinal: 0, predecessorRootDigest: ZERO_DIGEST,
      canonicalEntries: [entries[index]!], previousChainRecordCount: 0n,
      previousChainCanonicalMetadataBytes: 0n,
      previousBudgetUsage: previous?.budgetUsage ?? { memberCount: 0n, canonicalMetadataBytes: 0n },
      accountingPredecessor: previous === undefined
        ? { kind: 'checkpoint', checkpointGeneration: 1n, checkpointDigest: candidate.digest }
        : { kind: 'page', pageKind: previous.pageKind, pageId: previous.id, pageDigest: previous.digest },
    }))
  }
  const observation = await options.observeTarget(proof.epochRoot)
  if (observation.exactLength !== proof.end) throw new TypeError('Direct ZIP bootstrap target changed')
  const last = pages.at(-1)!
  const checkpoint = await createDirectZipCheckpointV1({
    operationId: candidate.operationId, receiveIntentDigest: options.receiveIntentDigest,
    targetBindingDigest: candidate.targetBindingDigest, policies: candidate.policies,
    generation: 1n, phase: 'between-members', entryOrdinal: 1n,
    discovery: { cursorCanonicalBytes: UNBOUND_DISCOVERY, directoryAdmissionDigest: rootDigest,
      discoveryRootDigest: rootDigest },
    archiveOffset: proof.end, committedArchiveLength: proof.end, committedSelectedPayloadBytes: 0n,
    parentBindingDigest: options.parentBindingDigest, fileBindingDigest: options.fileBindingDigest,
    targetObservation: observation, epochRootDigest: encodeBase64Url(proof.epochRoot),
    layoutPages: chainFromPage(pages[0]!), centralPages: chainFromPage(pages[1]!),
    epochPages: chainFromPage(pages[2]!), journalUsage: last.budgetUsage,
    accountingTailPageId: last.id,
  })
  return Object.freeze({ checkpoint, pages: Object.freeze(pages) })
}

export const ZERO_DIGEST = encodeBase64Url(new Uint8Array(32))

export function chainFromPage(page: DirectZipImmutablePageV1): DirectZipPageChainV1 {
  return Object.freeze({
    chainId: page.chainId, rootDigest: page.chainRootDigest,
    pageCount: BigInt(page.pageOrdinal + 1), recordCount: page.chainRecordCount,
    canonicalMetadataBytes: page.chainCanonicalMetadataBytes,
  })
}

export function rootDirectoryEvidence(rootId: string): Uint8Array {
  return canonicalRecord('windshare/browser-direct-zip-root/v1', 1, [
    canonicalFrame(canonicalIdentity(rootId, 16, 'Direct ZIP root directory ID')),
  ])
}

export function browserDirectZipRandomId(): string {
  return encodeBase64Url(crypto.getRandomValues(new Uint8Array(16)))
}

export function chainForKind(
  checkpoint: Pick<DirectZipCheckpointV1, 'layoutPages' | 'centralPages' | 'epochPages'>,
  kind: DirectZipPageKind,
): DirectZipPageChainV1 {
  switch (kind) {
    case 'layout': return checkpoint.layoutPages
    case 'central': return checkpoint.centralPages
    case 'epoch': return checkpoint.epochPages
  }
}
