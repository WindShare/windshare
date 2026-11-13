import type { ArtifactSpec, DirectoryTreeArtifact } from '../../transfer/intent'

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
