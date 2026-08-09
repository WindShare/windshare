import { describe, expect, it } from 'vitest'

import {
  createOriginalFileArtifact,
  createResultRootDirectoryTreeArtifact,
  createSingleFileDirectoryTreeArtifact,
  createZipArchiveArtifact,
} from '../../../src/transfer/intent'
import {
  artifactRequestedName,
  browserHandoffSuggestedName,
  decideCollisionName,
  resultRootLayoutFromProof,
} from '../../../src/output/planning'
import type { ArtifactShapeProof } from '../../../src/transfer/projection'
import { identity, treeProof } from './fixture'

describe('result naming and layout authority', () => {
  it.each([
    ['complete-directory', 'photos', 'photos'],
    ['directory-selection', 'photos', 'photos-selection'],
    ['synthetic-selection', undefined, 'windshare'],
  ] as const)('maps %s to one shared directory/ZIP result root', async (kind, sourcePath, expected) => {
    const proof = treeProof(kind === 'synthetic-selection'
      ? { kind }
      : { kind, anchor: { directoryId: identity(40), sourcePath: sourcePath ?? '' } })
    const layout = resultRootLayoutFromProof(proof)
    if (layout === null) throw new Error('test layout must be settled')
    const directory = await createResultRootDirectoryTreeArtifact(layout)
    const archive = await createZipArchiveArtifact(layout)

    expect(directory.layout.kind).toBe('result-root')
    if (directory.layout.kind !== 'result-root') throw new Error('unexpected directory layout')
    expect(directory.layout.root.canonicalBytes).toEqual(archive.layout.canonicalBytes)
    expect(directory.layout.root.name).toBe(expected)
    expect(archive.suggestedName).toBe(`${expected}.zip`)
  })

  it('keeps the single-file directory layout flat', async () => {
    const artifact = await createSingleFileDirectoryTreeArtifact({
      fileId: identity(41), sourcePath: 'docs/report.txt', outputName: 'report.txt',
    })

    expect(artifact.layout).toMatchObject({
      kind: 'single-file', sourcePath: 'docs/report.txt', outputName: 'report.txt',
    })
    expect(artifactRequestedName(artifact)).toBe('report.txt')
  })

  it('truncates protected suffixes on scalar boundaries and rejects noncanonical names', () => {
    const longLeaf = '界'.repeat(85)
    const layout = resultRootLayoutFromProof(treeProof({
      kind: 'directory-selection',
      anchor: { directoryId: identity(42), sourcePath: longLeaf },
    }))
    if (layout === null) throw new Error('test layout must be settled')

    expect(new TextEncoder().encode(layout.name).byteLength).toBeLessThanOrEqual(255)
    expect(layout.name.endsWith('-selection')).toBe(true)
    expect(() => resultRootLayoutFromProof(treeProof({
      kind: 'complete-directory',
      anchor: { directoryId: identity(43), sourcePath: 'CON' },
    }))).toThrow(/path policy|portable policy/u)
    expect(() => resultRootLayoutFromProof(treeProof({
      kind: 'complete-directory',
      anchor: { directoryId: identity(44), sourcePath: 're\u0301sume\u0301' },
    }))).toThrow(/canonical/u)
  })

  it('persists operation-derived suffix decisions without changing browser suggestions', async () => {
    const original = await createOriginalFileArtifact({
      fileId: identity(45), sourcePath: 'report.txt', suggestedName: 'report.txt',
    })
    const first = await decideCollisionName(identity(46), original, 1)
    const repeated = await decideCollisionName(identity(46), original, 1)
    const otherOperation = await decideCollisionName(identity(47), original, 1)

    expect(first).toEqual(repeated)
    expect(first.reservedName).toMatch(/^report-[0-9a-f]{10}\.txt$/u)
    expect(otherOperation.reservedName).not.toBe(first.reservedName)
    expect(browserHandoffSuggestedName(original)).toBe('report.txt')
  })

  it('returns data-only unsettled layout state', () => {
    const proof = treeProof({ kind: 'unsettled' }) as Extract<ArtifactShapeProof, { kind: 'tree' }>
    expect(resultRootLayoutFromProof(proof)).toBeNull()
  })
})
