import {
  ARTIFACT_CHOICE_IDENTITY_DOMAIN,
  CanonicalDecoder,
  canonicalRecord,
  canonicalValue,
  digestText,
  frame,
  invalidDecodedCanonicalBytes,
  requireDecodedCanonicalBytes,
} from './canonical'
import {
  ARTIFACT_CHOICE_IDENTITY_VERSION,
  type ArtifactChoiceID,
  type ArtifactChoiceIdentity,
  type ArtifactKind,
  type ArtifactSpec,
  type GuaranteeProfile,
  type MaterializationKind,
  type MaterializationPlan,
  type PreparationPolicy,
} from './model'

export async function createArtifactChoiceIdentity(input: {
  readonly artifactKind: ArtifactKind
  readonly materializationKind: MaterializationKind
  readonly guaranteeProfile: GuaranteeProfile
  readonly preparation: PreparationPolicy
}): Promise<ArtifactChoiceIdentity> {
  if (!legalArtifactChoiceTuple(input)) {
    throw new TypeError('artifact choice tuple is invalid')
  }
  const canonicalBytes = canonicalRecord(ARTIFACT_CHOICE_IDENTITY_DOMAIN, [
    frame(Uint8Array.of(artifactKindByte(input.artifactKind))),
    frame(Uint8Array.of(materializationKindByte(input.materializationKind))),
    frame(Uint8Array.of(guaranteeProfileByte(input.guaranteeProfile))),
    frame(Uint8Array.of(preparationByte(input.preparation))),
  ])
  return canonicalValue({
    version: ARTIFACT_CHOICE_IDENTITY_VERSION,
    ...input,
    id: await digestText(canonicalBytes) as ArtifactChoiceID,
  }, canonicalBytes)
}

export async function deriveArtifactChoiceIdentity(
  artifact: ArtifactSpec,
  plan: MaterializationPlan,
): Promise<ArtifactChoiceIdentity> {
  return createArtifactChoiceIdentity({
    artifactKind: artifact.kind,
    materializationKind: plan.kind,
    guaranteeProfile: materializationGuaranteeProfile(plan),
    preparation: plan.preparation,
  })
}

export async function decodeArtifactChoiceIdentity(
  encoded: Uint8Array<ArrayBuffer>,
): Promise<ArtifactChoiceIdentity> {
  const cursor = CanonicalDecoder.record(encoded, ARTIFACT_CHOICE_IDENTITY_DOMAIN)
  const identity = await createArtifactChoiceIdentity({
    artifactKind: artifactKindFromByte(cursor.readFramedByte()),
    materializationKind: materializationKindFromByte(cursor.readFramedByte()),
    guaranteeProfile: guaranteeProfileFromByte(cursor.readFramedByte()),
    preparation: preparationFromByte(cursor.readFramedByte()),
  })
  cursor.requireDone()
  requireDecodedCanonicalBytes(encoded, identity.canonicalBytes, 'artifact choice identity')
  return identity
}

function materializationGuaranteeProfile(plan: MaterializationPlan): GuaranteeProfile {
  switch (plan.kind) {
    case 'direct-tree':
    case 'direct-atomic':
      return plan.reservation.guarantees.profile
    case 'workspace-then-publish':
      return plan.publicationGuarantee
    case 'portable-handoff':
      return plan.publicationGuarantee
    case 'direct-resumable-zip':
      return plan.binding.guarantees.profile
  }
}

function legalArtifactChoiceTuple(input: {
  readonly artifactKind: ArtifactKind
  readonly materializationKind: MaterializationKind
  readonly guaranteeProfile: GuaranteeProfile
  readonly preparation: PreparationPolicy
}): boolean {
  switch (input.materializationKind) {
    case 'direct-tree':
      return input.artifactKind === 'directory-tree' && input.preparation === 'none' &&
        (input.guaranteeProfile === 'native-tree' || input.guaranteeProfile === 'fsa-tree')
    case 'direct-atomic':
      return input.artifactKind === 'original-file' && input.guaranteeProfile === 'managed-atomic' &&
        input.preparation === 'none'
    case 'workspace-then-publish':
      return (input.artifactKind === 'original-file' || input.artifactKind === 'zip-archive') &&
        (input.guaranteeProfile === 'managed-atomic' || input.guaranteeProfile === 'browser-handoff') &&
        input.preparation === (input.artifactKind === 'zip-archive' ? 'exact-zip' : 'none')
    case 'portable-handoff':
      return (input.artifactKind === 'original-file' || input.artifactKind === 'zip-archive') &&
        input.guaranteeProfile === 'browser-handoff' && input.preparation === 'exact-artifact'
    case 'direct-resumable-zip':
      return input.artifactKind === 'zip-archive' && input.guaranteeProfile === 'fsa-owned-file' &&
        input.preparation === 'none'
  }
}

function artifactKindByte(value: ArtifactKind): number {
  switch (value) {
    case 'original-file': return 1
    case 'directory-tree': return 2
    case 'zip-archive': return 3
  }
}

function materializationKindByte(value: MaterializationKind): number {
  switch (value) {
    case 'direct-tree': return 1
    case 'direct-atomic': return 2
    case 'workspace-then-publish': return 3
    case 'portable-handoff': return 4
    case 'direct-resumable-zip': return 5
  }
}

function guaranteeProfileByte(value: GuaranteeProfile): number {
  switch (value) {
    case 'native-tree': return 1
    case 'fsa-tree': return 2
    case 'managed-atomic': return 3
    case 'browser-handoff': return 4
    case 'fsa-owned-file': return 5
  }
}

function preparationByte(value: PreparationPolicy): number {
  switch (value) {
    case 'none': return 0
    case 'exact-zip': return 1
    case 'exact-artifact': return 2
  }
}

function artifactKindFromByte(value: number): ArtifactKind {
  switch (value) {
    case 1: return 'original-file'
    case 2: return 'directory-tree'
    case 3: return 'zip-archive'
    default: return invalidDecodedCanonicalBytes()
  }
}

function materializationKindFromByte(value: number): MaterializationKind {
  switch (value) {
    case 1: return 'direct-tree'
    case 2: return 'direct-atomic'
    case 3: return 'workspace-then-publish'
    case 4: return 'portable-handoff'
    case 5: return 'direct-resumable-zip'
    default: return invalidDecodedCanonicalBytes()
  }
}

function guaranteeProfileFromByte(value: number): GuaranteeProfile {
  switch (value) {
    case 1: return 'native-tree'
    case 2: return 'fsa-tree'
    case 3: return 'managed-atomic'
    case 4: return 'browser-handoff'
    case 5: return 'fsa-owned-file'
    default: return invalidDecodedCanonicalBytes()
  }
}

function preparationFromByte(value: number): PreparationPolicy {
  switch (value) {
    case 0: return 'none'
    case 1: return 'exact-zip'
    case 2: return 'exact-artifact'
    default: return invalidDecodedCanonicalBytes()
  }
}
