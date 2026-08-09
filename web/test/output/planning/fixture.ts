import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
  DEFAULT_PORTABLE_MAXIMUM_PARTS,
  browserHandoffGuarantees,
  fsaTreeGuarantees,
  managedAtomicGuarantees,
  nativeTreeGuarantees,
} from '../../../src/transfer/intent'
import type { SelectionSpec } from '../../../src/transfer/intent'
import type {
  ArtifactShapeProof,
  DiscoveryState,
  SelectionProjectionV1,
} from '../../../src/transfer/projection'
import { nextProjectionEpoch } from '../../../src/transfer/projection'
import {
  createEnvironmentOffers,
} from '../../../src/output/planning'
import type {
  DestinationGuaranteeFacts,
  EnvironmentOffers,
  EnvironmentTargetOfferInput,
  PortableEnvironmentOffer,
  WorkspaceEnvironmentOffer,
} from '../../../src/output/planning'

export const TEST_JOB_WORKSPACE_LIMIT = 8_589_934_592n
export const TEST_PROCESS_WORKSPACE_LIMIT = 17_179_869_184n
export const TEST_QUOTA_RESERVE = 536_870_912n

export function identity(seed: number, width = 16): string {
  const bytes = new Uint8Array(width)
  bytes[0] = seed
  bytes[bytes.length - 1] = seed ^ 0xff
  return encodeBase64Url(bytes)
}

export function nativeTarget(id = 'native'): EnvironmentTargetOfferInput {
  return {
    id,
    kind: 'native-directory-container',
    guarantees: guaranteeFacts(nativeTreeGuarantees()),
    persistence: 'durable-authority',
    hardMaximumOutputBytes: null,
  }
}

export function fsaTarget(id = 'fsa'): EnvironmentTargetOfferInput {
  return {
    id,
    kind: 'fsa-parent-directory',
    guarantees: guaranteeFacts(fsaTreeGuarantees()),
    persistence: 'durable-after-repository-commit',
    hardMaximumOutputBytes: null,
  }
}

export function managedTarget(
  id = 'managed',
  nameAuthority: 'application-chosen' | 'user-chosen' = 'application-chosen',
): EnvironmentTargetOfferInput {
  return {
    id,
    kind: 'managed-atomic-file-target',
    guarantees: guaranteeFacts(managedAtomicGuarantees(nameAuthority)),
    persistence: 'operation-scoped',
    hardMaximumOutputBytes: null,
  }
}

export function handoffTarget(id = 'handoff'): EnvironmentTargetOfferInput {
  return {
    id,
    kind: 'browser-handoff',
    guarantees: guaranteeFacts(browserHandoffGuarantees()),
    persistence: 'none',
    hardMaximumOutputBytes: null,
    objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
    supportsWorkspacePackage: true,
    supportsPortableArtifact: true,
  }
}

export function precreatedBrowserFileTarget(id = 'save-picker'): EnvironmentTargetOfferInput {
  return {
    id,
    kind: 'precreated-browser-file',
    guarantees: {
      nameAuthority: 'user-chosen',
      replacement: 'unknown',
      delivery: 'managed-target',
      visibility: 'unobservable',
      rollback: 'none',
    },
    persistence: 'operation-scoped',
    hardMaximumOutputBytes: null,
  }
}

export function workspaceOffer(id = 'workspace'): WorkspaceEnvironmentOffer {
  return {
    id,
    kind: 'origin-private-workspace',
    persistence: 'durable-owned-repository',
    jobHardLimitBytes: TEST_JOB_WORKSPACE_LIMIT,
    processHardLimitBytes: TEST_PROCESS_WORKSPACE_LIMIT,
    minimumQuotaReserveBytes: TEST_QUOTA_RESERVE,
    quotaAvailabilityEstimateBytes: null,
  }
}

export function portableOffer(id = 'portable'): PortableEnvironmentOffer {
  return {
    id,
    kind: 'portable-memory',
    persistence: 'none',
    maximumArtifactBytes: DEFAULT_PORTABLE_ARTIFACT_LIMIT,
    assemblyPartBytes: DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
    maximumParts: DEFAULT_PORTABLE_MAXIMUM_PARTS,
    objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  }
}

export function environment(input: Readonly<{
  targets?: readonly EnvironmentTargetOfferInput[]
  workspace?: WorkspaceEnvironmentOffer | null
  portable?: PortableEnvironmentOffer | null
}> = {}): EnvironmentOffers {
  return createEnvironmentOffers({
    targets: input.targets ?? [],
    workspace: input.workspace ?? null,
    portable: input.portable ?? null,
  })
}

export function projection(
  selection: SelectionSpec,
  proof: ArtifactShapeProof,
  byteCountLowerBound = 0n,
  epoch = 1n,
): SelectionProjectionV1 {
  const projectionEpoch = nextProjectionEpoch(epoch - 1n)
  const fileCountLowerBound = projectedFileCount(proof)
  const directoryCountLowerBound = proof.kind === 'tree' ? 1 : 0
  return {
    version: 1,
    epoch: projectionEpoch,
    selectionDigest: selection.digest,
    selectedRoots: proof.kind === 'tree' ? proof.selectedRoots : [],
    selectedRootCountLowerBound: proof.kind === 'tree' ? proof.selectedRootCountLowerBound : 0,
    selectedRootsTruncated: proof.kind === 'tree' && proof.selectedRootsTruncated,
    generations: [],
    metrics: { fileCountLowerBound, directoryCountLowerBound, byteCountLowerBound },
    unsettledTargets: proof.kind === 'unknown' ? [{ kind: 'synthetic-root', syntheticRoot: selection.syntheticRoot }] : [],
    proof,
  }
}

function projectedFileCount(proof: ArtifactShapeProof): number {
  if (proof.kind === 'single-file') return 1
  if (proof.kind === 'tree') return 2
  return 0
}

export function singleFileProof(): ArtifactShapeProof {
  return {
    kind: 'single-file',
    file: {
      fileId: identity(20),
      sourcePath: 'docs/report.txt',
      portableName: 'report.txt',
    },
  }
}

export function treeProof(
  layoutBasis: Extract<ArtifactShapeProof, { kind: 'tree' }>['layoutBasis'] = {
    kind: 'complete-directory',
    anchor: { directoryId: identity(30), sourcePath: 'photos' },
  },
): ArtifactShapeProof {
  return {
    kind: 'tree',
    selectedRoots: [{
      kind: 'directory',
      directoryId: identity(30),
      sourcePath: 'photos',
      portableName: 'photos',
    }],
    selectedRootCountLowerBound: 1,
    selectedRootsTruncated: false,
    layoutBasis,
  }
}

export const COMPLETE_DISCOVERY: DiscoveryState = Object.freeze({ kind: 'complete' })

function guaranteeFacts(input: DestinationGuaranteeFacts): DestinationGuaranteeFacts {
  return {
    nameAuthority: input.nameAuthority,
    replacement: input.replacement,
    delivery: input.delivery,
    visibility: input.visibility,
    rollback: input.rollback,
  }
}
