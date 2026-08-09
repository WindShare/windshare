import {
  canonicalizePortableCatalogPath,
  catalogNameCollisionKey,
  isPortableCatalogName,
  V2_CATALOG_NAME_BYTES,
  V2_CATALOG_PATH_BYTES,
  V2_CATALOG_PATH_DEPTH,
} from '../catalog/path-policy'
import { V2_MAXIMUM_SELECTION_RULE_OVERRIDES } from '../catalog/v2-selection'
import { decodeBase64Url, encodeBase64Url, equalBytes } from '../crypto/bytes'
import { sha256 } from '../crypto/digest'
import type {
  V2CanonicalSelectionRule,
  V2FrozenSelectionPolicy,
} from '../catalog/v2-selection'

type CanonicalBytes = Uint8Array<ArrayBuffer>

export const RECEIVE_INTENT_VERSION = 1 as const
export const SELECTION_SPEC_VERSION = 1 as const
export const ARTIFACT_SPEC_VERSION = 1 as const
export const MATERIALIZATION_PLAN_VERSION = 1 as const
export const DESTINATION_RESERVATION_VERSION = 1 as const
export const WORKSPACE_BINDING_VERSION = 1 as const
export const PORTABLE_BINDING_VERSION = 1 as const

export const STABLE_IDENTITY_BYTES = 16
export const AUTHORITY_REFERENCE_BYTES = 32
export const RECEIVE_INTENT_DIGEST_BYTES = 32
export const MAX_SELECTION_RULES = V2_MAXIMUM_SELECTION_RULE_OVERRIDES
export const MAX_SELECTION_TARGET_UTF8_BYTES = 1 << 20
export const DEFAULT_RESULT_ROOT_NAME = 'windshare'
export const DEFAULT_ARCHIVE_NAME = 'windshare.zip'
export const PARTIAL_SELECTION_SUFFIX = '-selection'
export const ARCHIVE_EXTENSION = '.zip'
export const RESULT_NAME_POLICY = 'windshare/result-name/v1-unicode-15.0.0'
export const MAX_RESULT_COMPONENT_BYTES = V2_CATALOG_NAME_BYTES
export const COLLISION_SUFFIX_HEX_CHARS = 10
export const DEFAULT_PORTABLE_ARTIFACT_LIMIT = 67_108_864n
export const DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES = 1_048_576n
export const DEFAULT_PORTABLE_MAXIMUM_PARTS = 64n
export const BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS = 60_000n

const TEXT_ENCODER = new TextEncoder()
const TEXT_DECODER = new TextDecoder('utf-8', { fatal: true })
const CANONICAL_SCHEMA_VERSION = 1
const MAX_CANONICAL_PATH_ENCODING_BYTES = 8 +
  V2_CATALOG_PATH_DEPTH * 8 + V2_CATALOG_PATH_BYTES
const INVALID_RECEIVE_INTENT_CANONICAL_BYTES = 'receive intent canonical bytes are invalid'
const SELECTION_SPEC_DOMAIN = 'windshare/selection-spec/v1'
const ARTIFACT_SPEC_DOMAIN = 'windshare/artifact-spec/v1'
const RESULT_ROOT_LAYOUT_DOMAIN = 'windshare/result-root-layout/v1'
const DESTINATION_RESERVATION_DOMAIN = 'windshare/destination-reservation/v1'
const WORKSPACE_BINDING_DOMAIN = 'windshare/workspace-binding/v1'
const PORTABLE_BINDING_DOMAIN = 'windshare/portable-binding/v1'
const MATERIALIZATION_PLAN_DOMAIN = 'windshare/materialization-plan/v1'
const RECEIVE_INTENT_DOMAIN = 'windshare/receive-intent/v1'
const NAME_COLLISION_DOMAIN = 'windshare/name-collision/v1'
const VALID_CANONICAL_VALUES = new WeakSet<object>()

export interface CanonicalValue {
  readonly canonicalBytes: CanonicalBytes
}

export interface CanonicalDigestValue extends CanonicalValue {
  readonly digest: string
}

export type SelectionMode = 'node-id' | 'catalog-path'
export type SelectionRuleKind = 'directory' | 'file'

export interface NodeSelectionRule {
  readonly kind: SelectionRuleKind
  readonly id: string
  readonly selected: boolean
}

export interface NodeIDSelectionRules {
  readonly mode: 'node-id'
  readonly defaultSelected: boolean
  readonly rules: readonly NodeSelectionRule[]
}

export interface CatalogPathSelectionRules {
  readonly mode: 'catalog-path'
  readonly defaultSelected: false
  readonly paths: readonly string[]
}

export type SelectionRulesSpec = NodeIDSelectionRules | CatalogPathSelectionRules

export interface SelectionSpec extends CanonicalDigestValue {
  readonly version: typeof SELECTION_SPEC_VERSION
  readonly shareInstance: string
  readonly syntheticRoot: string
  readonly rules: SelectionRulesSpec
}

export type ResultRootClass =
  | 'complete-directory'
  | 'directory-selection'
  | 'synthetic-selection'

export interface DirectoryResultRootLayout extends CanonicalValue {
  readonly class: 'complete-directory' | 'directory-selection'
  readonly anchor: Readonly<{
    kind: 'directory'
    directoryId: string
    sourcePath: string
  }>
  readonly name: string
}

export interface SyntheticResultRootLayout extends CanonicalValue {
  readonly class: 'synthetic-selection'
  readonly anchor: Readonly<{ kind: 'synthetic-root' }>
  readonly name: typeof DEFAULT_RESULT_ROOT_NAME
}

export type ResultRootLayout = DirectoryResultRootLayout | SyntheticResultRootLayout

export interface SingleFileDirectoryTreeLayout extends CanonicalValue {
  readonly kind: 'single-file'
  readonly fileId: string
  readonly sourcePath: string
  readonly outputName: string
}

export interface ResultRootDirectoryTreeLayout extends CanonicalValue {
  readonly kind: 'result-root'
  readonly root: ResultRootLayout
}

export interface CatalogRootDirectoryTreeLayout extends CanonicalValue {
  readonly kind: 'catalog-root'
}

export type DirectoryTreeLayout =
  | SingleFileDirectoryTreeLayout
  | ResultRootDirectoryTreeLayout
  | CatalogRootDirectoryTreeLayout

interface ArtifactSpecBase extends CanonicalDigestValue {
  readonly version: typeof ARTIFACT_SPEC_VERSION
}

export interface OriginalFileArtifact extends ArtifactSpecBase {
  readonly kind: 'original-file'
  readonly fileId: string
  readonly sourcePath: string
  readonly suggestedName: string
}

export interface DirectoryTreeArtifact extends ArtifactSpecBase {
  readonly kind: 'directory-tree'
  readonly layout: DirectoryTreeLayout
}

export interface ZipArchiveArtifact extends ArtifactSpecBase {
  readonly kind: 'zip-archive'
  readonly layout: ResultRootLayout
  readonly suggestedName: string
  readonly encoding: 'store'
  readonly completeness: 'complete-only'
}

export type ArtifactSpec =
  | OriginalFileArtifact
  | DirectoryTreeArtifact
  | ZipArchiveArtifact

export type NameAuthority = 'application-chosen' | 'user-chosen' | 'browser-chosen'
export type ReplacementGuarantee =
  | 'atomic-no-replace'
  | 'coordinated-no-replace'
  | 'user-authorized-replace'
  | 'unknown'
export type DeliveryMode = 'managed-target' | 'browser-handoff'
export type CommitVisibility = 'atomic-commit' | 'prefix-visible' | 'unobservable'
export type RollbackGuarantee = 'to-absent' | 'none'
export type GuaranteeProfile =
  | 'native-tree'
  | 'fsa-tree'
  | 'managed-atomic'
  | 'browser-handoff'

export interface GuaranteeSet {
  readonly profile: GuaranteeProfile
  readonly nameAuthority: NameAuthority
  readonly replacement: ReplacementGuarantee
  readonly delivery: DeliveryMode
  readonly visibility: CommitVisibility
  readonly rollback: RollbackGuarantee
}

interface DestinationReservationBase extends CanonicalDigestValue {
  readonly version: typeof DESTINATION_RESERVATION_VERSION
  readonly operationId: string
  readonly reservationId: string
  readonly artifactDigest: string
  readonly authorityKind: 'native-container' | 'fsa-container' | 'managed-atomic-target'
  readonly authorityRef: string
  readonly guarantees: GuaranteeSet
}

export interface ContainerRootReservation extends DestinationReservationBase {
  readonly kind: 'container-root'
  readonly authorityKind: 'native-container'
}

export interface NamedContainerEntryReservation extends DestinationReservationBase {
  readonly kind: 'named-container-entry'
  readonly authorityKind: 'native-container' | 'fsa-container'
  readonly entryKind: 'single-file' | 'result-root'
  readonly requestedName: string
  readonly reservedName: string
  readonly collisionIndex: number
}

export interface AtomicTargetReservation extends DestinationReservationBase {
  readonly kind: 'atomic-target'
  readonly authorityKind: 'managed-atomic-target'
  readonly requestedName: string
  readonly reservedName: string
  readonly collisionIndex: number
}

export type DestinationReservation =
  | ContainerRootReservation
  | NamedContainerEntryReservation
  | AtomicTargetReservation

export interface WorkspaceBinding extends CanonicalDigestValue {
  readonly version: typeof WORKSPACE_BINDING_VERSION
  readonly operationId: string
  readonly workspaceId: string
  readonly artifactDigest: string
  readonly repositoryRef: string
  readonly workspaceKind: 'origin-private'
  readonly budgetPolicy: 'workspace-v1'
  readonly retentionPolicy: 'stable-24h-v1'
}

export interface PortableBinding extends CanonicalDigestValue {
  readonly version: typeof PORTABLE_BINDING_VERSION
  readonly operationId: string
  readonly portablePlanId: string
  readonly artifactDigest: string
  readonly maximumArtifactBytes: typeof DEFAULT_PORTABLE_ARTIFACT_LIMIT
  readonly assemblyPartBytes: typeof DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES
  readonly maximumParts: typeof DEFAULT_PORTABLE_MAXIMUM_PARTS
  readonly objectUrlLeaseMilliseconds: typeof BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS
  readonly preparation: 'exact-artifact'
}

interface MaterializationPlanBase extends CanonicalValue {
  readonly version: typeof MATERIALIZATION_PLAN_VERSION
}

export interface DirectTreePlan extends MaterializationPlanBase {
  readonly kind: 'direct-tree'
  readonly reservation: DestinationReservation
  readonly preparation: 'none'
}

export interface DirectAtomicPlan extends MaterializationPlanBase {
  readonly kind: 'direct-atomic'
  readonly reservation: AtomicTargetReservation
  readonly preparation: 'none'
}

export interface WorkspaceThenPublishPlan extends MaterializationPlanBase {
  readonly kind: 'workspace-then-publish'
  readonly workspace: WorkspaceBinding
  readonly preparation: 'none' | 'exact-zip'
}

export interface PortableHandoffPlan extends MaterializationPlanBase {
  readonly kind: 'portable-handoff'
  readonly portable: PortableBinding
  readonly publicationRoute: 'browser-handoff'
  readonly preparation: 'exact-artifact'
}

export type MaterializationPlan =
  | DirectTreePlan
  | DirectAtomicPlan
  | WorkspaceThenPublishPlan
  | PortableHandoffPlan

export interface ReceiveIntent extends CanonicalDigestValue {
  readonly version: typeof RECEIVE_INTENT_VERSION
  readonly selection: SelectionSpec
  readonly artifact: ArtifactSpec
  readonly plan: MaterializationPlan
  readonly shareInstance: string
  readonly syntheticRoot: string
  readonly operationId: string
  readonly bindingDigest: string
}

export function createOperationID(): string {
  return createRandomIdentity('operation')
}

export function createDestinationReservationID(): string {
  return createRandomIdentity('destination reservation')
}

export function createWorkspaceID(): string {
  return createRandomIdentity('workspace')
}

export function createPortablePlanID(): string {
  return createRandomIdentity('portable plan')
}

export function createTransferJobID(): string {
  return createRandomIdentity('transfer job')
}

export function createOutputSessionID(): string {
  return createRandomIdentity('output session')
}

export function selectionRulesSpecFromPolicy(
  policy: V2FrozenSelectionPolicy,
): NodeIDSelectionRules {
  return snapshotSelectionRules({
    mode: 'node-id',
    defaultSelected: policy.defaultSelected,
    rules: policy.canonicalRules.map(selectionRuleFromPolicy),
  }) as NodeIDSelectionRules
}

function selectionRuleFromPolicy(rule: V2CanonicalSelectionRule): NodeSelectionRule {
  return {
    kind: rule.kind,
    id: encodeBase64Url(rule.id),
    selected: rule.selected,
  }
}

export async function createSelectionSpec(input: {
  readonly shareInstance: string
  readonly syntheticRoot: string
  readonly rules: SelectionRulesSpec
}): Promise<SelectionSpec> {
  const shareInstance = requireIdentity(input.shareInstance, STABLE_IDENTITY_BYTES, 'share instance')
  const syntheticRoot = requireIdentity(input.syntheticRoot, STABLE_IDENTITY_BYTES, 'synthetic root')
  const rules = snapshotSelectionRules(input.rules)
  const canonicalBytes = canonicalSelectionSpecBytes({ shareInstance, syntheticRoot, rules })
  return canonicalDigestValue({
    version: SELECTION_SPEC_VERSION,
    shareInstance,
    syntheticRoot,
    rules,
  }, await digestText(canonicalBytes), canonicalBytes)
}

export function canonicalSelectionSpecBytes(input: {
  readonly shareInstance: string
  readonly syntheticRoot: string
  readonly rules: SelectionRulesSpec
}): CanonicalBytes {
  const share = requireIdentityBytes(input.shareInstance, STABLE_IDENTITY_BYTES, 'share instance')
  const root = requireIdentityBytes(input.syntheticRoot, STABLE_IDENTITY_BYTES, 'synthetic root')
  const rules = snapshotSelectionRules(input.rules)
  const fields: Uint8Array[] = [
    frame(share),
    frame(root),
    frame(Uint8Array.of(rules.mode === 'node-id' ? 1 : 2)),
    frame(Uint8Array.of(rules.defaultSelected ? 1 : 0)),
  ]
  if (rules.mode === 'node-id') {
    fields.push(uint64(BigInt(rules.rules.length)))
    for (const rule of rules.rules) {
      fields.push(frame(Uint8Array.of(rule.kind === 'directory' ? 1 : 2)))
      fields.push(frame(requireIdentityBytes(rule.id, STABLE_IDENTITY_BYTES, 'selection rule identity')))
      fields.push(frame(Uint8Array.of(rule.selected ? 1 : 0)))
    }
  } else {
    fields.push(uint64(BigInt(rules.paths.length)))
    for (const path of rules.paths) fields.push(frame(TEXT_ENCODER.encode(path)))
  }
  return canonicalRecord(SELECTION_SPEC_DOMAIN, fields)
}

export async function validateSelectionSpec(input: SelectionSpec): Promise<SelectionSpec> {
  if (input.version !== SELECTION_SPEC_VERSION) throw new TypeError('selection spec version is invalid')
  const rebuilt = await createSelectionSpec(input)
  return requireSameDigestRecord(input, rebuilt, 'selection spec')
}

export function createCompleteDirectoryResultRoot(
  directoryId: string,
  sourcePath: string,
): DirectoryResultRootLayout {
  return createDirectoryResultRoot('complete-directory', directoryId, sourcePath)
}

export function createDirectorySelectionResultRoot(
  directoryId: string,
  sourcePath: string,
): DirectoryResultRootLayout {
  return createDirectoryResultRoot('directory-selection', directoryId, sourcePath)
}

export function createSyntheticSelectionResultRoot(): SyntheticResultRootLayout {
  const anchor = Object.freeze({ kind: 'synthetic-root' as const })
  const canonicalBytes = canonicalRecord(RESULT_ROOT_LAYOUT_DOMAIN, [
    frame(Uint8Array.of(3)),
    frame(Uint8Array.of(2)),
    frame(TEXT_ENCODER.encode(DEFAULT_RESULT_ROOT_NAME)),
  ])
  return canonicalValue({
    class: 'synthetic-selection' as const,
    anchor,
    name: DEFAULT_RESULT_ROOT_NAME,
  }, canonicalBytes)
}

function createDirectoryResultRoot(
  rootClass: 'complete-directory' | 'directory-selection',
  directoryIdInput: string,
  sourcePathInput: string,
): DirectoryResultRootLayout {
  const directoryId = requireIdentity(directoryIdInput, STABLE_IDENTITY_BYTES, 'result-root directory')
  const sourcePath = requireCanonicalPath(sourcePathInput)
  const leaf = sourcePath.split('/').at(-1)
  if (leaf === undefined) throw new TypeError('result-root path has no leaf')
  const name = rootClass === 'complete-directory'
    ? requireResultName(leaf)
    : appendProtectedSuffix(leaf, PARTIAL_SELECTION_SUFFIX)
  const anchor = Object.freeze({
    kind: 'directory' as const,
    directoryId,
    sourcePath,
  })
  const anchorBytes = concat([
    Uint8Array.of(1),
    frame(requireIdentityBytes(directoryId, STABLE_IDENTITY_BYTES, 'result-root directory')),
    frame(canonicalPathBytes(sourcePath)),
  ])
  const canonicalBytes = canonicalRecord(RESULT_ROOT_LAYOUT_DOMAIN, [
    frame(Uint8Array.of(rootClass === 'complete-directory' ? 1 : 2)),
    frame(anchorBytes),
    frame(TEXT_ENCODER.encode(name)),
  ])
  return canonicalValue({ class: rootClass, anchor, name }, canonicalBytes)
}

export function validateResultRootLayout(input: ResultRootLayout): ResultRootLayout {
  let rebuilt: ResultRootLayout
  switch (input.class) {
    case 'complete-directory':
      if (input.anchor.kind !== 'directory') throw new TypeError('complete result root requires a directory anchor')
      rebuilt = createCompleteDirectoryResultRoot(input.anchor.directoryId, input.anchor.sourcePath)
      break
    case 'directory-selection':
      if (input.anchor.kind !== 'directory') throw new TypeError('selection result root requires a directory anchor')
      rebuilt = createDirectorySelectionResultRoot(input.anchor.directoryId, input.anchor.sourcePath)
      break
    case 'synthetic-selection':
      if (input.anchor.kind !== 'synthetic-root' || input.name !== DEFAULT_RESULT_ROOT_NAME) {
        throw new TypeError('synthetic result root is invalid')
      }
      rebuilt = createSyntheticSelectionResultRoot()
      break
    default:
      throw new TypeError('result-root class is invalid')
  }
  if (input.name !== rebuilt.name) throw new TypeError('result-root name is not canonical')
  return requireSameCanonicalValue(input, rebuilt, 'result-root layout')
}

export async function createOriginalFileArtifact(input: {
  readonly fileId: string
  readonly sourcePath: string
  readonly suggestedName: string
}): Promise<OriginalFileArtifact> {
  const fileId = requireIdentity(input.fileId, STABLE_IDENTITY_BYTES, 'artifact file')
  const sourcePath = requireCanonicalPath(input.sourcePath)
  const suggestedName = requireResultName(input.suggestedName)
  if (sourcePath.split('/').at(-1) !== suggestedName) {
    throw new TypeError('original-file suggested name must equal the source-path leaf')
  }
  const canonicalBytes = canonicalRecord(ARTIFACT_SPEC_DOMAIN, [
    Uint8Array.of(1),
    frame(requireIdentityBytes(fileId, STABLE_IDENTITY_BYTES, 'artifact file')),
    frame(canonicalPathBytes(sourcePath)),
    frame(TEXT_ENCODER.encode(suggestedName)),
  ])
  return canonicalDigestValue({
    version: ARTIFACT_SPEC_VERSION,
    kind: 'original-file' as const,
    fileId,
    sourcePath,
    suggestedName,
  }, await digestText(canonicalBytes), canonicalBytes)
}

export async function createSingleFileDirectoryTreeArtifact(input: {
  readonly fileId: string
  readonly sourcePath: string
  readonly outputName: string
}): Promise<DirectoryTreeArtifact> {
  const fileId = requireIdentity(input.fileId, STABLE_IDENTITY_BYTES, 'artifact file')
  const sourcePath = requireCanonicalPath(input.sourcePath)
  const outputName = requireResultName(input.outputName)
  if (sourcePath.split('/').at(-1) !== outputName) {
    throw new TypeError('single-file output name must equal the source-path leaf')
  }
  const layout = canonicalValue({
    kind: 'single-file' as const,
    fileId,
    sourcePath,
    outputName,
  }, concat([
    Uint8Array.of(1),
    frame(requireIdentityBytes(fileId, STABLE_IDENTITY_BYTES, 'artifact file')),
    frame(canonicalPathBytes(sourcePath)),
    frame(TEXT_ENCODER.encode(outputName)),
  ]))
  return createDirectoryTreeArtifact(layout)
}

export async function createResultRootDirectoryTreeArtifact(
  input: ResultRootLayout,
): Promise<DirectoryTreeArtifact> {
  const root = validateResultRootLayout(input)
  const layout = canonicalValue({
    kind: 'result-root' as const,
    root,
  }, concat([Uint8Array.of(2), frame(root.canonicalBytes)]))
  return createDirectoryTreeArtifact(layout)
}

export async function createCatalogRootDirectoryTreeArtifact(): Promise<DirectoryTreeArtifact> {
  return createDirectoryTreeArtifact(canonicalValue({ kind: 'catalog-root' as const }, Uint8Array.of(3)))
}

async function createDirectoryTreeArtifact(
  layout: DirectoryTreeLayout,
): Promise<DirectoryTreeArtifact> {
  const canonicalBytes = canonicalRecord(ARTIFACT_SPEC_DOMAIN, [
    Uint8Array.of(2),
    frame(layout.canonicalBytes),
  ])
  return canonicalDigestValue({
    version: ARTIFACT_SPEC_VERSION,
    kind: 'directory-tree' as const,
    layout,
  }, await digestText(canonicalBytes), canonicalBytes)
}

export async function createZipArchiveArtifact(
  input: ResultRootLayout,
): Promise<ZipArchiveArtifact> {
  const layout = validateResultRootLayout(input)
  const suggestedName = appendProtectedSuffix(layout.name, ARCHIVE_EXTENSION)
  const canonicalBytes = canonicalRecord(ARTIFACT_SPEC_DOMAIN, [
    Uint8Array.of(3),
    frame(layout.canonicalBytes),
    frame(TEXT_ENCODER.encode(suggestedName)),
    frame(Uint8Array.of(1)),
    frame(Uint8Array.of(1)),
  ])
  return canonicalDigestValue({
    version: ARTIFACT_SPEC_VERSION,
    kind: 'zip-archive' as const,
    layout,
    suggestedName,
    encoding: 'store' as const,
    completeness: 'complete-only' as const,
  }, await digestText(canonicalBytes), canonicalBytes)
}

export async function validateArtifactSpec(input: ArtifactSpec): Promise<ArtifactSpec> {
  if (input.version !== ARTIFACT_SPEC_VERSION) throw new TypeError('artifact spec version is invalid')
  let rebuilt: ArtifactSpec
  switch (input.kind) {
    case 'original-file':
      rebuilt = await createOriginalFileArtifact(input)
      break
    case 'directory-tree':
      switch (input.layout.kind) {
        case 'single-file':
          rebuilt = await createSingleFileDirectoryTreeArtifact(input.layout)
          break
        case 'result-root':
          rebuilt = await createResultRootDirectoryTreeArtifact(input.layout.root)
          break
        case 'catalog-root':
          rebuilt = await createCatalogRootDirectoryTreeArtifact()
          break
        default:
          throw new TypeError('directory-tree layout is invalid')
      }
      requireSameCanonicalValue(input.layout, rebuilt.layout, 'directory-tree layout')
      break
    case 'zip-archive':
      if (input.encoding !== 'store' || input.completeness !== 'complete-only') {
        throw new TypeError('ZIP artifact policy is invalid')
      }
      rebuilt = await createZipArchiveArtifact(input.layout)
      if (input.suggestedName !== rebuilt.suggestedName) {
        throw new TypeError('ZIP artifact name is not canonical')
      }
      break
    default:
      throw new TypeError('artifact kind is invalid')
  }
  return requireSameDigestRecord(input, rebuilt, 'artifact spec')
}

export function appendProtectedSuffix(baseInput: string, suffix: string): string {
  const base = requireResultName(baseInput)
  if (typeof suffix !== 'string' || suffix.length === 0) {
    throw new TypeError('protected result-name suffix is invalid')
  }
  const maximumBaseBytes = MAX_RESULT_COMPONENT_BYTES - TEXT_ENCODER.encode(suffix).byteLength
  if (maximumBaseBytes <= 0) throw new TypeError('protected result-name suffix is too long')
  const scalars = Array.from(base)
  while (TEXT_ENCODER.encode(scalars.join('')).byteLength > maximumBaseBytes) scalars.pop()
  if (scalars.length === 0) throw new TypeError('protected suffix consumed the complete result name')
  return requireResultName(scalars.join('') + suffix)
}

export function nativeTreeGuarantees(): GuaranteeSet {
  return guaranteeSet({
    profile: 'native-tree',
    nameAuthority: 'application-chosen',
    replacement: 'atomic-no-replace',
    delivery: 'managed-target',
    visibility: 'prefix-visible',
    rollback: 'none',
  })
}

export function fsaTreeGuarantees(): GuaranteeSet {
  return guaranteeSet({
    profile: 'fsa-tree',
    nameAuthority: 'application-chosen',
    replacement: 'coordinated-no-replace',
    delivery: 'managed-target',
    visibility: 'prefix-visible',
    rollback: 'none',
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
    visibility: 'atomic-commit',
    rollback: 'to-absent',
  })
}

export function browserHandoffGuarantees(): GuaranteeSet {
  return guaranteeSet({
    profile: 'browser-handoff',
    nameAuthority: 'browser-chosen',
    replacement: 'unknown',
    delivery: 'browser-handoff',
    visibility: 'unobservable',
    rollback: 'none',
  })
}

function guaranteeSet(value: GuaranteeSet): GuaranteeSet {
  return Object.freeze({ ...value })
}

export async function collisionName(
  operationIdInput: string,
  requestedNameInput: string,
  collisionIndex: number,
  fileLike: boolean,
): Promise<string> {
  const operationId = requireIdentity(operationIdInput, STABLE_IDENTITY_BYTES, 'operation')
  const requestedName = requireResultName(requestedNameInput)
  requireUint32(collisionIndex, 'collision index')
  if (typeof fileLike !== 'boolean') throw new TypeError('collision file-like decision must be boolean')
  if (collisionIndex === 0) return requestedName
  const material = canonicalRecord(NAME_COLLISION_DOMAIN, [
    frame(requireIdentityBytes(operationId, STABLE_IDENTITY_BYTES, 'operation')),
    uint32(collisionIndex),
    frame(TEXT_ENCODER.encode(requestedName)),
  ])
  const token = (await sha256(material)).slice(0, COLLISION_SUFFIX_HEX_CHARS / 2)
  const suffix = '-' + [...token].map((byte) => byte.toString(16).padStart(2, '0')).join('')
  let stem = requestedName
  let extension = ''
  if (fileLike) {
    const dot = requestedName.lastIndexOf('.')
    if (dot > 0) {
      stem = requestedName.slice(0, dot)
      extension = requestedName.slice(dot)
    }
  }
  const maximumStemBytes = MAX_RESULT_COMPONENT_BYTES -
    TEXT_ENCODER.encode(suffix).byteLength - TEXT_ENCODER.encode(extension).byteLength
  const scalars = Array.from(stem)
  while (TEXT_ENCODER.encode(scalars.join('')).byteLength > maximumStemBytes) scalars.pop()
  if (scalars.length === 0) throw new TypeError('collision suffix consumed the complete result name')
  return requireResultName(scalars.join('') + suffix + extension)
}

export async function createNativeContainerRootReservation(input: {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityRef: string
}): Promise<ContainerRootReservation> {
  const artifact = await validateArtifactSpec(input.artifact)
  if (artifact.kind !== 'directory-tree' || artifact.layout.kind !== 'catalog-root') {
    throw new TypeError('native container-root requires a catalog-root directory tree')
  }
  return createDestinationReservation({
    kind: 'container-root',
    operationId: input.operationId,
    reservationId: input.reservationId,
    artifact,
    authorityKind: 'native-container',
    authorityRef: input.authorityRef,
    guarantees: nativeTreeGuarantees(),
  }) as Promise<ContainerRootReservation>
}

export async function createNativeNamedEntryReservation(input: {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityRef: string
  readonly reservedName: string
  readonly collisionIndex: number
}): Promise<NamedContainerEntryReservation> {
  return createNamedEntryReservation({ ...input, authorityKind: 'native-container' })
}

export async function createFSANamedEntryReservation(input: {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityRef: string
  readonly reservedName: string
  readonly collisionIndex: number
}): Promise<NamedContainerEntryReservation> {
  return createNamedEntryReservation({ ...input, authorityKind: 'fsa-container' })
}

async function createNamedEntryReservation(input: {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityRef: string
  readonly authorityKind: 'native-container' | 'fsa-container'
  readonly reservedName: string
  readonly collisionIndex: number
}): Promise<NamedContainerEntryReservation> {
  const artifact = await validateArtifactSpec(input.artifact)
  if (artifact.kind !== 'directory-tree' || artifact.layout.kind === 'catalog-root') {
    throw new TypeError('named container entry requires a named directory-tree layout')
  }
  const entryKind = artifact.layout.kind === 'single-file' ? 'single-file' : 'result-root'
  const requestedName = artifact.layout.kind === 'single-file'
    ? artifact.layout.outputName
    : artifact.layout.root.name
  const reservedName = requireResultName(input.reservedName)
  const expected = await collisionName(
    input.operationId,
    requestedName,
    input.collisionIndex,
    entryKind === 'single-file',
  )
  if (reservedName !== expected) throw new TypeError('named-entry collision decision is invalid')
  return createDestinationReservation({
    kind: 'named-container-entry',
    operationId: input.operationId,
    reservationId: input.reservationId,
    artifact,
    authorityKind: input.authorityKind,
    authorityRef: input.authorityRef,
    guarantees: input.authorityKind === 'native-container'
      ? nativeTreeGuarantees()
      : fsaTreeGuarantees(),
    entryKind,
    requestedName,
    reservedName,
    collisionIndex: input.collisionIndex,
  }) as Promise<NamedContainerEntryReservation>
}

export async function createManagedAtomicReservation(input: {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityRef: string
  readonly nameAuthority: 'application-chosen' | 'user-chosen'
  readonly requestedName: string
  readonly reservedName: string
  readonly collisionIndex: number
}): Promise<AtomicTargetReservation> {
  const artifact = await validateArtifactSpec(input.artifact)
  const artifactName = completeArtifactName(artifact)
  const requestedName = requireResultName(input.requestedName)
  if (input.nameAuthority === 'application-chosen' && requestedName !== artifactName) {
    throw new TypeError('application-chosen atomic target must use the artifact name')
  }
  const reservedName = requireResultName(input.reservedName)
  const expected = await collisionName(
    input.operationId,
    requestedName,
    input.collisionIndex,
    true,
  )
  if (reservedName !== expected) throw new TypeError('atomic-target collision decision is invalid')
  return createDestinationReservation({
    kind: 'atomic-target',
    operationId: input.operationId,
    reservationId: input.reservationId,
    artifact,
    authorityKind: 'managed-atomic-target',
    authorityRef: input.authorityRef,
    guarantees: managedAtomicGuarantees(input.nameAuthority),
    requestedName,
    reservedName,
    collisionIndex: input.collisionIndex,
  }) as Promise<AtomicTargetReservation>
}

type DestinationReservationInput =
  | Readonly<{
      kind: 'container-root'
      operationId: string
      reservationId: string
      artifact: ArtifactSpec
      authorityKind: 'native-container'
      authorityRef: string
      guarantees: GuaranteeSet
    }>
  | Readonly<{
      kind: 'named-container-entry'
      operationId: string
      reservationId: string
      artifact: ArtifactSpec
      authorityKind: 'native-container' | 'fsa-container'
      authorityRef: string
      guarantees: GuaranteeSet
      entryKind: 'single-file' | 'result-root'
      requestedName: string
      reservedName: string
      collisionIndex: number
    }>
  | Readonly<{
      kind: 'atomic-target'
      operationId: string
      reservationId: string
      artifact: ArtifactSpec
      authorityKind: 'managed-atomic-target'
      authorityRef: string
      guarantees: GuaranteeSet
      requestedName: string
      reservedName: string
      collisionIndex: number
    }>

async function createDestinationReservation(
  input: DestinationReservationInput,
): Promise<DestinationReservation> {
  const operationId = requireIdentity(input.operationId, STABLE_IDENTITY_BYTES, 'operation')
  const reservationId = requireIdentity(
    input.reservationId,
    STABLE_IDENTITY_BYTES,
    'destination reservation',
  )
  const authorityRef = requireIdentity(
    input.authorityRef,
    AUTHORITY_REFERENCE_BYTES,
    'destination authority',
  )
  const guarantees = snapshotGuarantees(input.guarantees)
  const fields: Uint8Array[] = [
    Uint8Array.of(reservationKindByte(input.kind)),
    frame(requireIdentityBytes(operationId, STABLE_IDENTITY_BYTES, 'operation')),
    frame(requireIdentityBytes(reservationId, STABLE_IDENTITY_BYTES, 'destination reservation')),
    frame(requireIdentityBytes(input.artifact.digest, AUTHORITY_REFERENCE_BYTES, 'artifact digest')),
    frame(Uint8Array.of(authorityKindByte(input.authorityKind))),
    frame(requireIdentityBytes(authorityRef, AUTHORITY_REFERENCE_BYTES, 'destination authority')),
    frame(canonicalGuarantees(guarantees)),
  ]
  let variant: object
  switch (input.kind) {
    case 'container-root':
      variant = { kind: input.kind }
      break
    case 'named-container-entry':
      requireUint32(input.collisionIndex, 'collision index')
      fields.push(frame(Uint8Array.of(input.entryKind === 'single-file' ? 1 : 2)))
      fields.push(frame(TEXT_ENCODER.encode(input.requestedName)))
      fields.push(frame(TEXT_ENCODER.encode(input.reservedName)))
      fields.push(frame(uint32(input.collisionIndex)))
      variant = {
        kind: input.kind,
        entryKind: input.entryKind,
        requestedName: input.requestedName,
        reservedName: input.reservedName,
        collisionIndex: input.collisionIndex,
      }
      break
    case 'atomic-target':
      requireUint32(input.collisionIndex, 'collision index')
      fields.push(frame(TEXT_ENCODER.encode(input.requestedName)))
      fields.push(frame(TEXT_ENCODER.encode(input.reservedName)))
      fields.push(frame(uint32(input.collisionIndex)))
      variant = {
        kind: input.kind,
        requestedName: input.requestedName,
        reservedName: input.reservedName,
        collisionIndex: input.collisionIndex,
      }
      break
  }
  const canonicalBytes = canonicalRecord(DESTINATION_RESERVATION_DOMAIN, fields)
  return canonicalDigestValue({
    version: DESTINATION_RESERVATION_VERSION,
    ...variant,
    operationId,
    reservationId,
    artifactDigest: input.artifact.digest,
    authorityKind: input.authorityKind,
    authorityRef,
    guarantees,
  }, await digestText(canonicalBytes), canonicalBytes) as DestinationReservation
}

export async function validateDestinationReservation(
  input: DestinationReservation,
  artifactInput: ArtifactSpec,
): Promise<DestinationReservation> {
  if (input.version !== DESTINATION_RESERVATION_VERSION) {
    throw new TypeError('destination reservation version is invalid')
  }
  const artifact = await validateArtifactSpec(artifactInput)
  if (input.artifactDigest !== artifact.digest) {
    throw new TypeError('destination reservation artifact digest is invalid')
  }
  let rebuilt: DestinationReservation
  switch (input.kind) {
    case 'container-root':
      rebuilt = await createNativeContainerRootReservation({
        operationId: input.operationId,
        reservationId: input.reservationId,
        artifact,
        authorityRef: input.authorityRef,
      })
      break
    case 'named-container-entry': {
      const options = {
        operationId: input.operationId,
        reservationId: input.reservationId,
        artifact,
        authorityRef: input.authorityRef,
        reservedName: input.reservedName,
        collisionIndex: input.collisionIndex,
      }
      rebuilt = input.authorityKind === 'native-container'
        ? await createNativeNamedEntryReservation(options)
        : await createFSANamedEntryReservation(options)
      if (input.entryKind !== rebuilt.entryKind) {
        throw new TypeError('destination reservation entry kind is invalid')
      }
      break
    }
    case 'atomic-target':
      rebuilt = await createManagedAtomicReservation({
        operationId: input.operationId,
        reservationId: input.reservationId,
        artifact,
        authorityRef: input.authorityRef,
        nameAuthority: managedNameAuthority(input.guarantees.nameAuthority),
        requestedName: input.requestedName,
        reservedName: input.reservedName,
        collisionIndex: input.collisionIndex,
      })
      break
    default:
      throw new TypeError('destination reservation kind is invalid')
  }
  if (!sameGuarantees(input.guarantees, rebuilt.guarantees) ||
      input.authorityKind !== rebuilt.authorityKind) {
    throw new TypeError('destination reservation guarantee profile is invalid')
  }
  return requireSameDigestRecord(input, rebuilt, 'destination reservation')
}

export async function createWorkspaceBinding(input: {
  readonly operationId: string
  readonly workspaceId: string
  readonly artifact: ArtifactSpec
  readonly repositoryRef: string
}): Promise<WorkspaceBinding> {
  const artifact = await validateArtifactSpec(input.artifact)
  if (artifact.kind === 'directory-tree') {
    throw new TypeError('workspace binding requires a complete artifact')
  }
  const operationId = requireIdentity(input.operationId, STABLE_IDENTITY_BYTES, 'operation')
  const workspaceId = requireIdentity(input.workspaceId, STABLE_IDENTITY_BYTES, 'workspace')
  const repositoryRef = requireIdentity(
    input.repositoryRef,
    AUTHORITY_REFERENCE_BYTES,
    'workspace repository',
  )
  const canonicalBytes = canonicalRecord(WORKSPACE_BINDING_DOMAIN, [
    frame(requireIdentityBytes(operationId, STABLE_IDENTITY_BYTES, 'operation')),
    frame(requireIdentityBytes(workspaceId, STABLE_IDENTITY_BYTES, 'workspace')),
    frame(requireIdentityBytes(artifact.digest, AUTHORITY_REFERENCE_BYTES, 'artifact digest')),
    frame(requireIdentityBytes(repositoryRef, AUTHORITY_REFERENCE_BYTES, 'workspace repository')),
    frame(Uint8Array.of(1)),
    frame(Uint8Array.of(1)),
    frame(Uint8Array.of(1)),
  ])
  return canonicalDigestValue({
    version: WORKSPACE_BINDING_VERSION,
    operationId,
    workspaceId,
    artifactDigest: artifact.digest,
    repositoryRef,
    workspaceKind: 'origin-private' as const,
    budgetPolicy: 'workspace-v1' as const,
    retentionPolicy: 'stable-24h-v1' as const,
  }, await digestText(canonicalBytes), canonicalBytes)
}

export async function validateWorkspaceBinding(
  input: WorkspaceBinding,
  artifact: ArtifactSpec,
): Promise<WorkspaceBinding> {
  if (input.version !== WORKSPACE_BINDING_VERSION ||
      input.workspaceKind !== 'origin-private' ||
      input.budgetPolicy !== 'workspace-v1' ||
      input.retentionPolicy !== 'stable-24h-v1') {
    throw new TypeError('workspace binding policy is invalid')
  }
  const rebuilt = await createWorkspaceBinding({
    operationId: input.operationId,
    workspaceId: input.workspaceId,
    artifact,
    repositoryRef: input.repositoryRef,
  })
  if (input.artifactDigest !== rebuilt.artifactDigest) {
    throw new TypeError('workspace binding artifact digest is invalid')
  }
  return requireSameDigestRecord(input, rebuilt, 'workspace binding')
}

export async function createPortableBinding(input: {
  readonly operationId: string
  readonly portablePlanId: string
  readonly artifact: ArtifactSpec
}): Promise<PortableBinding> {
  const artifact = await validateArtifactSpec(input.artifact)
  if (artifact.kind === 'directory-tree') {
    throw new TypeError('portable binding requires a complete artifact')
  }
  const operationId = requireIdentity(input.operationId, STABLE_IDENTITY_BYTES, 'operation')
  const portablePlanId = requireIdentity(
    input.portablePlanId,
    STABLE_IDENTITY_BYTES,
    'portable plan',
  )
  const canonicalBytes = canonicalRecord(PORTABLE_BINDING_DOMAIN, [
    frame(requireIdentityBytes(operationId, STABLE_IDENTITY_BYTES, 'operation')),
    frame(requireIdentityBytes(portablePlanId, STABLE_IDENTITY_BYTES, 'portable plan')),
    frame(requireIdentityBytes(artifact.digest, AUTHORITY_REFERENCE_BYTES, 'artifact digest')),
    frame(uint64(DEFAULT_PORTABLE_ARTIFACT_LIMIT)),
    frame(uint64(DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES)),
    frame(uint64(DEFAULT_PORTABLE_MAXIMUM_PARTS)),
    frame(uint64(BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS)),
    frame(Uint8Array.of(2)),
  ])
  return canonicalDigestValue({
    version: PORTABLE_BINDING_VERSION,
    operationId,
    portablePlanId,
    artifactDigest: artifact.digest,
    maximumArtifactBytes: DEFAULT_PORTABLE_ARTIFACT_LIMIT,
    assemblyPartBytes: DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
    maximumParts: DEFAULT_PORTABLE_MAXIMUM_PARTS,
    objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
    preparation: 'exact-artifact' as const,
  }, await digestText(canonicalBytes), canonicalBytes)
}

export async function validatePortableBinding(
  input: PortableBinding,
  artifact: ArtifactSpec,
): Promise<PortableBinding> {
  if (input.version !== PORTABLE_BINDING_VERSION ||
      input.maximumArtifactBytes !== DEFAULT_PORTABLE_ARTIFACT_LIMIT ||
      input.assemblyPartBytes !== DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES ||
      input.maximumParts !== DEFAULT_PORTABLE_MAXIMUM_PARTS ||
      input.objectUrlLeaseMilliseconds !== BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS ||
      input.preparation !== 'exact-artifact') {
    throw new TypeError('portable binding policy is invalid')
  }
  const rebuilt = await createPortableBinding({
    operationId: input.operationId,
    portablePlanId: input.portablePlanId,
    artifact,
  })
  if (input.artifactDigest !== rebuilt.artifactDigest) {
    throw new TypeError('portable binding artifact digest is invalid')
  }
  return requireSameDigestRecord(input, rebuilt, 'portable binding')
}

export async function createDirectTreePlan(
  artifactInput: ArtifactSpec,
  reservationInput: DestinationReservation,
): Promise<DirectTreePlan> {
  const artifact = await validateArtifactSpec(artifactInput)
  const reservation = await validateDestinationReservation(reservationInput, artifact)
  if (artifact.kind !== 'directory-tree') throw new TypeError('direct-tree requires a directory-tree artifact')
  switch (artifact.layout.kind) {
    case 'catalog-root':
      if (reservation.kind !== 'container-root') {
        throw new TypeError('catalog-root direct tree requires a container-root reservation')
      }
      break
    case 'single-file':
      if (reservation.kind !== 'named-container-entry' || reservation.entryKind !== 'single-file') {
        throw new TypeError('single-file direct tree requires a matching named reservation')
      }
      break
    case 'result-root':
      if (reservation.kind !== 'named-container-entry' || reservation.entryKind !== 'result-root') {
        throw new TypeError('result-root direct tree requires a matching named reservation')
      }
      break
  }
  const canonicalBytes = canonicalRecord(MATERIALIZATION_PLAN_DOMAIN, [
    Uint8Array.of(1),
    frame(reservation.canonicalBytes),
    frame(Uint8Array.of(0)),
  ])
  return canonicalValue({
    version: MATERIALIZATION_PLAN_VERSION,
    kind: 'direct-tree' as const,
    reservation,
    preparation: 'none' as const,
  }, canonicalBytes)
}

export async function createDirectAtomicPlan(
  artifactInput: ArtifactSpec,
  reservationInput: DestinationReservation,
): Promise<DirectAtomicPlan> {
  const artifact = await validateArtifactSpec(artifactInput)
  const reservation = await validateDestinationReservation(reservationInput, artifact)
  if (artifact.kind === 'directory-tree' || reservation.kind !== 'atomic-target') {
    throw new TypeError('direct-atomic requires a complete artifact and atomic reservation')
  }
  const canonicalBytes = canonicalRecord(MATERIALIZATION_PLAN_DOMAIN, [
    Uint8Array.of(2),
    frame(reservation.canonicalBytes),
    frame(Uint8Array.of(0)),
  ])
  return canonicalValue({
    version: MATERIALIZATION_PLAN_VERSION,
    kind: 'direct-atomic' as const,
    reservation,
    preparation: 'none' as const,
  }, canonicalBytes)
}

export async function createWorkspaceThenPublishPlan(
  artifactInput: ArtifactSpec,
  workspaceInput: WorkspaceBinding,
): Promise<WorkspaceThenPublishPlan> {
  const artifact = await validateArtifactSpec(artifactInput)
  const workspace = await validateWorkspaceBinding(workspaceInput, artifact)
  const preparation = artifact.kind === 'zip-archive' ? 'exact-zip' : 'none'
  const canonicalBytes = canonicalRecord(MATERIALIZATION_PLAN_DOMAIN, [
    Uint8Array.of(3),
    frame(workspace.canonicalBytes),
    frame(Uint8Array.of(preparation === 'exact-zip' ? 1 : 0)),
  ])
  return canonicalValue({
    version: MATERIALIZATION_PLAN_VERSION,
    kind: 'workspace-then-publish' as const,
    workspace,
    preparation,
  }, canonicalBytes)
}

export async function createPortableHandoffPlan(
  artifactInput: ArtifactSpec,
  portableInput: PortableBinding,
): Promise<PortableHandoffPlan> {
  const artifact = await validateArtifactSpec(artifactInput)
  const portable = await validatePortableBinding(portableInput, artifact)
  const canonicalBytes = canonicalRecord(MATERIALIZATION_PLAN_DOMAIN, [
    Uint8Array.of(4),
    frame(portable.canonicalBytes),
    frame(Uint8Array.of(2)),
    frame(Uint8Array.of(2)),
  ])
  return canonicalValue({
    version: MATERIALIZATION_PLAN_VERSION,
    kind: 'portable-handoff' as const,
    portable,
    publicationRoute: 'browser-handoff' as const,
    preparation: 'exact-artifact' as const,
  }, canonicalBytes)
}

export async function validateMaterializationPlan(
  input: MaterializationPlan,
  artifact: ArtifactSpec,
): Promise<MaterializationPlan> {
  if (input.version !== MATERIALIZATION_PLAN_VERSION) {
    throw new TypeError('materialization plan version is invalid')
  }
  let rebuilt: MaterializationPlan
  switch (input.kind) {
    case 'direct-tree':
      if (input.preparation !== 'none') throw new TypeError('direct-tree preparation is invalid')
      rebuilt = await createDirectTreePlan(artifact, input.reservation)
      break
    case 'direct-atomic':
      if (input.preparation !== 'none') throw new TypeError('direct-atomic preparation is invalid')
      rebuilt = await createDirectAtomicPlan(artifact, input.reservation)
      break
    case 'workspace-then-publish':
      rebuilt = await createWorkspaceThenPublishPlan(artifact, input.workspace)
      if (input.preparation !== rebuilt.preparation) {
        throw new TypeError('workspace preparation policy is invalid')
      }
      break
    case 'portable-handoff':
      if (input.publicationRoute !== 'browser-handoff' || input.preparation !== 'exact-artifact') {
        throw new TypeError('portable handoff policy is invalid')
      }
      rebuilt = await createPortableHandoffPlan(artifact, input.portable)
      break
    default:
      throw new TypeError('materialization plan kind is invalid')
  }
  return requireSameCanonicalValue(input, rebuilt, 'materialization plan')
}

export function materializationPlanOperationID(plan: MaterializationPlan): string {
  switch (plan.kind) {
    case 'direct-tree':
    case 'direct-atomic':
      return plan.reservation.operationId
    case 'workspace-then-publish':
      return plan.workspace.operationId
    case 'portable-handoff':
      return plan.portable.operationId
  }
}

export function materializationPlanArtifactDigest(plan: MaterializationPlan): string {
  switch (plan.kind) {
    case 'direct-tree':
    case 'direct-atomic':
      return plan.reservation.artifactDigest
    case 'workspace-then-publish':
      return plan.workspace.artifactDigest
    case 'portable-handoff':
      return plan.portable.artifactDigest
  }
}

export function materializationPlanBindingDigest(plan: MaterializationPlan): string {
  switch (plan.kind) {
    case 'direct-tree':
    case 'direct-atomic':
      return plan.reservation.digest
    case 'workspace-then-publish':
      return plan.workspace.digest
    case 'portable-handoff':
      return plan.portable.digest
  }
}

export async function createReceiveIntent(input: {
  readonly selection: SelectionSpec
  readonly artifact: ArtifactSpec
  readonly plan: MaterializationPlan
}): Promise<ReceiveIntent> {
  const selection = await validateSelectionSpec(input.selection)
  const artifact = await validateArtifactSpec(input.artifact)
  const plan = await validateMaterializationPlan(input.plan, artifact)
  if (materializationPlanArtifactDigest(plan) !== artifact.digest) {
    throw new TypeError('materialization plan does not bind the receive artifact')
  }
  const operationId = materializationPlanOperationID(plan)
  const bindingDigest = materializationPlanBindingDigest(plan)
  const canonicalBytes = canonicalReceiveIntentBytes({ selection, artifact, plan })
  return canonicalDigestValue({
    version: RECEIVE_INTENT_VERSION,
    selection,
    artifact,
    plan,
    shareInstance: selection.shareInstance,
    syntheticRoot: selection.syntheticRoot,
    operationId,
    bindingDigest,
  }, await digestText(canonicalBytes), canonicalBytes)
}

export function canonicalReceiveIntentBytes(input: {
  readonly selection: SelectionSpec
  readonly artifact: ArtifactSpec
  readonly plan: MaterializationPlan
}): CanonicalBytes {
  requireCanonicalValueProvenance(input.selection, 'selection spec')
  requireCanonicalValueProvenance(input.artifact, 'artifact spec')
  requireCanonicalValueProvenance(input.plan, 'materialization plan')
  if (materializationPlanArtifactDigest(input.plan) !== input.artifact.digest) {
    throw new TypeError('materialization plan artifact binding is invalid')
  }
  return canonicalRecord(RECEIVE_INTENT_DOMAIN, [
    frame(input.selection.canonicalBytes),
    frame(input.artifact.canonicalBytes),
    frame(input.plan.canonicalBytes),
  ])
}

// Persistence must rebuild authority through the same constructors as a new
// operation; accepting a merely parseable image could silently normalize a
// different selection, binding, or guarantee profile on reopen.
export async function decodeReceiveIntent(canonicalBytes: Uint8Array): Promise<ReceiveIntent> {
  if (!(canonicalBytes instanceof Uint8Array)) {
    throw new TypeError(INVALID_RECEIVE_INTENT_CANONICAL_BYTES)
  }
  const encoded = Uint8Array.from(canonicalBytes)
  try {
    const cursor = CanonicalDecoder.record(encoded, RECEIVE_INTENT_DOMAIN)
    const selectionBytes = cursor.readFrame(cursor.remaining)
    const artifactBytes = cursor.readFrame(cursor.remaining)
    const planBytes = cursor.readFrame(cursor.remaining)
    cursor.requireDone()

    const selection = await decodeSelectionSpecBytes(selectionBytes)
    const artifact = await decodeArtifactSpecBytes(artifactBytes)
    const plan = await decodeMaterializationPlanBytes(planBytes, artifact)
    const intent = await createReceiveIntent({ selection, artifact, plan })
    requireDecodedCanonicalBytes(encoded, intent.canonicalBytes, 'receive intent')
    return intent
  } catch {
    throw new TypeError(INVALID_RECEIVE_INTENT_CANONICAL_BYTES)
  }
}

export async function validateReceiveIntent(input: ReceiveIntent): Promise<ReceiveIntent> {
  if (input.version !== RECEIVE_INTENT_VERSION) throw new TypeError('receive intent version is invalid')
  const rebuilt = await createReceiveIntent(input)
  if (input.shareInstance !== rebuilt.shareInstance ||
      input.syntheticRoot !== rebuilt.syntheticRoot ||
      input.operationId !== rebuilt.operationId ||
      input.bindingDigest !== rebuilt.bindingDigest) {
    throw new TypeError('receive intent derived authority fields are invalid')
  }
  return requireSameDigestRecord(input, rebuilt, 'receive intent')
}

export async function receiveIntentDigest(input: ReceiveIntent): Promise<string> {
  return (await validateReceiveIntent(input)).digest
}

async function decodeSelectionSpecBytes(encoded: CanonicalBytes): Promise<SelectionSpec> {
  const cursor = CanonicalDecoder.record(encoded, SELECTION_SPEC_DOMAIN)
  const shareInstance = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const syntheticRoot = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const mode = cursor.readFramedByte()
  const defaultSelected = cursor.readFramedBoolean()
  const count = cursor.readRawUint64()
  const rules = decodeSelectionRulesBytes(cursor, mode, defaultSelected, count)
  const selection = await createSelectionSpec({ shareInstance, syntheticRoot, rules })
  cursor.requireDone()
  requireDecodedCanonicalBytes(encoded, selection.canonicalBytes, 'selection spec')
  return selection
}

function decodeSelectionRulesBytes(
  cursor: CanonicalDecoder,
  mode: number,
  defaultSelected: boolean,
  count: bigint,
): SelectionRulesSpec {
  if (mode === 1) return decodeNodeSelectionRulesBytes(cursor, defaultSelected, count)
  if (mode === 2) return decodePathSelectionRulesBytes(cursor, defaultSelected, count)
  return invalidDecodedCanonicalBytes()
}

function decodeNodeSelectionRulesBytes(
  cursor: CanonicalDecoder,
  defaultSelected: boolean,
  count: bigint,
): NodeIDSelectionRules {
  const ruleCount = decodedCount(count, 0, MAX_SELECTION_RULES)
  const rules: NodeSelectionRule[] = []
  for (let index = 0; index < ruleCount; index += 1) {
    const kind = cursor.readFramedByte()
    if (kind !== 1 && kind !== 2) invalidDecodedCanonicalBytes()
    rules.push({
      kind: kind === 1 ? 'directory' : 'file',
      id: encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES)),
      selected: cursor.readFramedBoolean(),
    })
  }
  return { mode: 'node-id', defaultSelected, rules }
}

function decodePathSelectionRulesBytes(
  cursor: CanonicalDecoder,
  defaultSelected: boolean,
  count: bigint,
): CatalogPathSelectionRules {
  if (defaultSelected) invalidDecodedCanonicalBytes()
  const pathCount = decodedCount(count, 1, MAX_SELECTION_RULES)
  const paths: string[] = []
  let totalBytes = 0
  for (let index = 0; index < pathCount; index += 1) {
    const pathBytes = cursor.readFrame(V2_CATALOG_PATH_BYTES)
    totalBytes += pathBytes.byteLength
    if (totalBytes > MAX_SELECTION_TARGET_UTF8_BYTES) invalidDecodedCanonicalBytes()
    paths.push(decodeCanonicalText(pathBytes))
  }
  return { mode: 'catalog-path', defaultSelected: false, paths }
}

async function decodeArtifactSpecBytes(encoded: CanonicalBytes): Promise<ArtifactSpec> {
  const cursor = CanonicalDecoder.record(encoded, ARTIFACT_SPEC_DOMAIN)
  const kind = cursor.readRawByte()
  let artifact: ArtifactSpec
  switch (kind) {
    case 1: {
      const fileId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
      const sourcePath = decodeCanonicalPath(cursor.readFrame(MAX_CANONICAL_PATH_ENCODING_BYTES))
      const suggestedName = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
      artifact = await createOriginalFileArtifact({ fileId, sourcePath, suggestedName })
      break
    }
    case 2:
      artifact = await decodeDirectoryTreeArtifactBytes(cursor.readFrame(cursor.remaining))
      break
    case 3: {
      const layout = decodeResultRootLayoutBytes(cursor.readFrame(cursor.remaining))
      const suggestedName = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
      const encoding = cursor.readFramedByte()
      const completeness = cursor.readFramedByte()
      if (encoding !== 1 || completeness !== 1) invalidDecodedCanonicalBytes()
      artifact = await createZipArchiveArtifact(layout)
      if (artifact.suggestedName !== suggestedName) invalidDecodedCanonicalBytes()
      break
    }
    default:
      return invalidDecodedCanonicalBytes()
  }
  cursor.requireDone()
  requireDecodedCanonicalBytes(encoded, artifact.canonicalBytes, 'artifact spec')
  return artifact
}

async function decodeDirectoryTreeArtifactBytes(encoded: CanonicalBytes): Promise<ArtifactSpec> {
  const cursor = new CanonicalDecoder(encoded)
  const kind = cursor.readRawByte()
  let artifact: ArtifactSpec
  switch (kind) {
    case 1:
      artifact = await createSingleFileDirectoryTreeArtifact({
        fileId: encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES)),
        sourcePath: decodeCanonicalPath(cursor.readFrame(MAX_CANONICAL_PATH_ENCODING_BYTES)),
        outputName: decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES)),
      })
      break
    case 2:
      artifact = await createResultRootDirectoryTreeArtifact(
        decodeResultRootLayoutBytes(cursor.readFrame(cursor.remaining)),
      )
      break
    case 3:
      artifact = await createCatalogRootDirectoryTreeArtifact()
      break
    default:
      return invalidDecodedCanonicalBytes()
  }
  cursor.requireDone()
  return artifact
}

function decodeResultRootLayoutBytes(encoded: CanonicalBytes): ResultRootLayout {
  const cursor = CanonicalDecoder.record(encoded, RESULT_ROOT_LAYOUT_DOMAIN)
  const rootClass = cursor.readFramedByte()
  const anchorBytes = cursor.readFrame(cursor.remaining)
  const name = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
  cursor.requireDone()

  const anchor = new CanonicalDecoder(anchorBytes)
  const anchorKind = anchor.readRawByte()
  let layout: ResultRootLayout
  switch (anchorKind) {
    case 1: {
      const directoryId = encodeBase64Url(anchor.readFixedFrame(STABLE_IDENTITY_BYTES))
      const sourcePath = decodeCanonicalPath(anchor.readFrame(MAX_CANONICAL_PATH_ENCODING_BYTES))
      anchor.requireDone()
      if (rootClass === 1) {
        layout = createCompleteDirectoryResultRoot(directoryId, sourcePath)
      } else if (rootClass === 2) {
        layout = createDirectorySelectionResultRoot(directoryId, sourcePath)
      } else {
        return invalidDecodedCanonicalBytes()
      }
      break
    }
    case 2:
      if (rootClass !== 3) return invalidDecodedCanonicalBytes()
      anchor.requireDone()
      layout = createSyntheticSelectionResultRoot()
      break
    default:
      return invalidDecodedCanonicalBytes()
  }
  if (layout.name !== name) invalidDecodedCanonicalBytes()
  requireDecodedCanonicalBytes(encoded, layout.canonicalBytes, 'result-root layout')
  return layout
}

async function decodeDestinationReservationBytes(
  encoded: CanonicalBytes,
  artifact: ArtifactSpec,
): Promise<DestinationReservation> {
  const cursor = CanonicalDecoder.record(encoded, DESTINATION_RESERVATION_DOMAIN)
  const kind = cursor.readRawByte()
  const operationId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const reservationId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const artifactDigest = encodeBase64Url(cursor.readFixedFrame(AUTHORITY_REFERENCE_BYTES))
  const authorityKind = cursor.readFramedByte()
  const authorityRef = encodeBase64Url(cursor.readFixedFrame(AUTHORITY_REFERENCE_BYTES))
  const guarantees = decodeGuaranteeSetBytes(cursor.readFrame(cursor.remaining))
  if (artifactDigest !== artifact.digest) invalidDecodedCanonicalBytes()
  const common: DecodedDestinationReservationCommon = {
    operationId,
    reservationId,
    artifact,
    authorityKind,
    authorityRef,
    guarantees,
  }
  let reservation: DestinationReservation
  switch (kind) {
    case 1:
      reservation = await decodeContainerRootReservationBytes(common)
      break
    case 2:
      reservation = await decodeNamedContainerEntryReservationBytes(cursor, common)
      break
    case 3:
      reservation = await decodeAtomicTargetReservationBytes(cursor, common)
      break
    default:
      return invalidDecodedCanonicalBytes()
  }
  cursor.requireDone()
  requireDecodedCanonicalBytes(encoded, reservation.canonicalBytes, 'destination reservation')
  return reservation
}

interface DecodedDestinationReservationCommon {
  readonly operationId: string
  readonly reservationId: string
  readonly artifact: ArtifactSpec
  readonly authorityKind: number
  readonly authorityRef: string
  readonly guarantees: GuaranteeSet
}

async function decodeContainerRootReservationBytes(
  common: DecodedDestinationReservationCommon,
): Promise<ContainerRootReservation> {
  if (common.authorityKind !== 1 ||
      !sameGuarantees(common.guarantees, nativeTreeGuarantees())) {
    return invalidDecodedCanonicalBytes()
  }
  return createNativeContainerRootReservation(common)
}

async function decodeNamedContainerEntryReservationBytes(
  cursor: CanonicalDecoder,
  common: DecodedDestinationReservationCommon,
): Promise<NamedContainerEntryReservation> {
  const entryKind = cursor.readFramedByte()
  if (entryKind !== 1 && entryKind !== 2) invalidDecodedCanonicalBytes()
  const requestedName = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
  const reservedName = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
  const collisionIndex = cursor.readFramedUint32()
  const options = { ...common, reservedName, collisionIndex }
  let reservation: NamedContainerEntryReservation
  if (common.authorityKind === 1 &&
      sameGuarantees(common.guarantees, nativeTreeGuarantees())) {
    reservation = await createNativeNamedEntryReservation(options)
  } else if (common.authorityKind === 2 &&
             sameGuarantees(common.guarantees, fsaTreeGuarantees())) {
    reservation = await createFSANamedEntryReservation(options)
  } else {
    return invalidDecodedCanonicalBytes()
  }
  const expectedEntryKind = entryKind === 1 ? 'single-file' : 'result-root'
  if (reservation.entryKind !== expectedEntryKind || reservation.requestedName !== requestedName) {
    return invalidDecodedCanonicalBytes()
  }
  return reservation
}

async function decodeAtomicTargetReservationBytes(
  cursor: CanonicalDecoder,
  common: DecodedDestinationReservationCommon,
): Promise<AtomicTargetReservation> {
  const requestedName = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
  const reservedName = decodeCanonicalText(cursor.readFrame(MAX_RESULT_COMPONENT_BYTES))
  const collisionIndex = cursor.readFramedUint32()
  if (common.authorityKind !== 3 || common.guarantees.profile !== 'managed-atomic') {
    return invalidDecodedCanonicalBytes()
  }
  return createManagedAtomicReservation({
    ...common,
    nameAuthority: managedNameAuthority(common.guarantees.nameAuthority),
    requestedName,
    reservedName,
    collisionIndex,
  })
}

async function decodeWorkspaceBindingBytes(
  encoded: CanonicalBytes,
  artifact: ArtifactSpec,
): Promise<WorkspaceBinding> {
  const cursor = CanonicalDecoder.record(encoded, WORKSPACE_BINDING_DOMAIN)
  const operationId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const workspaceId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const artifactDigest = encodeBase64Url(cursor.readFixedFrame(AUTHORITY_REFERENCE_BYTES))
  const repositoryRef = encodeBase64Url(cursor.readFixedFrame(AUTHORITY_REFERENCE_BYTES))
  const workspaceKind = cursor.readFramedByte()
  const budgetPolicy = cursor.readFramedByte()
  const retentionPolicy = cursor.readFramedByte()
  cursor.requireDone()
  if (artifactDigest !== artifact.digest || workspaceKind !== 1 ||
      budgetPolicy !== 1 || retentionPolicy !== 1) {
    return invalidDecodedCanonicalBytes()
  }
  const binding = await createWorkspaceBinding({
    operationId,
    workspaceId,
    artifact,
    repositoryRef,
  })
  requireDecodedCanonicalBytes(encoded, binding.canonicalBytes, 'workspace binding')
  return binding
}

async function decodePortableBindingBytes(
  encoded: CanonicalBytes,
  artifact: ArtifactSpec,
): Promise<PortableBinding> {
  const cursor = CanonicalDecoder.record(encoded, PORTABLE_BINDING_DOMAIN)
  const operationId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const portablePlanId = encodeBase64Url(cursor.readFixedFrame(STABLE_IDENTITY_BYTES))
  const artifactDigest = encodeBase64Url(cursor.readFixedFrame(AUTHORITY_REFERENCE_BYTES))
  const maximumArtifactBytes = cursor.readFramedUint64()
  const assemblyPartBytes = cursor.readFramedUint64()
  const maximumParts = cursor.readFramedUint64()
  const objectUrlLeaseMilliseconds = cursor.readFramedUint64()
  const preparation = cursor.readFramedByte()
  cursor.requireDone()
  if (artifactDigest !== artifact.digest ||
      maximumArtifactBytes !== DEFAULT_PORTABLE_ARTIFACT_LIMIT ||
      assemblyPartBytes !== DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES ||
      maximumParts !== DEFAULT_PORTABLE_MAXIMUM_PARTS ||
      objectUrlLeaseMilliseconds !== BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS ||
      preparation !== 2) {
    return invalidDecodedCanonicalBytes()
  }
  const binding = await createPortableBinding({ operationId, portablePlanId, artifact })
  requireDecodedCanonicalBytes(encoded, binding.canonicalBytes, 'portable binding')
  return binding
}

async function decodeMaterializationPlanBytes(
  encoded: CanonicalBytes,
  artifact: ArtifactSpec,
): Promise<MaterializationPlan> {
  const cursor = CanonicalDecoder.record(encoded, MATERIALIZATION_PLAN_DOMAIN)
  const kind = cursor.readRawByte()
  let plan: MaterializationPlan
  switch (kind) {
    case 1: {
      const reservation = await decodeDestinationReservationBytes(
        cursor.readFrame(cursor.remaining),
        artifact,
      )
      if (cursor.readFramedByte() !== 0) invalidDecodedCanonicalBytes()
      plan = await createDirectTreePlan(artifact, reservation)
      break
    }
    case 2: {
      const reservation = await decodeDestinationReservationBytes(
        cursor.readFrame(cursor.remaining),
        artifact,
      )
      if (cursor.readFramedByte() !== 0) invalidDecodedCanonicalBytes()
      plan = await createDirectAtomicPlan(artifact, reservation)
      break
    }
    case 3: {
      const workspace = await decodeWorkspaceBindingBytes(
        cursor.readFrame(cursor.remaining),
        artifact,
      )
      const preparation = cursor.readFramedByte()
      plan = await createWorkspaceThenPublishPlan(artifact, workspace)
      const expectedPreparation = plan.preparation === 'exact-zip' ? 1 : 0
      if (preparation !== expectedPreparation) invalidDecodedCanonicalBytes()
      break
    }
    case 4: {
      const portable = await decodePortableBindingBytes(
        cursor.readFrame(cursor.remaining),
        artifact,
      )
      const publicationRoute = cursor.readFramedByte()
      const preparation = cursor.readFramedByte()
      if (publicationRoute !== 2 || preparation !== 2) invalidDecodedCanonicalBytes()
      plan = await createPortableHandoffPlan(artifact, portable)
      break
    }
    default:
      return invalidDecodedCanonicalBytes()
  }
  cursor.requireDone()
  requireDecodedCanonicalBytes(encoded, plan.canonicalBytes, 'materialization plan')
  return plan
}

function decodeGuaranteeSetBytes(encoded: CanonicalBytes): GuaranteeSet {
  const candidates = [
    nativeTreeGuarantees(),
    fsaTreeGuarantees(),
    managedAtomicGuarantees('application-chosen'),
    managedAtomicGuarantees('user-chosen'),
    browserHandoffGuarantees(),
  ]
  const guarantee = candidates.find((candidate) =>
    equalBytes(encoded, canonicalGuarantees(candidate)))
  return guarantee ?? invalidDecodedCanonicalBytes()
}

function decodeCanonicalPath(encoded: CanonicalBytes): string {
  const cursor = new CanonicalDecoder(encoded)
  const segmentCount = decodedCount(cursor.readRawUint64(), 1, V2_CATALOG_PATH_DEPTH)
  const segments: string[] = []
  let pathBytes = 0
  for (let index = 0; index < segmentCount; index += 1) {
    const segmentBytes = cursor.readFrame(V2_CATALOG_NAME_BYTES)
    pathBytes += segmentBytes.byteLength + (index === 0 ? 0 : 1)
    if (pathBytes > V2_CATALOG_PATH_BYTES) invalidDecodedCanonicalBytes()
    segments.push(decodeCanonicalText(segmentBytes))
  }
  cursor.requireDone()
  const path = requireCanonicalPath(segments.join('/'))
  requireDecodedCanonicalBytes(encoded, canonicalPathBytes(path), 'canonical path')
  return path
}

function decodeCanonicalText(encoded: CanonicalBytes): string {
  return TEXT_DECODER.decode(encoded)
}

function decodedCount(value: bigint, minimum: number, maximum: number): number {
  if (!Number.isSafeInteger(minimum) || !Number.isSafeInteger(maximum) ||
      minimum < 0 || maximum < minimum ||
      value < BigInt(minimum) || value > BigInt(maximum)) {
    return invalidDecodedCanonicalBytes()
  }
  return Number(value)
}

function requireDecodedCanonicalBytes(
  encoded: Uint8Array,
  rebuilt: Uint8Array,
  label: string,
): void {
  if (!equalBytes(encoded, rebuilt)) {
    throw new TypeError(label + ' bytes are not canonical')
  }
}

function invalidDecodedCanonicalBytes(): never {
  throw new TypeError(INVALID_RECEIVE_INTENT_CANONICAL_BYTES)
}

// Lengths are checked as bigint before conversion so hostile persisted frames
// cannot wrap JavaScript numbers or make a truncated image look complete.
class CanonicalDecoder {
  private offset = 0
  private readonly encoded: CanonicalBytes

  public constructor(encoded: CanonicalBytes) {
    this.encoded = encoded
  }

  public static record(encoded: CanonicalBytes, domain: string): CanonicalDecoder {
    const cursor = new CanonicalDecoder(encoded)
    for (const expected of TEXT_ENCODER.encode(domain)) {
      if (cursor.readRawByte() !== expected) invalidDecodedCanonicalBytes()
    }
    if (cursor.readRawByte() !== 0 || cursor.readRawByte() !== CANONICAL_SCHEMA_VERSION) {
      return invalidDecodedCanonicalBytes()
    }
    return cursor
  }

  public get remaining(): number {
    return this.encoded.byteLength - this.offset
  }

  public readRawByte(): number {
    if (this.remaining < 1) return invalidDecodedCanonicalBytes()
    return this.encoded[this.offset++]!
  }

  public readRawUint64(): bigint {
    if (this.remaining < 8) return invalidDecodedCanonicalBytes()
    const value = new DataView(
      this.encoded.buffer,
      this.encoded.byteOffset + this.offset,
      8,
    ).getBigUint64(0)
    this.offset += 8
    return value
  }

  public readFrame(maximum: number): CanonicalBytes {
    if (!Number.isSafeInteger(maximum) || maximum < 0) return invalidDecodedCanonicalBytes()
    const length = this.readRawUint64()
    if (length > BigInt(maximum) || length > BigInt(this.remaining)) {
      return invalidDecodedCanonicalBytes()
    }
    const size = Number(length)
    const value = this.encoded.slice(this.offset, this.offset + size)
    this.offset += size
    return value
  }

  public readFixedFrame(width: number): CanonicalBytes {
    const value = this.readFrame(width)
    if (value.byteLength !== width) return invalidDecodedCanonicalBytes()
    return value
  }

  public readFramedByte(): number {
    return this.readFixedFrame(1)[0]!
  }

  public readFramedBoolean(): boolean {
    const value = this.readFramedByte()
    if (value !== 0 && value !== 1) return invalidDecodedCanonicalBytes()
    return value === 1
  }

  public readFramedUint32(): number {
    const value = this.readFixedFrame(4)
    return new DataView(value.buffer, value.byteOffset, value.byteLength).getUint32(0)
  }

  public readFramedUint64(): bigint {
    const value = this.readFixedFrame(8)
    return new DataView(value.buffer, value.byteOffset, value.byteLength).getBigUint64(0)
  }

  public requireDone(): void {
    if (this.remaining !== 0) invalidDecodedCanonicalBytes()
  }
}

function snapshotSelectionRules(input: SelectionRulesSpec): SelectionRulesSpec {
  if (input.mode !== 'node-id' && input.mode !== 'catalog-path') {
    throw new TypeError('selection mode is invalid')
  }
  if (typeof input.defaultSelected !== 'boolean') {
    throw new TypeError('selection default must be boolean')
  }
  if (input.mode === 'catalog-path') {
    if (input.defaultSelected !== false || !Array.isArray(input.paths) ||
        input.paths.length === 0 || input.paths.length > MAX_SELECTION_RULES) {
      throw new TypeError('catalog-path selection is invalid')
    }
    const seen = new Set<string>()
    let totalBytes = 0
    const paths = input.paths.map((path) => {
      if (typeof path !== 'string') throw new TypeError('selection path must be text')
      const canonical = requireCanonicalPath(path)
      if (seen.has(canonical)) throw new TypeError('catalog-path selection contains a duplicate')
      seen.add(canonical)
      totalBytes += TEXT_ENCODER.encode(canonical).byteLength
      if (totalBytes > MAX_SELECTION_TARGET_UTF8_BYTES) {
        throw new RangeError('catalog-path selection exceeds its UTF-8 byte limit')
      }
      return canonical
    })
    paths.sort(compareTextBytes)
    return Object.freeze({
      mode: 'catalog-path' as const,
      defaultSelected: false as const,
      paths: Object.freeze(paths),
    })
  }
  if (!Array.isArray(input.rules) || input.rules.length > MAX_SELECTION_RULES) {
    throw new TypeError('node-id selection is invalid')
  }
  const seen = new Set<string>()
  const rules = input.rules.map((rule) => {
    if (rule.kind !== 'directory' && rule.kind !== 'file') {
      throw new TypeError('selection rule kind is invalid')
    }
    if (typeof rule.selected !== 'boolean') throw new TypeError('selection rule decision must be boolean')
    const id = requireIdentity(rule.id, STABLE_IDENTITY_BYTES, 'selection rule identity')
    if (seen.has(id)) throw new TypeError('node-id selection contains a duplicate rule')
    seen.add(id)
    return Object.freeze({ kind: rule.kind, id, selected: rule.selected })
  })
  rules.sort((left, right) => {
    const comparison = compareBytes(
      requireIdentityBytes(left.id, STABLE_IDENTITY_BYTES, 'selection rule identity'),
      requireIdentityBytes(right.id, STABLE_IDENTITY_BYTES, 'selection rule identity'),
    )
    if (comparison !== 0) return comparison
    return (left.kind === 'directory' ? 1 : 2) - (right.kind === 'directory' ? 1 : 2)
  })
  return Object.freeze({
    mode: 'node-id' as const,
    defaultSelected: input.defaultSelected,
    rules: Object.freeze(rules),
  })
}

function snapshotGuarantees(input: GuaranteeSet): GuaranteeSet {
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
    default:
      throw new TypeError('guarantee profile is invalid')
  }
  if (!sameGuarantees(input, expected)) throw new TypeError('guarantee profile fields are invalid')
  return expected
}

function sameGuarantees(left: GuaranteeSet, right: GuaranteeSet): boolean {
  return left.profile === right.profile &&
    left.nameAuthority === right.nameAuthority &&
    left.replacement === right.replacement &&
    left.delivery === right.delivery &&
    left.visibility === right.visibility &&
    left.rollback === right.rollback
}

function canonicalGuarantees(value: GuaranteeSet): CanonicalBytes {
  return concat([
    frame(Uint8Array.of(nameAuthorityByte(value.nameAuthority))),
    frame(Uint8Array.of(replacementGuaranteeByte(value.replacement))),
    frame(Uint8Array.of(value.delivery === 'managed-target' ? 1 : 2)),
    frame(Uint8Array.of(commitVisibilityByte(value.visibility))),
    frame(Uint8Array.of(value.rollback === 'to-absent' ? 1 : 2)),
  ])
}

function completeArtifactName(artifact: ArtifactSpec): string {
  switch (artifact.kind) {
    case 'original-file':
      return artifact.suggestedName
    case 'zip-archive':
      return artifact.suggestedName
    case 'directory-tree':
      throw new TypeError('directory tree is not a complete single artifact')
  }
}

function managedNameAuthority(
  value: NameAuthority,
): 'application-chosen' | 'user-chosen' {
  if (value !== 'application-chosen' && value !== 'user-chosen') {
    throw new TypeError('managed atomic name authority is invalid')
  }
  return value
}

function requireResultName(value: string): string {
  if (typeof value !== 'string' || !isPortableCatalogName(value) ||
      catalogNameCollisionKey(value).startsWith('.wsresume')) {
    throw new TypeError('result name violates the frozen portable policy')
  }
  return value
}

function requireCanonicalPath(value: string): string {
  if (typeof value !== 'string') throw new TypeError('canonical path must be text')
  const canonical = canonicalizePortableCatalogPath(value)
  if (canonical !== value) throw new TypeError('path is not in canonical form')
  return canonical
}

function canonicalPathBytes(pathInput: string): CanonicalBytes {
  const path = requireCanonicalPath(pathInput)
  const segments = path.split('/')
  return concat([
    uint64(BigInt(segments.length)),
    ...segments.map((segment) => frame(TEXT_ENCODER.encode(segment))),
  ])
}

function createRandomIdentity(label: string): string {
  if (globalThis.crypto?.getRandomValues === undefined) {
    throw new DOMException('Secure ' + label + ' identity generation is unavailable', 'NotSupportedError')
  }
  const value = new Uint8Array(STABLE_IDENTITY_BYTES)
  globalThis.crypto.getRandomValues(value)
  if (value.every((byte) => byte === 0)) throw new Error('Generated ' + label + ' identity was all zeroes')
  return encodeBase64Url(value)
}

function requireIdentity(value: string, width: number, label: string): string {
  const bytes = requireIdentityBytes(value, width, label)
  return encodeBase64Url(bytes)
}

function requireIdentityBytes(value: string, width: number, label: string): CanonicalBytes {
  if (typeof value !== 'string') throw new TypeError(label + ' must be a canonical base64url identity')
  const decoded = decodeBase64Url(value)
  if (decoded === undefined || decoded.byteLength !== width ||
      decoded.every((byte) => byte === 0) || encodeBase64Url(decoded) !== value) {
    throw new TypeError(label + ' must be a non-zero canonical ' + width + '-byte identity')
  }
  return Uint8Array.from(decoded)
}

function canonicalValue<T extends object>(fields: T, canonicalBytes: Uint8Array): T & CanonicalValue {
  const stored = Uint8Array.from(canonicalBytes)
  const value = { ...fields } as T & CanonicalValue
  Object.defineProperty(value, 'canonicalBytes', {
    enumerable: true,
    get: () => Uint8Array.from(stored),
  })
  const frozen = Object.freeze(value)
  VALID_CANONICAL_VALUES.add(frozen)
  return frozen
}

function requireCanonicalValueProvenance(value: object, label: string): void {
  if (!VALID_CANONICAL_VALUES.has(value)) {
    throw new TypeError(label + ' must be created or validated by the canonical codec')
  }
}

function canonicalDigestValue<T extends object>(
  fields: T,
  digest: string,
  canonicalBytes: Uint8Array,
): T & CanonicalDigestValue {
  requireIdentity(digest, AUTHORITY_REFERENCE_BYTES, 'canonical digest')
  return canonicalValue({ ...fields, digest }, canonicalBytes)
}

function requireSameCanonicalValue<T extends CanonicalValue>(
  input: T,
  rebuilt: T,
  label: string,
): T {
  if (!(input.canonicalBytes instanceof Uint8Array) ||
      !equalBytes(input.canonicalBytes, rebuilt.canonicalBytes)) {
    throw new TypeError(label + ' canonical bytes do not match its semantic fields')
  }
  return rebuilt
}

function requireSameDigestRecord<T extends CanonicalDigestValue>(
  input: T,
  rebuilt: T,
  label: string,
): T {
  requireSameCanonicalValue(input, rebuilt, label)
  requireIdentity(input.digest, AUTHORITY_REFERENCE_BYTES, label + ' digest')
  if (input.digest !== rebuilt.digest) {
    throw new TypeError(label + ' digest does not match its canonical bytes')
  }
  return rebuilt
}

async function digestText(value: Uint8Array): Promise<string> {
  return encodeBase64Url(await sha256(value))
}

function canonicalRecord(domain: string, fields: readonly Uint8Array[]): CanonicalBytes {
  return concat([
    TEXT_ENCODER.encode(domain),
    Uint8Array.of(0, 1),
    ...fields,
  ])
}

function frame(value: Uint8Array): CanonicalBytes {
  return concat([uint64(BigInt(value.byteLength)), value])
}

function uint32(value: number): CanonicalBytes {
  requireUint32(value, 'u32')
  const result = new Uint8Array(4)
  new DataView(result.buffer).setUint32(0, value)
  return result
}

function uint64(value: bigint): CanonicalBytes {
  if (value < 0n || value > 0xffff_ffff_ffff_ffffn) throw new RangeError('u64 is outside its range')
  const result = new Uint8Array(8)
  new DataView(result.buffer).setBigUint64(0, value)
  return result
}

function requireUint32(value: number, label: string): void {
  if (!Number.isInteger(value) || value < 0 || value > 0xffff_ffff) {
    throw new RangeError(label + ' must be an unsigned 32-bit integer')
  }
}

function concat(parts: readonly Uint8Array[]): CanonicalBytes {
  const total = parts.reduce((sum, part) => sum + part.byteLength, 0)
  const output = new Uint8Array(total)
  let offset = 0
  for (const part of parts) {
    output.set(part, offset)
    offset += part.byteLength
  }
  return output
}

function compareTextBytes(left: string, right: string): number {
  return compareBytes(TEXT_ENCODER.encode(left), TEXT_ENCODER.encode(right))
}

function compareBytes(left: Uint8Array, right: Uint8Array): number {
  const length = Math.min(left.byteLength, right.byteLength)
  for (let index = 0; index < length; index += 1) {
    const difference = left[index]! - right[index]!
    if (difference !== 0) return difference
  }
  return left.byteLength - right.byteLength
}

function reservationKindByte(value: DestinationReservation['kind']): number {
  switch (value) {
    case 'container-root': return 1
    case 'named-container-entry': return 2
    case 'atomic-target': return 3
  }
}

function authorityKindByte(value: DestinationReservation['authorityKind']): number {
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

function commitVisibilityByte(value: CommitVisibility): number {
  switch (value) {
    case 'atomic-commit': return 1
    case 'prefix-visible': return 2
    case 'unobservable': return 3
  }
}
