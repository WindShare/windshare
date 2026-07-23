import { describe, expect, it } from 'vitest'

import {
  directoryId,
  fileId,
  scanAttemptId,
  structuralCatalogNamePolicy,
  type CatalogNamePolicy,
} from '../../src/catalog/model'
import { ProgressiveCatalogTree } from '../../src/catalog/tree'

const foldedNames: CatalogNamePolicy = Object.freeze({
  validate: structuralCatalogNamePolicy.validate,
  collisionKey: (name: string) => name.toLocaleLowerCase('en-US'),
})

describe('progressive catalog tree', () => {
  it('retains only published generations while expansion controls visible descendants', () => {
    const root = directoryId('root')
    const first = directoryId('first')
    const sibling = directoryId('sibling')
    const leaf = fileId('leaf')
    const tree = new ProgressiveCatalogTree(root, structuralCatalogNamePolicy)

    tree.publishDirectory({
      directoryId: root,
      generation: 'root-1',
      children: [
        { kind: 'directory', id: first, name: 'first' },
        { kind: 'directory', id: sibling, name: 'sibling' },
      ],
    })
    tree.setExpanded(first, true)
    expect(tree.visibleWindow(10).rows.map(({ node }) => node.name)).toEqual([
      'first',
      'sibling',
    ])
    expect(tree.visibleWindow(2).truncated).toBe(false)
    expect(tree.directoryState(sibling).status).toBe('undiscovered')

    tree.publishDirectory({
      directoryId: first,
      generation: 'first-1',
      children: [{ kind: 'file', id: leaf, name: 'leaf.bin', expectedSize: 7n }],
    })
    expect(tree.visibleWindow(10).rows.map(({ node }) => node.name)).toEqual([
      'first',
      'leaf.bin',
      'sibling',
    ])
    expect(tree.visibleWindow(3).truncated).toBe(false)

    tree.setExpanded(first, false)
    expect(tree.visibleWindow(10).rows.map(({ node }) => node.name)).toEqual([
      'first',
      'sibling',
    ])
    expect(tree.requireFile(leaf).expectedSize).toBe(7n)
    expect(tree.directoryState(sibling).status).toBe('undiscovered')
  })

  it('makes byte-equivalent semantic replay idempotent and conflicting replay atomic', () => {
    const root = directoryId('root')
    const leaf = fileId('leaf')
    const tree = new ProgressiveCatalogTree(root, structuralCatalogNamePolicy)
    const generation = {
      directoryId: root,
      generation: 'g1',
      children: [{ kind: 'file' as const, id: leaf, name: 'leaf', expectedSize: 1n }],
    }

    const committed = tree.publishDirectory(generation)
    expect(tree.publishDirectory(generation)).toBe(committed)
    expect(tree.nodeCount).toBe(2)

    expect(() => tree.publishDirectory({
      directoryId: root,
      generation: 'g2',
      children: [
        { kind: 'file', id: leaf, name: 'changed', expectedSize: 1n },
        { kind: 'file', id: fileId('new'), name: 'new', expectedSize: 2n },
      ],
    })).toThrow(/already bound|different committed generation/u)
    expect(tree.nodeCount).toBe(2)
    expect(tree.node(fileId('new'))).toBeUndefined()
  })

  it('rejects a hostile generation before publishing any child', () => {
    const root = directoryId('root')
    const tree = new ProgressiveCatalogTree(root, foldedNames)
    expect(() => tree.publishDirectory({
      directoryId: root,
      generation: 'g1',
      children: [
        { kind: 'file', id: fileId('one'), name: 'Readme', expectedSize: 1n },
        { kind: 'file', id: fileId('two'), name: 'README', expectedSize: 2n },
      ],
    })).toThrow(/collision/u)
    expect(tree.nodeCount).toBe(1)
    expect(tree.directoryState(root).status).toBe('undiscovered')

    for (const child of [
      { kind: 'file' as const, id: fileId('negative'), name: 'negative', expectedSize: -1n },
      { kind: 'directory' as const, id: directoryId('slash'), name: 'bad/name' },
      {
        kind: 'file' as const,
        id: fileId('mtime'),
        name: 'mtime',
        expectedSize: 0n,
        modifiedTime: { milliseconds: 0n, precisionMilliseconds: 0n },
      },
    ]) {
      expect(() => tree.publishDirectory({
        directoryId: root,
        generation: 'hostile',
        children: [child],
      })).toThrow()
      expect(tree.nodeCount).toBe(1)
    }
  })

  it('requires retry failures to carry an attempt identity and retry delay', () => {
    const root = directoryId('root')
    const tree = new ProgressiveCatalogTree(root, structuralCatalogNamePolicy)
    const attempt = scanAttemptId('attempt')
    expect(() => tree.failDirectory(root, {
      attemptId: attempt,
      kind: 'retryable',
      message: 'try later',
    })).toThrow(/retry delay/u)
    expect(tree.directoryState(root).status).toBe('undiscovered')

    tree.failDirectory(root, {
      attemptId: attempt,
      kind: 'retryable',
      message: 'try later',
      retryAfterMilliseconds: 250,
    })
    expect(tree.directoryState(root)).toMatchObject({
      status: 'failed',
      failure: { kind: 'retryable', retryAfterMilliseconds: 250 },
    })
    expect(() => tree.beginDirectoryLoad(root)).toThrow(/explicit retry/u)
    expect(() => tree.beginDirectoryRetry(root, scanAttemptId('another'))).toThrow(/attempt/u)
    tree.beginDirectoryRetry(root, attempt)
    expect(tree.directoryState(root).status).toBe('loading')
    tree.abandonDirectoryLoad(root)
    expect(tree.directoryState(root)).toMatchObject({
      status: 'failed',
      failure: { attemptId: attempt },
    })
  })

  it('reuses a terminal failure and rejects a conflicting replay', () => {
    const root = directoryId('root')
    const tree = new ProgressiveCatalogTree(root, structuralCatalogNamePolicy)
    const failure = {
      attemptId: scanAttemptId('attempt'),
      kind: 'permanent' as const,
      message: 'denied',
    }
    tree.failDirectory(root, failure)
    tree.failDirectory(root, failure)
    expect(() => tree.failDirectory(root, { ...failure, message: 'changed' }))
      .toThrow(/conflicting/u)
    expect(tree.directoryState(root)).toMatchObject({ status: 'failed', failure })
    expect(() => tree.beginDirectoryRetry(root, failure.attemptId)).toThrow(/retryable/u)
  })

  it('walks a deep expanded tree iteratively and enforces one global row budget', () => {
    const root = directoryId('root')
    const tree = new ProgressiveCatalogTree(root, structuralCatalogNamePolicy)
    let parent = root
    const depth = 1_000
    for (let index = 0; index < depth; index += 1) {
      const child = directoryId(`directory-${index}`)
      tree.publishDirectory({
        directoryId: parent,
        generation: `generation-${index}`,
        children: [{ kind: 'directory', id: child, name: `d${index}` }],
      })
      tree.setExpanded(child, true)
      parent = child
    }

    const window = tree.visibleWindow(37)
    expect(window.rows).toHaveLength(37)
    expect(window.truncated).toBe(true)
    expect(window.rows.at(-1)?.depth).toBe(36)
    expect(tree.outputPath(parent)).toHaveLength(depth)
  })

  it('bounds a wide directory without treating the UI window as catalog pagination', () => {
    const root = directoryId('root')
    const tree = new ProgressiveCatalogTree(root, structuralCatalogNamePolicy)
    tree.publishDirectory({
      directoryId: root,
      generation: 'wide',
      children: Array.from({ length: 1_000 }, (_, index) => ({
        kind: 'file' as const,
        id: fileId(`file-${index}`),
        name: `file-${index}`,
        expectedSize: BigInt(index),
      })),
    })

    expect(tree.nodeCount).toBe(1_001)
    expect(tree.visibleWindow(25)).toMatchObject({ truncated: true })
    expect(tree.visibleWindow(25).rows).toHaveLength(25)
  })
})
