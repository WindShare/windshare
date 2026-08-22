import type { ArtifactChoiceID } from '../../../transfer/intent'
import type { ReviewedDirectZipRuntimeFactsV1 } from '../../../output/direct-zip/session'
import type { OfferedArtifactChoice } from '../../../output/planning'
import type { BrowserReceiveWindow } from '../contracts'
import { snapshotPreClickRanking, unavailableRoute } from '../shared'
import type { V2ArtifactPresentationAuthority } from '../../v2-receive-runtime'
import {
  BROWSER_DIRECT_ZIP_TARGET_ROUTE_ID,
} from './support'
import type { BrowserDirectZipCompositionPort } from './contracts'

export interface InstalledBrowserDirectZipRoute {
  readonly directZip: BrowserDirectZipCompositionPort
  readonly reviewed: ReviewedDirectZipRuntimeFactsV1
}

/** Picker invocation remains synchronous with the click; all later work owns that promise. */
export function startBrowserDirectZipAuthority(
  windowPort: BrowserReceiveWindow,
  offered: OfferedArtifactChoice,
  preClickRanking: readonly ArtifactChoiceID[],
  installed: InstalledBrowserDirectZipRoute | undefined,
): V2ArtifactPresentationAuthority {
  if (installed === undefined || offered.route.kind !== 'direct-resumable-zip' ||
      offered.route.target.routeId !== BROWSER_DIRECT_ZIP_TARGET_ROUTE_ID ||
      !sameSupport(offered.route.target.support, installed.reviewed.support)) {
    throw unavailableRoute()
  }
  const picker = windowPort.showDirectoryPicker
  if (typeof picker !== 'function') throw unavailableRoute()
  const frozenPreClickRanking = snapshotPreClickRanking(offered.choice.choiceId, preClickRanking)
  let pickedParent: Promise<FileSystemDirectoryHandle>
  try {
    pickedParent = picker.call(windowPort, { mode: 'readwrite' })
  } catch (error) {
    throw error instanceof DOMException && error.name === 'AbortError'
      ? new DOMException('Direct ZIP parent picker was cancelled', 'AbortError')
      : error
  }
  return installed.directZip.runtime.startFresh({
    offered: offered as Parameters<BrowserDirectZipCompositionPort['runtime']['startFresh']>[0]['offered'],
    pickedParent,
    preClickRanking: frozenPreClickRanking,
    reviewed: installed.reviewed,
  })
}

function sameSupport(
  left: ReviewedDirectZipRuntimeFactsV1['support'],
  right: ReviewedDirectZipRuntimeFactsV1['support'],
): boolean {
  return left.supportMatrixDigest === right.supportMatrixDigest &&
    left.browserBinaryDigest === right.browserBinaryDigest &&
    left.browserVersion === right.browserVersion &&
    left.operatingSystemBuild === right.operatingSystemBuild &&
    left.filesystemProfile === right.filesystemProfile &&
    left.rawEvidenceDigest === right.rawEvidenceDigest &&
    left.requiredFeatureFactsDigest === right.requiredFeatureFactsDigest &&
    left.recommendationPolicyDigest === right.recommendationPolicyDigest &&
    left.policies.zipEncoding === right.policies.zipEncoding &&
    left.policies.layout === right.policies.layout &&
    left.policies.checkpoint === right.policies.checkpoint &&
    left.policies.journalBudget === right.policies.journalBudget &&
    left.policies.epoch === right.policies.epoch
}
