import { createHash } from 'node:crypto'
import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'

import { beforeAll, describe, expect, it } from 'vitest'

import {
  artifactIdForManifest,
  artifactManifestSha256,
} from '../../scripts/browser-evidence/artifact/manifest.ts'
import {
  comparePortablePaths,
  portablePathCollisionKey,
  requirePortableRelativePath,
} from '../../scripts/browser-evidence/filesystem/portable-path.ts'

interface PortablePathVectors {
  readonly accepted: readonly string[]
  readonly rejected: readonly string[]
  readonly unordered: readonly string[]
  readonly ordered: readonly string[]
  readonly collisions: readonly (readonly [string, string])[]
}

let vectors: PortablePathVectors

beforeAll(async () => {
  const path = fileURLToPath(new URL(
    '../../../testdata/browser-evidence/portable-path-vectors.json',
    import.meta.url,
  ))
  vectors = JSON.parse(await readFile(path, 'utf8')) as PortablePathVectors
})

describe('shared portable path contract', () => {
  it('accepts and rejects the same scalar, NFC, Windows, and device-name vectors as Go', () => {
    for (const path of vectors.accepted) {
      expect(requirePortableRelativePath(path, 'shared vector')).toBe(path)
    }
    for (const path of vectors.rejected) {
      expect(() => requirePortableRelativePath(path, 'shared vector')).toThrow(/portable|Unicode/u)
    }
  })

  it('pins ASCII folding and UTF-8 byte ordering across Go and TypeScript', () => {
    expect([...vectors.unordered].sort(comparePortablePaths)).toEqual(vectors.ordered)
    for (const [left, right] of vectors.collisions) {
      expect(portablePathCollisionKey(left)).toBe(portablePathCollisionKey(right))
    }
  })

  it('uses that UTF-8 order in the authenticated artifact manifest', () => {
    const artifacts = vectors.unordered.map((relativePath) => {
      const manifest = {
        kind: 'process-log',
        relativePath,
        mediaType: 'text/plain',
        byteLength: 0,
        sha256: '0'.repeat(64),
      }
      return { artifactId: artifactIdForManifest(manifest), ...manifest }
    })
    const byPath = new Map(artifacts.map((artifact) => [artifact.relativePath, artifact]))
    const canonical = vectors.ordered.map((path) => {
      const artifact = byPath.get(path)
      if (artifact === undefined) throw new Error(`shared vector lost ${path}`)
      return artifact
    })
    const expectedBytes = Buffer.from(JSON.stringify({
      schemaVersion: 1,
      artifacts: canonical,
    }), 'utf8')
    expect(artifactManifestSha256(artifacts)).toBe(
      createHash('sha256').update(expectedBytes).digest('hex'),
    )
  })
})
