import { concat, frame } from '../canonical'
import {
  type ArtifactAvailability,
  type CleanupAuthority,
  type DestinationReservation,
  type GuaranteeSet,
  type NameAuthority,
  type ReplacementGuarantee,
  type TargetVisibility,
} from '../model'

export function canonicalGuarantees(value: GuaranteeSet): Uint8Array<ArrayBuffer> {
  return concat([
    frame(Uint8Array.of(guaranteeProfileByte(value.profile))),
    frame(Uint8Array.of(nameAuthorityByte(value.nameAuthority))),
    frame(Uint8Array.of(replacementGuaranteeByte(value.replacement))),
    frame(Uint8Array.of(value.delivery === 'managed-target' ? 1 : 2)),
    frame(Uint8Array.of(targetVisibilityByte(value.targetVisibility))),
    frame(Uint8Array.of(artifactAvailabilityByte(value.artifactAvailability))),
    frame(Uint8Array.of(cleanupAuthorityByte(value.cleanupAuthority))),
  ])
}

export function reservationKindByte(value: DestinationReservation['kind']): number {
  switch (value) {
    case 'container-root': return 1
    case 'named-container-entry': return 2
    case 'atomic-target': return 3
  }
}

export function authorityKindByte(value: DestinationReservation['authorityKind']): number {
  switch (value) {
    case 'native-container': return 1
    case 'fsa-container': return 2
    case 'managed-atomic-target': return 3
  }
}

function nameAuthorityByte(value: NameAuthority): number {
  switch (value) {
    case 'application-chosen': return 1
    case 'user-chosen': return 2
    case 'browser-chosen': return 3
  }
}

function replacementGuaranteeByte(value: ReplacementGuarantee): number {
  switch (value) {
    case 'atomic-no-replace': return 1
    case 'coordinated-no-replace': return 2
    case 'user-authorized-replace': return 3
    case 'unknown': return 4
  }
}

function guaranteeProfileByte(value: GuaranteeSet['profile']): number {
  switch (value) {
    case 'native-tree': return 1
    case 'fsa-tree': return 2
    case 'managed-atomic': return 3
    case 'browser-handoff': return 4
    case 'fsa-owned-file': return 5
  }
}

function targetVisibilityByte(value: TargetVisibility): number {
  switch (value) {
    case 'hidden-until-verified-publication': return 1
    case 'committed-objects-visible': return 2
    case 'unobservable': return 3
    case 'operation-owned-incomplete-file-visible': return 4
  }
}

function artifactAvailabilityByte(value: ArtifactAvailability): number {
  switch (value) {
    case 'verified-complete-only': return 1
    case 'committed-objects-usable': return 2
    case 'handoff-only': return 3
  }
}

function cleanupAuthorityByte(value: CleanupAuthority): number {
  switch (value) {
    case 'rollback-to-absent-before-publication': return 1
    case 'no-whole-target-rollback': return 2
    case 'ownership-proof-required': return 3
    case 'no-managed-cleanup': return 4
  }
}
