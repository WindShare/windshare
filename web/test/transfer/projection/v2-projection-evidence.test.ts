import { describe, expect, it } from 'vitest'

import type { CommittedDirectory } from '../../../src/catalog/page-store'
import type { CatalogPage } from '../../../src/catalog/records'
import { frozenSelectionPolicy } from '../../../src/catalog/selection'
import { encodeBase64Url } from '../../../src/crypto/bytes'
import { projectAuthenticatedGeneration } from '../../../src/transfer/discovery/projection-evidence'

describe('authenticated catalog generation projection adapter', () => {
  it('streams a committed generation into bounded shape evidence', async () => {
    const fixture = committedFixture()
    const evidence = await projectAuthenticatedGeneration({
      committed: fixture.committed,
      pages: pages(fixture.page),
      selection: frozenSelectionPolicy(true, []),
      directoryAncestry: [],
      directoryPath: [],
      containingDirectorySelected: false,
      unsettledTargets: [{
        kind: 'synthetic-root',
        syntheticRoot: fixture.committed.directoryIdText,
      }],
    })

    expect(evidence.metrics).toEqual({
      fileCountLowerBound: 1,
      directoryCountLowerBound: 1,
      byteCountLowerBound: 17n,
    })
    expect(evidence.selectedRoots).toHaveLength(2)
    expect(evidence.settledTargets).toEqual([{
      kind: 'synthetic-root',
      syntheticRoot: fixture.committed.directoryIdText,
    }])
    expect(evidence.earlyLayoutBasis).toEqual({ kind: 'synthetic-selection' })
  })

  it('rejects pages that do not match committed authenticated authority', async () => {
    const fixture = committedFixture()
    const tampered = Object.freeze({
      ...fixture.page,
      generation: identityBytes(99),
      generationText: identityText(99),
    })
    await expect(projectAuthenticatedGeneration({
      committed: fixture.committed,
      pages: pages(tampered),
      selection: frozenSelectionPolicy(true, []),
      directoryAncestry: [],
      directoryPath: [],
      containingDirectorySelected: false,
      unsettledTargets: [],
    })).rejects.toThrow(/committed generation/u)
  })
})

function committedFixture(): Readonly<{
  committed: CommittedDirectory
  page: CatalogPage
}> {
  const directoryId = identityBytes(1)
  const generation = identityBytes(2)
  const terminalCommitment = commitment(3)
  const entries: CatalogPage['entries'] = Object.freeze([
    Object.freeze({
      kind: 'file' as const,
      id: identityBytes(4),
      idText: identityText(4),
      name: 'one.txt',
      expectedSize: 17n,
    }),
    Object.freeze({
      kind: 'directory' as const,
      id: identityBytes(5),
      idText: identityText(5),
      name: 'empty',
    }),
  ])
  const page: CatalogPage = Object.freeze({
    shareInstance: identityBytes(6),
    directoryId,
    directoryIdText: encodeBase64Url(directoryId),
    generation,
    generationText: encodeBase64Url(generation),
    pageIndex: 0,
    terminal: true,
    previousCommitment: new Uint8Array(32),
    entries,
    omittedCount: 0n,
    objectCommitment: terminalCommitment,
    senderObjectBytes: 128,
  })
  return Object.freeze({
    committed: Object.freeze({
      directoryIdText: page.directoryIdText,
      generationText: page.generationText,
      directoryId,
      generation,
      pageCount: 1,
      entryCount: entries.length,
      omittedCount: 0n,
      terminalCommitment,
    }),
    page,
  })
}

async function* pages(page: CatalogPage): AsyncGenerator<CatalogPage> {
  yield page
}

function identityBytes(seed: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(16)
  new DataView(bytes.buffer).setUint32(12, seed, false)
  return bytes
}

function identityText(seed: number): string {
  return encodeBase64Url(identityBytes(seed))
}

function commitment(seed: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(32)
  new DataView(bytes.buffer).setUint32(28, seed, false)
  return bytes
}
