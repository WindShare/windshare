import {
  catalogNameCollisionKey,
  isPortableCatalogName,
} from '../../catalog/path-policy'
import { encodeBase64Url } from '../../crypto/bytes'
import type {
  V2CanonicalSelectionRule,
  V2FrozenSelectionPolicy,
} from '../../catalog/v2-selection'
import {
  ARTIFACT_SPEC_DOMAIN,
  RESULT_ROOT_LAYOUT_DOMAIN,
  SELECTION_SPEC_DOMAIN,
  TEXT_ENCODER,
  canonicalDigestValue,
  canonicalPathBytes,
  canonicalRecord,
  canonicalValue,
  compareBytes,
  compareTextBytes,
  concat,
  digestText,
  frame,
  requireCanonicalPath,
  requireSameCanonicalValue,
  requireSameDigestRecord,
  uint64,
} from './canonical'
import { requireIdentity, requireIdentityBytes } from './identity'
import {
  ARCHIVE_EXTENSION,
  ARTIFACT_SPEC_VERSION,
  DEFAULT_RESULT_ROOT_NAME,
  MAX_RESULT_COMPONENT_BYTES,
  MAX_SELECTION_RULES,
  MAX_SELECTION_TARGET_UTF8_BYTES,
  PARTIAL_SELECTION_SUFFIX,
  SELECTION_SPEC_VERSION,
  STABLE_IDENTITY_BYTES,
  type ArtifactSpec,
  type CanonicalBytes,
  type DirectoryResultRootLayout,
  type DirectoryTreeArtifact,
  type DirectoryTreeLayout,
  type NodeIDSelectionRules,
  type NodeSelectionRule,
  type OriginalFileArtifact,
  type ResultRootLayout,
  type SelectionRulesSpec,
  type SelectionSpec,
  type SyntheticResultRootLayout,
  type ZipArchiveArtifact,
} from './model'

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

export function requireResultName(value: string): string {
  if (typeof value !== 'string' || !isPortableCatalogName(value) ||
      catalogNameCollisionKey(value).startsWith('.wsresume')) {
    throw new TypeError('result name violates the frozen portable policy')
  }
  return value
}

export function completeArtifactName(artifact: ArtifactSpec): string {
  switch (artifact.kind) {
    case 'original-file':
      return artifact.suggestedName
    case 'zip-archive':
      return artifact.suggestedName
    case 'directory-tree':
      throw new TypeError('directory tree is not a complete single artifact')
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
