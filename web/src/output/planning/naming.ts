import {
  collisionName,
  createCompleteDirectoryResultRoot,
  createDirectorySelectionResultRoot,
  createSyntheticSelectionResultRoot,
} from '../../transfer/intent'
import type {
  ArtifactSpec,
  DirectoryTreeArtifact,
  ResultRootLayout,
} from '../../transfer/intent'
import type { ArtifactShapeProof } from '../../transfer/projection'

export interface CollisionNameDecision {
  readonly requestedName: string
  readonly reservedName: string
  readonly collisionIndex: number
  readonly entryKind: 'file' | 'directory'
}

export function resultRootLayoutFromProof(proof: ArtifactShapeProof): ResultRootLayout | null {
  if (proof.kind !== 'tree') throw new TypeError('result-root layout requires tree proof')
  switch (proof.layoutBasis.kind) {
    case 'unsettled':
      return null
    case 'complete-directory':
      return createCompleteDirectoryResultRoot(
        proof.layoutBasis.anchor.directoryId,
        proof.layoutBasis.anchor.sourcePath,
      )
    case 'directory-selection':
      return createDirectorySelectionResultRoot(
        proof.layoutBasis.anchor.directoryId,
        proof.layoutBasis.anchor.sourcePath,
      )
    case 'synthetic-selection':
      return createSyntheticSelectionResultRoot()
  }
}

export function artifactRequestedName(artifact: ArtifactSpec): string {
  switch (artifact.kind) {
    case 'original-file':
      return artifact.suggestedName
    case 'zip-archive':
      return artifact.suggestedName
    case 'directory-tree':
      return directoryTreeRequestedName(artifact)
  }
}

export function browserHandoffSuggestedName(artifact: ArtifactSpec): string {
  if (artifact.kind === 'directory-tree') {
    throw new TypeError('browser handoff cannot publish a directory tree')
  }
  return artifact.suggestedName
}

export async function decideCollisionName(
  operationId: string,
  artifact: ArtifactSpec,
  collisionIndex: number,
): Promise<CollisionNameDecision> {
  const requestedName = artifactRequestedName(artifact)
  const entryKind = artifact.kind === 'directory-tree' && artifact.layout.kind === 'result-root'
    ? 'directory'
    : 'file'
  const reservedName = await collisionName(
    operationId,
    requestedName,
    collisionIndex,
    entryKind === 'file',
  )
  return Object.freeze({ requestedName, reservedName, collisionIndex, entryKind })
}

function directoryTreeRequestedName(artifact: DirectoryTreeArtifact): string {
  switch (artifact.layout.kind) {
    case 'single-file':
      return artifact.layout.outputName
    case 'result-root':
      return artifact.layout.root.name
    case 'catalog-root':
      throw new TypeError('catalog-root layout binds a container and has no requested child name')
  }
}
