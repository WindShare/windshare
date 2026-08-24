import { snapshotPortableCatalogPath } from '../../../catalog/path-policy'
import {
  validateReceiveIntent,
  type DirectTreePlan,
  type DirectoryTreeArtifact,
  type ReceiveIntent,
} from '../../intent'
import {
  artifactDirectoryPath,
  artifactFilePath,
} from '../artifact-path'

declare const SOURCE_AUTHENTICATION_PATH_BRAND: unique symbol
declare const LOGICAL_ARTIFACT_PATH_BRAND: unique symbol
declare const MATERIALIZATION_ROOT_RELATIVE_PATH_BRAND: unique symbol

export type SourceAuthenticationPath = readonly string[] &
  Readonly<{ [SOURCE_AUTHENTICATION_PATH_BRAND]: true }>
export type LogicalArtifactPath = readonly string[] &
  Readonly<{ [LOGICAL_ARTIFACT_PATH_BRAND]: true }>
export type MaterializationRootRelativePath = readonly string[] &
  Readonly<{ [MATERIALIZATION_ROOT_RELATIVE_PATH_BRAND]: true }>

export type DirectTreeMaterializationCoordinate =
  | 'fsa-reserved-root-relative'
  | 'native-container-relative'

export type DirectTreeRootExpectation =
  | Readonly<{
      kind: 'none'
      anchorKind: 'single-file'
    }>
  | Readonly<{
      kind: 'materialized-directory'
      anchorKind: 'directory' | 'synthetic-root' | 'catalog-root'
      directoryId: string
      relativePath: MaterializationRootRelativePath
    }>

export type DirectTreeDirectoryProjection =
  | Readonly<{
      kind: 'reference'
      sourceAuthenticationPath: SourceAuthenticationPath
      logicalArtifactPath: LogicalArtifactPath
    }>
  | Readonly<{
      kind: 'materialize'
      sourceAuthenticationPath: SourceAuthenticationPath
      logicalArtifactPath: LogicalArtifactPath
      relativePath: MaterializationRootRelativePath
    }>

export interface DirectTreeFileProjection {
  readonly sourceAuthenticationPath: SourceAuthenticationPath
  readonly logicalArtifactPath: LogicalArtifactPath
  readonly relativePath: MaterializationRootRelativePath
}

export interface DirectTreeCoordinateContract {
  readonly intent: DirectTreeReceiveIntent
  readonly coordinate: DirectTreeMaterializationCoordinate
  readonly rootExpectation: DirectTreeRootExpectation
  projectDirectory(sourcePath: readonly string[]): DirectTreeDirectoryProjection
  projectFile(sourcePath: readonly string[]): DirectTreeFileProjection
}

export type DirectTreeReceiveIntent = ReceiveIntent &
  Readonly<{ artifact: DirectoryTreeArtifact; plan: DirectTreePlan }>

/**
 * Validation and projection are coupled so callers cannot derive root authority
 * from an unverified intent or rebase paths differently at later FSA layers.
 */
export async function createDirectTreeCoordinateContract(
  input: ReceiveIntent,
): Promise<DirectTreeCoordinateContract> {
  const validated = await validateReceiveIntent(input)
  if (validated.plan.kind !== 'direct-tree' || validated.artifact.kind !== 'directory-tree') {
    throw new TypeError('DirectTree coordinates require a validated directory-tree intent')
  }
  const intent = validated as DirectTreeReceiveIntent
  const coordinate = materializationCoordinate(intent)
  const rootExpectation = projectRootExpectation(intent, coordinate)

  return Object.freeze({
    intent,
    coordinate,
    rootExpectation,
    projectDirectory: (sourcePath: readonly string[]) =>
      projectDirectory(intent, coordinate, sourcePath),
    projectFile: (sourcePath: readonly string[]) =>
      projectFile(intent, coordinate, sourcePath),
  })
}

export function snapshotSourceAuthenticationPath(
  input: readonly string[],
): SourceAuthenticationPath {
  return snapshotCoordinate(input) as SourceAuthenticationPath
}

export function snapshotLogicalArtifactPath(
  input: readonly string[],
): LogicalArtifactPath {
  return snapshotCoordinate(input) as LogicalArtifactPath
}

export function snapshotMaterializationRootRelativePath(
  input: readonly string[],
): MaterializationRootRelativePath {
  return snapshotCoordinate(input) as MaterializationRootRelativePath
}

export function sameMaterializationRootRelativePath(
  left: MaterializationRootRelativePath,
  right: MaterializationRootRelativePath,
): boolean {
  return left.length === right.length && left.every((segment, index) => segment === right[index])
}

function materializationCoordinate(
  intent: DirectTreeReceiveIntent,
): DirectTreeMaterializationCoordinate {
  const reservation = intent.plan.reservation
  return reservation.kind === 'named-container-entry' &&
    reservation.authorityKind === 'fsa-container'
    ? 'fsa-reserved-root-relative'
    : 'native-container-relative'
}

function projectRootExpectation(
  intent: DirectTreeReceiveIntent,
  coordinate: DirectTreeMaterializationCoordinate,
): DirectTreeRootExpectation {
  switch (intent.artifact.layout.kind) {
    case 'single-file':
      return Object.freeze({ kind: 'none', anchorKind: 'single-file' })
    case 'catalog-root':
      return materializedRoot('catalog-root', intent.syntheticRoot, [])
    case 'result-root': {
      const root = intent.artifact.layout.root
      const relativePath = coordinate === 'fsa-reserved-root-relative' ? [] : [root.name]
      return root.anchor.kind === 'directory'
        ? materializedRoot('directory', root.anchor.directoryId, relativePath)
        : materializedRoot('synthetic-root', intent.syntheticRoot, relativePath)
    }
  }
}

function materializedRoot(
  anchorKind: 'directory' | 'synthetic-root' | 'catalog-root',
  directoryId: string,
  relativePath: readonly string[],
): DirectTreeRootExpectation {
  return Object.freeze({
    kind: 'materialized-directory',
    anchorKind,
    directoryId,
    relativePath: snapshotMaterializationRootRelativePath(relativePath),
  })
}

function projectDirectory(
  intent: DirectTreeReceiveIntent,
  coordinate: DirectTreeMaterializationCoordinate,
  sourcePathInput: readonly string[],
): DirectTreeDirectoryProjection {
  const sourceAuthenticationPath = snapshotSourceAuthenticationPath(sourcePathInput)
  const logicalArtifactPath = artifactDirectoryPath(intent, sourceAuthenticationPath)

  switch (intent.artifact.layout.kind) {
    case 'single-file':
      requireSingleFileAncestor(intent.artifact.layout.sourcePath, sourceAuthenticationPath)
      return Object.freeze({
        kind: 'reference',
        sourceAuthenticationPath,
        logicalArtifactPath,
      })
    case 'catalog-root':
      return materializedDirectory(
        sourceAuthenticationPath,
        logicalArtifactPath,
        sourceAuthenticationPath,
      )
    case 'result-root': {
      const root = intent.artifact.layout.root
      if (root.anchor.kind === 'synthetic-root') {
        return materializedDirectory(
          sourceAuthenticationPath,
          logicalArtifactPath,
          coordinate === 'fsa-reserved-root-relative'
            ? sourceAuthenticationPath
            : logicalArtifactPath,
        )
      }
      const anchor = root.anchor.sourcePath.split('/')
      if (!startsWith(sourceAuthenticationPath, anchor)) {
        if (startsWith(anchor, sourceAuthenticationPath)) {
          return Object.freeze({
            kind: 'reference',
            sourceAuthenticationPath,
            logicalArtifactPath,
          })
        }
        throw new TypeError('directory source path is outside the frozen result-root ancestry')
      }
      return materializedDirectory(
        sourceAuthenticationPath,
        logicalArtifactPath,
        coordinate === 'fsa-reserved-root-relative'
          ? sourceAuthenticationPath.slice(anchor.length)
          : logicalArtifactPath,
      )
    }
  }
}

function projectFile(
  intent: DirectTreeReceiveIntent,
  coordinate: DirectTreeMaterializationCoordinate,
  sourcePathInput: readonly string[],
): DirectTreeFileProjection {
  const sourceAuthenticationPath = snapshotSourceAuthenticationPath(sourcePathInput)
  if (sourceAuthenticationPath.length === 0) {
    throw new TypeError('DirectTree file projection must identify a relative file')
  }
  const logicalArtifactPath = artifactFilePath(intent, sourceAuthenticationPath)
  let rootRelativeSourcePath: readonly string[]

  switch (intent.artifact.layout.kind) {
    case 'single-file':
      rootRelativeSourcePath = coordinate === 'fsa-reserved-root-relative'
        ? []
        : [intent.artifact.layout.outputName]
      break
    case 'catalog-root':
      rootRelativeSourcePath = sourceAuthenticationPath
      break
    case 'result-root': {
      const anchor = intent.artifact.layout.root.anchor
      rootRelativeSourcePath = anchor.kind === 'synthetic-root'
        ? sourceAuthenticationPath
        : sourceAuthenticationPath.slice(anchor.sourcePath.split('/').length)
      break
    }
  }
  if (rootRelativeSourcePath.length === 0 &&
      !(intent.artifact.layout.kind === 'single-file' &&
        coordinate === 'fsa-reserved-root-relative')) {
    throw new TypeError('DirectTree file projection must identify a relative file')
  }
  const relativePath = coordinate === 'native-container-relative'
    ? logicalArtifactPath
    : rootRelativeSourcePath
  return Object.freeze({
    sourceAuthenticationPath,
    logicalArtifactPath,
    relativePath: snapshotMaterializationRootRelativePath(relativePath),
  })
}

function materializedDirectory(
  sourceAuthenticationPath: SourceAuthenticationPath,
  logicalArtifactPath: LogicalArtifactPath,
  relativePath: readonly string[],
): DirectTreeDirectoryProjection {
  return Object.freeze({
    kind: 'materialize',
    sourceAuthenticationPath,
    logicalArtifactPath,
    relativePath: snapshotMaterializationRootRelativePath(relativePath),
  })
}

function requireSingleFileAncestor(
  filePath: string,
  directoryPath: SourceAuthenticationPath,
): void {
  const fileSegments = filePath.split('/')
  if (directoryPath.length >= fileSegments.length || !startsWith(fileSegments, directoryPath)) {
    throw new TypeError('directory source path is outside the frozen single-file ancestry')
  }
}

function snapshotCoordinate(input: readonly string[]): readonly string[] {
  if (!Array.isArray(input)) throw new TypeError('path coordinate must be segmented')
  if (input.length === 0) return Object.freeze([])
  return snapshotPortableCatalogPath(input)
}

function startsWith(path: readonly string[], prefix: readonly string[]): boolean {
  return path.length >= prefix.length && prefix.every((segment, index) => path[index] === segment)
}
