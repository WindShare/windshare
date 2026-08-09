import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
  DEFAULT_PORTABLE_MAXIMUM_PARTS,
  browserHandoffGuarantees,
  fsaTreeGuarantees,
  managedAtomicGuarantees,
  nativeTreeGuarantees,
} from '../../transfer/intent'
import type { GuaranteeProfile } from '../../transfer/intent'
import type {
  DestinationGuaranteeFacts,
  EnvironmentOffers,
  EnvironmentOffersInput,
  EnvironmentTargetKind,
  EnvironmentTargetOffer,
  EnvironmentTargetOfferInput,
  PortableEnvironmentOffer,
  TargetAuthorityPersistence,
  WorkspaceEnvironmentOffer,
} from './contracts'

const MAX_ENVIRONMENT_OFFER_ID_UTF8_BYTES = 128
const TEXT_ENCODER = new TextEncoder()
const VALID_ENVIRONMENT_OFFERS = new WeakSet<object>()

const PRECREATED_BROWSER_FILE_FACTS: DestinationGuaranteeFacts = Object.freeze({
  nameAuthority: 'user-chosen',
  replacement: 'unknown',
  delivery: 'managed-target',
  visibility: 'unobservable',
  rollback: 'none',
})

export function legalGuaranteeProfile(
  targetKind: EnvironmentTargetKind,
  facts: DestinationGuaranteeFacts,
): GuaranteeProfile | null {
  switch (targetKind) {
    case 'native-directory-container':
      return sameGuaranteeFacts(facts, nativeTreeGuarantees()) ? 'native-tree' : null
    case 'fsa-parent-directory':
      return sameGuaranteeFacts(facts, fsaTreeGuarantees()) ? 'fsa-tree' : null
    case 'managed-atomic-file-target':
      return isManagedAtomicFacts(facts) ? 'managed-atomic' : null
    case 'browser-handoff':
      return sameGuaranteeFacts(facts, browserHandoffGuarantees()) ? 'browser-handoff' : null
    case 'precreated-browser-file':
      return null
  }
}

export function createEnvironmentOffers(input: EnvironmentOffersInput): EnvironmentOffers {
  if (!Array.isArray(input.targets)) throw new TypeError('environment targets must be an array')
  const seen = new Set<string>()
  const targets = input.targets.map((target) => {
    const snapshot = snapshotTarget(target)
    if (seen.has(snapshot.id)) throw new TypeError('environment target identifiers must be unique')
    seen.add(snapshot.id)
    return snapshot
  }).sort(compareTarget)
  const workspace = input.workspace === undefined || input.workspace === null
    ? null
    : snapshotWorkspace(input.workspace)
  const portable = input.portable === undefined || input.portable === null
    ? null
    : snapshotPortable(input.portable)
  const result = Object.freeze({
    targets: Object.freeze(targets),
    workspace,
    portable,
  })
  VALID_ENVIRONMENT_OFFERS.add(result)
  return result
}

export function assertEnvironmentOffers(value: EnvironmentOffers): void {
  if (!VALID_ENVIRONMENT_OFFERS.has(value)) {
    throw new TypeError('environment offers must be created by the pure environment constructor')
  }
}

export function sameGuaranteeFacts(
  left: DestinationGuaranteeFacts,
  right: DestinationGuaranteeFacts,
): boolean {
  return left.nameAuthority === right.nameAuthority &&
    left.replacement === right.replacement &&
    left.delivery === right.delivery &&
    left.visibility === right.visibility &&
    left.rollback === right.rollback
}

export function sameTargetSemantics(
  left: EnvironmentTargetOffer,
  right: EnvironmentTargetOffer,
): boolean {
  if (left.kind !== right.kind ||
      left.persistence !== right.persistence ||
      left.hardMaximumOutputBytes !== right.hardMaximumOutputBytes ||
      left.legalProfile !== right.legalProfile ||
      !sameGuaranteeFacts(left.guarantees, right.guarantees)) return false
  if (left.kind !== 'browser-handoff' || right.kind !== 'browser-handoff') return true
  return left.objectUrlLeaseMilliseconds === right.objectUrlLeaseMilliseconds &&
    left.supportsWorkspacePackage === right.supportsWorkspacePackage &&
    left.supportsPortableArtifact === right.supportsPortableArtifact
}

export function outputLowerBoundFits(
  hardMaximumOutputBytes: bigint | null,
  byteCountLowerBound: bigint,
): boolean {
  return hardMaximumOutputBytes === null || byteCountLowerBound <= hardMaximumOutputBytes
}

function snapshotTarget(input: EnvironmentTargetOfferInput): EnvironmentTargetOffer {
  const id = requireOfferID(input.id)
  requireHardMaximum(input.hardMaximumOutputBytes)
  requireTargetPersistence(input.kind, input.persistence)
  const guarantees = snapshotGuaranteeFacts(input.guarantees)
  const legalProfile = legalGuaranteeProfile(input.kind, guarantees)
  if (input.kind === 'precreated-browser-file') {
    if (!sameGuaranteeFacts(guarantees, PRECREATED_BROWSER_FILE_FACTS)) {
      throw new TypeError('precreated browser file facts do not match showSaveFilePicker behavior')
    }
    return Object.freeze({ ...input, id, guarantees, legalProfile: null })
  }
  if (legalProfile === null) {
    throw new TypeError('environment target reports a contradictory guarantee combination')
  }
  if (input.kind === 'browser-handoff') {
    requireBoolean(input.supportsWorkspacePackage, 'workspace-package support')
    requireBoolean(input.supportsPortableArtifact, 'portable-artifact support')
    if (input.objectUrlLeaseMilliseconds !== BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS) {
      throw new TypeError('browser handoff lease does not match the frozen finite lease')
    }
  }
  return Object.freeze({ ...input, id, guarantees, legalProfile }) as EnvironmentTargetOffer
}

function snapshotWorkspace(input: WorkspaceEnvironmentOffer): WorkspaceEnvironmentOffer {
  const id = requireOfferID(input.id)
  if (input.kind !== 'origin-private-workspace' ||
      input.persistence !== 'durable-owned-repository') {
    throw new TypeError('workspace persistence facts are invalid')
  }
  requirePositiveBytes(input.jobHardLimitBytes, 'workspace job hard limit')
  requirePositiveBytes(input.processHardLimitBytes, 'workspace process hard limit')
  requirePositiveBytes(input.minimumQuotaReserveBytes, 'workspace quota reserve')
  if (input.jobHardLimitBytes > input.processHardLimitBytes) {
    throw new RangeError('workspace job hard limit exceeds the process hard limit')
  }
  if (input.quotaAvailabilityEstimateBytes !== null && input.quotaAvailabilityEstimateBytes < 0n) {
    throw new RangeError('workspace quota availability estimate must be non-negative')
  }
  return Object.freeze({ ...input, id })
}

function snapshotPortable(input: PortableEnvironmentOffer): PortableEnvironmentOffer {
  const id = requireOfferID(input.id)
  if (input.kind !== 'portable-memory' || input.persistence !== 'none' ||
      input.maximumArtifactBytes !== DEFAULT_PORTABLE_ARTIFACT_LIMIT ||
      input.assemblyPartBytes !== DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES ||
      input.maximumParts !== DEFAULT_PORTABLE_MAXIMUM_PARTS ||
      input.objectUrlLeaseMilliseconds !== BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS) {
    throw new TypeError('portable environment facts do not match the bounded portable contract')
  }
  return Object.freeze({ ...input, id })
}

function isManagedAtomicFacts(facts: DestinationGuaranteeFacts): boolean {
  if (facts.nameAuthority !== 'application-chosen' && facts.nameAuthority !== 'user-chosen') {
    return false
  }
  return sameGuaranteeFacts(facts, managedAtomicGuarantees(facts.nameAuthority))
}

function snapshotGuaranteeFacts(input: DestinationGuaranteeFacts): DestinationGuaranteeFacts {
  const snapshot = {
    nameAuthority: input.nameAuthority,
    replacement: input.replacement,
    delivery: input.delivery,
    visibility: input.visibility,
    rollback: input.rollback,
  } as const
  for (const value of Object.values(snapshot)) {
    if (typeof value !== 'string') throw new TypeError('destination guarantee facts are invalid')
  }
  return Object.freeze(snapshot)
}

function requireTargetPersistence(
  kind: EnvironmentTargetKind,
  persistence: TargetAuthorityPersistence,
): void {
  const expected: Record<EnvironmentTargetKind, TargetAuthorityPersistence> = {
    'native-directory-container': 'durable-authority',
    'fsa-parent-directory': 'durable-after-repository-commit',
    'managed-atomic-file-target': 'operation-scoped',
    'browser-handoff': 'none',
    'precreated-browser-file': 'operation-scoped',
  }
  if (persistence !== expected[kind]) throw new TypeError('environment target persistence fact is invalid')
}

function requireOfferID(value: string): string {
  if (typeof value !== 'string' || value.length === 0 ||
      TEXT_ENCODER.encode(value).byteLength > MAX_ENVIRONMENT_OFFER_ID_UTF8_BYTES) {
    throw new TypeError('environment offer identifier is invalid')
  }
  return value
}

function requireHardMaximum(value: bigint | null): void {
  if (value !== null) requirePositiveBytes(value, 'target output hard limit')
}

function requirePositiveBytes(value: bigint, label: string): void {
  if (typeof value !== 'bigint' || value <= 0n) throw new RangeError(label + ' must be positive')
}

function requireBoolean(value: boolean, label: string): void {
  if (typeof value !== 'boolean') throw new TypeError(label + ' must be boolean')
}

function compareTarget(left: EnvironmentTargetOffer, right: EnvironmentTargetOffer): number {
  const kindOrder = targetKindOrder(left.kind) - targetKindOrder(right.kind)
  if (kindOrder !== 0 || left.id === right.id) return kindOrder
  return left.id < right.id ? -1 : 1
}

function targetKindOrder(value: EnvironmentTargetKind): number {
  switch (value) {
    case 'native-directory-container': return 1
    case 'fsa-parent-directory': return 2
    case 'managed-atomic-file-target': return 3
    case 'browser-handoff': return 4
    case 'precreated-browser-file': return 5
  }
}
