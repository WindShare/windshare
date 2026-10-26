import {
  type GuaranteeSet,
  type NameChooser,
} from '../model'

export function nativeTreeGuarantees(): GuaranteeSet {
  return guaranteeSet({
    profile: 'native-tree',
    nameChooser: 'application-chosen',
    replacement: 'atomic-no-replace',
    delivery: 'managed-target',
    targetVisibility: 'committed-objects-visible',
    artifactAvailability: 'committed-objects-usable',
    cleanupContract: 'no-whole-target-rollback',
  })
}

export function fsaTreeGuarantees(): GuaranteeSet {
  return guaranteeSet({
    profile: 'fsa-tree',
    nameChooser: 'application-chosen',
    replacement: 'coordinated-no-replace',
    delivery: 'managed-target',
    targetVisibility: 'committed-objects-visible',
    artifactAvailability: 'committed-objects-usable',
    cleanupContract: 'no-whole-target-rollback',
  })
}

export function managedAtomicGuarantees(
  nameChooser: 'application-chosen' | 'user-chosen',
): GuaranteeSet {
  if (nameChooser !== 'application-chosen' && nameChooser !== 'user-chosen') {
    throw new TypeError('managed atomic name chooser is invalid')
  }
  return guaranteeSet({
    profile: 'managed-atomic',
    nameChooser,
    replacement: 'atomic-no-replace',
    delivery: 'managed-target',
    targetVisibility: 'hidden-until-verified-publication',
    artifactAvailability: 'verified-complete-only',
    cleanupContract: 'rollback-to-absent-before-publication',
  })
}

export function browserHandoffGuarantees(): GuaranteeSet {
  return guaranteeSet({
    profile: 'browser-handoff',
    nameChooser: 'browser-chosen',
    replacement: 'unknown',
    delivery: 'browser-handoff',
    targetVisibility: 'unobservable',
    artifactAvailability: 'handoff-only',
    cleanupContract: 'no-managed-cleanup',
  })
}

export function fsaOwnedFileGuarantees(): GuaranteeSet & Readonly<{ profile: 'fsa-owned-file' }> {
  return guaranteeSet({
    profile: 'fsa-owned-file',
    nameChooser: 'application-chosen',
    replacement: 'coordinated-no-replace',
    delivery: 'managed-target',
    targetVisibility: 'operation-owned-incomplete-file-visible',
    artifactAvailability: 'verified-complete-only',
    cleanupContract: 'ownership-proof-required',
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
      expected = managedAtomicGuarantees(managedNameChooser(input.nameChooser))
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
    left.nameChooser === right.nameChooser &&
    left.replacement === right.replacement &&
    left.delivery === right.delivery &&
    left.targetVisibility === right.targetVisibility &&
    left.artifactAvailability === right.artifactAvailability &&
    left.cleanupContract === right.cleanupContract
}

export function managedNameChooser(
  value: NameChooser,
): 'application-chosen' | 'user-chosen' {
  if (value !== 'application-chosen' && value !== 'user-chosen') {
    throw new TypeError('managed atomic name chooser is invalid')
  }
  return value
}
