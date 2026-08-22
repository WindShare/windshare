import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  browserHandoffGuarantees,
  fsaTreeGuarantees,
} from '../../transfer/intent'
import {
  probeBrowserHandoffCapabilities,
  type BrowserHandoffCapabilityFacts,
} from '../portable/packaged-handoff'
import {
  createEnvironmentOffers,
  sameGuaranteeFacts,
  type BrowserHandoffTargetOffer,
  type EnvironmentOffersInput,
  type FSADirectoryContainerOffer,
} from '../planning'
import {
  BROWSER_HANDOFF_TARGET_ROUTE_ID,
  FSA_PARENT_DIRECTORY_ROUTE_ID,
  type AcquiredFSAParentAuthority,
  type AuthorityAcquiredDecision,
  type BrowserCapabilityRuntime,
  type BrowserEnvironmentSnapshot,
  type CapabilityTrace,
} from './contract'

const READ_WRITE_PERMISSION = Object.freeze({ mode: 'readwrite' as const })

interface PermissionCapableDirectoryHandle extends FileSystemDirectoryHandle {
  queryPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
  requestPermission?(descriptor?: { readonly mode?: 'read' | 'readwrite' }): Promise<PermissionState>
}

/**
 * Capability detection reports hard browser facts only. Artifact choice remains in
 * output/planning, so gaining or losing FSA cannot silently select a ZIP route.
 */
export function probeBrowserEnvironment(
  runtime: BrowserCapabilityRuntime,
  supplemental: Omit<EnvironmentOffersInput, 'targets'> & {
    readonly targets?: EnvironmentOffersInput['targets']
  } = {},
): BrowserEnvironmentSnapshot {
  const fsaParent = runtime.showDirectoryPicker === undefined ? null : fsaParentOffer()
  const handoffFacts = runtime.browserHandoff === undefined
    ? null
    : probeBrowserHandoffCapabilities(runtime.browserHandoff)
  const browserHandoff = handoffFacts === null ||
      (!handoffFacts.supportsWorkspacePackage && !handoffFacts.supportsPortableArtifact)
    ? null
    : browserHandoffOffer(handoffFacts)
  const offers = createEnvironmentOffers({
    targets: Object.freeze([
      ...(supplemental.targets ?? []),
      ...(fsaParent === null ? [] : [fsaParent]),
      ...(browserHandoff === null ? [] : [browserHandoff]),
    ]),
    ...(supplemental.workspace === undefined ? {} : { workspace: supplemental.workspace }),
    ...(supplemental.portable === undefined ? {} : { portable: supplemental.portable }),
    ...(supplemental.directZipSupport === undefined
      ? {}
      : { directZipSupport: supplemental.directZipSupport }),
    ...(supplemental.zipRecommendationPolicy === undefined
      ? {}
      : { zipRecommendationPolicy: supplemental.zipRecommendationPolicy }),
  })
  return Object.freeze({ offers, fsaParent, browserHandoff })
}

export function browserHandoffOffer(
  facts: BrowserHandoffCapabilityFacts,
  routeId = BROWSER_HANDOFF_TARGET_ROUTE_ID,
): BrowserHandoffTargetOffer {
  const guarantees = browserHandoffGuarantees()
  const offers = createEnvironmentOffers({
    targets: [{
      routeId,
      kind: 'browser-handoff',
      guarantees: {
        nameAuthority: guarantees.nameAuthority,
        replacement: guarantees.replacement,
        delivery: guarantees.delivery,
        targetVisibility: guarantees.targetVisibility,
        artifactAvailability: guarantees.artifactAvailability,
        cleanupAuthority: guarantees.cleanupAuthority,
      },
      persistence: 'none',
      hardMaximumOutputBytes: null,
      objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
      supportsWorkspacePackage: facts.supportsWorkspacePackage,
      supportsPortableArtifact: facts.supportsPortableArtifact,
    }],
  })
  const target = offers.targets[0]
  if (target?.kind !== 'browser-handoff') {
    throw new TypeError('Browser handoff environment offer construction failed')
  }
  return target
}

export function fsaParentOffer(
  routeId = FSA_PARENT_DIRECTORY_ROUTE_ID,
): FSADirectoryContainerOffer {
  const guarantees = fsaTreeGuarantees()
  const offers = createEnvironmentOffers({
    targets: [{
      routeId,
      kind: 'fsa-parent-directory',
      guarantees: {
        nameAuthority: guarantees.nameAuthority,
        replacement: guarantees.replacement,
        delivery: guarantees.delivery,
        targetVisibility: guarantees.targetVisibility,
        artifactAvailability: guarantees.artifactAvailability,
        cleanupAuthority: guarantees.cleanupAuthority,
      },
      persistence: 'durable-after-repository-commit',
      hardMaximumOutputBytes: null,
    }],
  })
  const target = offers.targets[0]
  if (target?.kind !== 'fsa-parent-directory') {
    throw new TypeError('FSA environment offer construction failed')
  }
  return target
}

/**
 * This function deliberately is not async: showDirectoryPicker is invoked before the
 * first promise continuation, preserving the browser's user-activation boundary.
 */
export function startFSAParentPicker(
  runtime: BrowserCapabilityRuntime,
  offer: FSADirectoryContainerOffer,
  trace: CapabilityTrace = () => undefined,
): Promise<AcquiredFSAParentAuthority> {
  assertFSAOffer(offer)
  const picker = runtime.showDirectoryPicker
  if (picker === undefined) {
    return Promise.reject(new DOMException(
      'Directory output is unavailable in this browser',
      'NotSupportedError',
    ))
  }
  const picked = picker(READ_WRITE_PERMISSION)
  return picked.then((parent) => {
    if (parent.kind !== 'directory') {
      throw new TypeError('The directory picker returned a non-directory authority')
    }
    emitCapabilityTrace(trace, authorityDecision())
    return Object.freeze({
      kind: 'fsa-parent-directory-authority' as const,
      targetRouteId: offer.routeId,
      offer,
      parent,
    })
  })
}

export async function authorizeFSAParent(
  authority: AcquiredFSAParentAuthority,
): Promise<void> {
  assertFSAOffer(authority.offer)
  if (authority.targetRouteId !== authority.offer.routeId ||
      authority.parent.kind !== 'directory') {
    throw new TypeError('Acquired FSA authority does not match its environment offer')
  }
  const parent = authority.parent as PermissionCapableDirectoryHandle
  if (parent.queryPermission === undefined) return
  const current = await parent.queryPermission(READ_WRITE_PERMISSION)
  if (current === 'granted') return
  if (parent.requestPermission === undefined ||
      await parent.requestPermission(READ_WRITE_PERMISSION) !== 'granted') {
    throw new DOMException('Directory output permission was not granted', 'NotAllowedError')
  }
}

function assertFSAOffer(offer: FSADirectoryContainerOffer): void {
  const expected = fsaTreeGuarantees()
  if (offer.kind !== 'fsa-parent-directory' || offer.legalProfile !== 'fsa-tree' ||
      offer.persistence !== 'durable-after-repository-commit' ||
      offer.hardMaximumOutputBytes !== null || !sameGuaranteeFacts(offer.guarantees, expected)) {
    throw new TypeError('FSA parent authority must use the frozen fsa-tree guarantees')
  }
}

function authorityDecision(): AuthorityAcquiredDecision {
  return Object.freeze({
    name: 'receive.authority.acquired',
    operation_id_present: false,
    authority_kind: 'fsa-container',
    name_authority: 'application-chosen',
    replacement_guarantee: 'coordinated-no-replace',
    delivery_mode: 'managed-target',
    commit_visibility: 'prefix-visible',
    rollback_guarantee: 'none',
  })
}

function emitCapabilityTrace(trace: CapabilityTrace, decision: AuthorityAcquiredDecision): void {
  try {
    trace(decision)
  } catch {
    // Telemetry cannot revoke a user-granted authority after the picker succeeds.
  }
}
