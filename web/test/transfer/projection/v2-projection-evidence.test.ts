import { describe, expect, it } from 'vitest'

import type { V2CommittedDirectory } from '../../../src/catalog/v2-page-store'
import type { V2CatalogPage } from '../../../src/catalog/v2-records'
import { frozenV2SelectionPolicy } from '../../../src/catalog/v2-selection'
import { encodeBase64Url } from '../../../src/crypto/bytes'
import { projectAuthenticatedV2Generation } from '../../../src/transfer/discovery/v2-projection-evidence'

describe('authenticated catalog generation projection adapter', () => {
  it('streams a committed generation into bounded shape evidence', async () => {
    const fixture = committedFixture()
    const evidence = await projectAuthenticatedV2Generation({
      committed: fixture.committed,
      pages: pages(fixture.page),
      selection: frozenV2SelectionPolicy(true, []),
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
    await expect(projectAuthenticatedV2Generation({
      committed: fixture.committed,
      pages: pages(tampered),
      selection: frozenV2SelectionPolicy(true, []),
      directoryAncestry: [],
      directoryPath: [],
      containingDirectorySelected: false,
      unsettledTargets: [],
    })).rejects.toThrow(/committed generation/u)
  })
})

function committedFixture(): Readonly<{
  committed: V2CommittedDirectory
  page: V2CatalogPage
}> {
  const directoryId = identityBytes(1)
  const generation = identityBytes(2)
  const terminalCommitment = commitment(3)
  const entries: V2CatalogPage['entries'] = Object.freeze([
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
  const page: V2CatalogPage = Object.freeze({
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

async function* pages(page: V2CatalogPage): AsyncGenerator<V2CatalogPage> {
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
