import { encodeBase64Url } from '../../crypto/bytes'
import type { AdmittedWorkspaceContent } from '../../output/workspace/stages'
import type { ExactSingleFileEvidence } from '../../transfer/output/session'
import type { ReceiveLifecycleState } from '../../transfer/lifecycle/state'
import type { ArtifactChoiceID, ReceiveIntent } from '../../transfer/intent'

const AUTHORITY_REFERENCE_BYTES = 32

export function requireMatchingSingleFileAdmission(
  admitted: AdmittedWorkspaceContent,
  evidence: ExactSingleFileEvidence,
): void {
  const frozen = admitted.budget.evidence
  if (frozen.kind !== 'single-file' || frozen.fileId !== evidence.fileId ||
      frozen.containingDirectoryId !== evidence.containingDirectoryId ||
      frozen.generation !== evidence.generation ||
      frozen.catalogSize !== evidence.catalogSize) {
    throw new TypeError('Workspace continuation changed its admitted file evidence')
  }
}

export function requireSameIntent(expected: ReceiveIntent, supplied: ReceiveIntent): void {
  if (expected.operationId !== supplied.operationId || expected.digest !== supplied.digest) {
    throw new TypeError('Receive authority escaped its frozen intent')
  }
}

export function snapshotPreClickRanking(
  selected: ArtifactChoiceID,
  ranking: readonly ArtifactChoiceID[],
): readonly ArtifactChoiceID[] {
  const snapshot = Object.freeze([...ranking])
  if (snapshot.length === 0 || !snapshot.includes(selected) ||
      new Set(snapshot).size !== snapshot.length) {
    throw new DOMException(
      'Artifact activation lost the exact pre-click choice ranking',
      'InvalidStateError',
    )
  }
  return snapshot
}

export function randomAuthorityReference(): string {
  const bytes = new Uint8Array(AUTHORITY_REFERENCE_BYTES)
  crypto.getRandomValues(bytes)
  return encodeBase64Url(bytes)
}

export function unavailableRoute(): DOMException {
  return new DOMException('No installed browser authority matches this action', 'NotSupportedError')
}

export function isWorkspaceTerminal(state: ReceiveLifecycleState): boolean {
  return state.kind === 'published' || state.kind === 'partial-directory' ||
    state.kind === 'restart-required' || state.kind === 'source-invalidated' || state.kind === 'discarded' ||
    state.kind === 'needs-attention' ||
    (state.kind === 'download-started' && state.attemptKind === 'portable')
}
