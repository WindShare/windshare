import { snapshotPortableCatalogPath } from '../../catalog/path-policy'
import type { ResultRootLayout, ReceiveIntent } from '../intent'
import type { ArtifactLayoutClass } from './contract'
import type { LogicalArtifactPath } from './coordinate/direct-tree'

export function artifactLayoutClass(intent: ReceiveIntent): ArtifactLayoutClass {
  switch (intent.artifact.kind) {
    case 'original-file':
      return 'original-file'
    case 'zip-archive':
      return 'zip-result-root'
    case 'directory-tree':
      switch (intent.artifact.layout.kind) {
        case 'single-file': return 'directory-tree-single-file'
        case 'result-root': return 'directory-tree-result-root'
        case 'catalog-root': return 'directory-tree-catalog-root'
      }
  }
}

export function artifactFilePath(
  intent: ReceiveIntent,
  sourcePath: readonly string[],
): LogicalArtifactPath {
  const source = snapshotPortableCatalogPath(sourcePath)
  switch (intent.artifact.kind) {
    case 'original-file':
      requireSourcePath(intent.artifact.sourcePath, source)
      return logicalArtifactPath([intent.artifact.suggestedName])
    case 'zip-archive':
      return resultRootArtifactPath(intent.artifact.layout, source)
    case 'directory-tree':
      switch (intent.artifact.layout.kind) {
        case 'single-file':
          requireSourcePath(intent.artifact.layout.sourcePath, source)
          return logicalArtifactPath([intent.artifact.layout.outputName])
        case 'result-root':
          return resultRootArtifactPath(intent.artifact.layout.root, source)
        case 'catalog-root':
          return logicalArtifactPath(source)
      }
  }
}

export function artifactDirectoryPath(
  intent: ReceiveIntent,
  sourcePath: readonly string[],
): LogicalArtifactPath {
  if (sourcePath.length === 0) {
    if (intent.artifact.kind === 'directory-tree' && intent.artifact.layout.kind === 'result-root' &&
        intent.artifact.layout.root.anchor.kind === 'synthetic-root') {
      return logicalArtifactPath([intent.artifact.layout.root.name])
    }
    if (intent.artifact.kind === 'zip-archive' &&
        intent.artifact.layout.anchor.kind === 'synthetic-root') {
      return logicalArtifactPath([intent.artifact.layout.name])
    }
    return logicalArtifactPath([])
  }
  const source = snapshotPortableCatalogPath(sourcePath)
  switch (intent.artifact.kind) {
    case 'original-file':
      return logicalArtifactPath([])
    case 'zip-archive':
      return resultRootDirectoryPath(intent.artifact.layout, source)
    case 'directory-tree':
      switch (intent.artifact.layout.kind) {
        case 'single-file': return logicalArtifactPath([])
        case 'result-root': return resultRootDirectoryPath(intent.artifact.layout.root, source)
        case 'catalog-root': return logicalArtifactPath(source)
      }
  }
}

function resultRootDirectoryPath(
  root: ResultRootLayout,
  sourcePath: readonly string[],
): LogicalArtifactPath {
  if (root.anchor.kind === 'synthetic-root') {
    return logicalArtifactPath([root.name, ...sourcePath])
  }
  const anchor = root.anchor.sourcePath.split('/')
  if (sourcePath.length < anchor.length && startsWith(anchor, sourcePath)) return logicalArtifactPath([])
  return resultRootArtifactPath(root, sourcePath)
}

export function directoryIsResultRoot(
  intent: ReceiveIntent,
  sourcePath: readonly string[],
): boolean {
  let root: ResultRootLayout | undefined
  if (intent.artifact.kind === 'zip-archive') root = intent.artifact.layout
  if (intent.artifact.kind === 'directory-tree' && intent.artifact.layout.kind === 'result-root') {
    root = intent.artifact.layout.root
  }
  if (root === undefined) return false
  if (root.anchor.kind === 'synthetic-root') return sourcePath.length === 0
  return samePath(sourcePath, root.anchor.sourcePath.split('/'))
}

function resultRootArtifactPath(
  root: ResultRootLayout,
  sourcePath: readonly string[],
): LogicalArtifactPath {
  if (root.anchor.kind === 'synthetic-root') {
    return logicalArtifactPath([root.name, ...sourcePath])
  }
  const anchor = root.anchor.sourcePath.split('/')
  if (!startsWith(sourcePath, anchor)) {
    throw new TypeError('selected source path escapes the frozen result-root anchor')
  }
  return logicalArtifactPath([root.name, ...sourcePath.slice(anchor.length)])
}

function logicalArtifactPath(input: readonly string[]): LogicalArtifactPath {
  if (input.length === 0) return Object.freeze([]) as unknown as LogicalArtifactPath
  return snapshotPortableCatalogPath(input) as LogicalArtifactPath
}

function requireSourcePath(expected: string, actual: readonly string[]): void {
  if (!samePath(expected.split('/'), actual)) {
    throw new TypeError('selected file differs from the frozen single-file artifact')
  }
}

function startsWith(path: readonly string[], prefix: readonly string[]): boolean {
  return path.length >= prefix.length && prefix.every((segment, index) => path[index] === segment)
}

function samePath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((segment, index) => right[index] === segment)
}
