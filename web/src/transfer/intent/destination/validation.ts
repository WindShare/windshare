import {
  type GuaranteeSet,
  type NameAuthority,
} from '../model'

export function nativeTreeGuarantees(): GuaranteeSet {
  return guaranteeSet({
    profile: 'native-tree',
    nameAuthority: 'application-chosen',
    replacement: 'atomic-no-replace',
    delivery: 'managed-target',
    targetVisibility: 'committed-objects-visible',
    artifactAvailability: 'committed-objects-usable',
    cleanupAuthority: 'no-whole-target-rollback',
  })
}

export function fsaTreeGuarantees(): GuaranteeSet {
  return guaranteeSet({
    profile: 'fsa-tree',
    nameAuthority: 'application-chosen',
    replacement: 'coordinated-no-replace',
    delivery: 'managed-target',
    targetVisibility: 'committed-objects-visible',
    artifactAvailability: 'committed-objects-usable',
    cleanupAuthority: 'no-whole-target-rollback',
  })
}

export function managedAtomicGuarantees(
  nameAuthority: 'application-chosen' | 'user-chosen',
): GuaranteeSet {
  if (nameAuthority !== 'application-chosen' && nameAuthority !== 'user-chosen') {
    throw new TypeError('managed atomic name authority is invalid')
  }
  return guaranteeSet({
    profile: 'managed-atomic',
    nameAuthority,
    replacement: 'atomic-no-replace',
    delivery: 'managed-target',
    targetVisibility: 'hidden-until-verified-publication',
    artifactAvailability: 'verified-complete-only',
    cleanupAuthority: 'rollback-to-absent-before-publication',
  })
}

export function browserHandoffGuarantees(): GuaranteeSet {
  return guaranteeSet({
    profile: 'browser-handoff',
    nameAuthority: 'browser-chosen',
    replacement: 'unknown',
    delivery: 'browser-handoff',
    targetVisibility: 'unobservable',
    artifactAvailability: 'handoff-only',
    cleanupAuthority: 'no-managed-cleanup',
  })
}

export function fsaOwnedFileGuarantees(): GuaranteeSet & Readonly<{ profile: 'fsa-owned-file' }> {
  return guaranteeSet({
    profile: 'fsa-owned-file',
    nameAuthority: 'application-chosen',
    replacement: 'coordinated-no-replace',
    delivery: 'managed-target',
    targetVisibility: 'operation-owned-incomplete-file-visible',
    artifactAvailability: 'verified-complete-only',
    cleanupAuthority: 'ownership-proof-required',
  }) as GuaranteeSet & Readonly<{ profile: 'fsa-owned-file' }>
}

function guaranteeSet(value: GuaranteeSet): GuaranteeSet {
  return Object.freeze({ ...value })
}

export function snapshotGuarantees(input: GuaranteeSet): GuaranteeSet {
  let expected: GuaranteeSet
  switch (input.profile) {
    case 'native-tree':
      expected = nativeTreeGuarantees()
      break
    case 'fsa-tree':
      expected = fsaTreeGuarantees()
      break
    case 'managed-atomic':
      expected = managedAtomicGuarantees(managedNameAuthority(input.nameAuthority))
      break
    case 'browser-handoff':
      expected = browserHandoffGuarantees()
      break
    case 'fsa-owned-file':
      expected = fsaOwnedFileGuarantees()
      break
    default:
      throw new TypeError('guarantee profile is invalid')
  }
  if (!sameGuarantees(input, expected)) throw new TypeError('guarantee profile fields are invalid')
  return expected
}

export function sameGuarantees(left: GuaranteeSet, right: GuaranteeSet): boolean {
  return left.profile === right.profile &&
    left.nameAuthority === right.nameAuthority &&
    left.replacement === right.replacement &&
    left.delivery === right.delivery &&
    left.targetVisibility === right.targetVisibility &&
    left.artifactAvailability === right.artifactAvailability &&
    left.cleanupAuthority === right.cleanupAuthority
}

export function managedNameAuthority(
  value: NameAuthority,
): 'application-chosen' | 'user-chosen' {
  if (value !== 'application-chosen' && value !== 'user-chosen') {
    throw new TypeError('managed atomic name authority is invalid')
  }
  return value
}
