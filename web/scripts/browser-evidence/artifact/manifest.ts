import { createHash } from 'node:crypto'

import { comparePortablePaths } from '../filesystem/portable-path.ts'

export interface ArtifactManifestIdentity {
  readonly kind: string
  readonly relativePath: string
  readonly mediaType: string
  readonly byteLength: number
  readonly sha256: string
}

const ARTIFACT_MANIFEST_ID_SCHEMA_VERSION = 1 as const
const ARTIFACT_MANIFEST_SET_SCHEMA_VERSION = 1 as const

/**
 * Guard authorization must follow exact bytes and metadata across process
 * boundaries. Content-addressing the full manifest prevents a path-stable file
 * replacement from borrowing another sample's guard result.
 */
export function artifactIdForManifest(artifact: ArtifactManifestIdentity): string {
  const encoded = JSON.stringify({
    schemaVersion: ARTIFACT_MANIFEST_ID_SCHEMA_VERSION,
    kind: artifact.kind,
    relativePath: artifact.relativePath,
    mediaType: artifact.mediaType,
    byteLength: artifact.byteLength,
    sha256: artifact.sha256,
  })
  return `artifact-${createHash('sha256').update(encoded, 'utf8').digest('hex')}`
}

export function artifactManifestSha256(
  artifacts: readonly (ArtifactManifestIdentity & { readonly artifactId: string })[],
): string {
  const canonical = [...artifacts].map((artifact) => ({
    artifactId: artifact.artifactId,
    kind: artifact.kind,
    relativePath: artifact.relativePath,
    mediaType: artifact.mediaType,
    byteLength: artifact.byteLength,
    sha256: artifact.sha256,
  })).sort((left, right) => comparePortablePaths(left.relativePath, right.relativePath) ||
    compareStrings(left.artifactId, right.artifactId))
  return sha256Bytes(Buffer.from(JSON.stringify({
    schemaVersion: ARTIFACT_MANIFEST_SET_SCHEMA_VERSION,
    artifacts: canonical,
  }), 'utf8'))
}

export function sha256Bytes(value: Uint8Array): string {
  return createHash('sha256').update(value).digest('hex')
}

function compareStrings(left: string, right: string): number {
  if (left === right) return 0
  return left < right ? -1 : 1
}
