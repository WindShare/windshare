import type { ArtifactChoiceID } from '../../../transfer/intent'
import type { DirectZipBootstrapResumeDescriptorV1 } from '../../../output/direct-zip/journal'
import type {
  DirectZipRuntimePlatformFactsV1,
  DirectZipSupportMatrixArtifactV1,
  ReviewedDirectZipRuntimeFactsV1,
} from '../../../output/direct-zip/session'
import type { ReopenedDirectZipOperation } from '../../../output/resume/reopen-authority'
import type { OfferedArtifactChoice } from '../../../output/planning'
import type { OutputFailureSinks } from '../../../output/diagnostics'
import type {
  V2ArtifactPresentationAuthority,
  V2BoundReceiveOperation,
} from '../../v2-receive-runtime'

export interface BrowserDirectZipEvidenceSource {
  read(signal: AbortSignal): Promise<Readonly<{
    readonly artifact: DirectZipSupportMatrixArtifactV1
    readonly runtime: Omit<DirectZipRuntimePlatformFactsV1, 'featureFacts'>
  }>>
}

export interface BrowserDirectZipFreshAuthorityInput {
  readonly offered: OfferedArtifactChoice & Readonly<{
    route: Extract<OfferedArtifactChoice['route'], { kind: 'direct-resumable-zip' }>
  }>
  readonly pickedParent: Promise<FileSystemDirectoryHandle>
  readonly preClickRanking: readonly ArtifactChoiceID[]
  readonly reviewed: ReviewedDirectZipRuntimeFactsV1
}

export interface BrowserDirectZipRuntimePort {
  startFresh(input: BrowserDirectZipFreshAuthorityInput): V2ArtifactPresentationAuthority
  dispatchBootstrapCandidate(
    candidate: DirectZipBootstrapResumeDescriptorV1,
    signal: AbortSignal,
  ): Promise<void>
  resume(
    operation: ReopenedDirectZipOperation,
    signal: AbortSignal,
    failures?: OutputFailureSinks,
  ): Promise<V2BoundReceiveOperation>
  /** Uses retained-session cleanup; it must never reopen transfer execution. */
  deleteRetained(
    operation: ReopenedDirectZipOperation,
    signal: AbortSignal,
    failures?: OutputFailureSinks,
  ): Promise<void>
}

export interface BrowserDirectZipCompositionPort {
  readonly evidence: BrowserDirectZipEvidenceSource
  readonly runtime: BrowserDirectZipRuntimePort
}
