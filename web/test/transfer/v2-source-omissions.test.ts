import { describe, expect, it } from 'vitest'
import { SelectionPolicy } from '../../src/catalog/selection'
import { createOriginalFileArtifact } from '../../src/transfer/intent'
import { collectExactSingleFileEvidence } from '../../src/transfer/job/single-file-evidence'
import {
  catalogFixture, directoryEntry, fileEntry, identity, identityText,
  planAuthorityFixture, readerFixture, receiveIntentFixture, transferJobFixture,
} from './v2-job-fixture'

describe('source exclusions in authenticated directories', () => {
  it.each(['direct-tree', 'workspace-then-publish', 'portable-handoff'] as const)(
    'downloads supported descendants through %s and counts omissions once', async planKind => {
      const folder = directoryEntry(identity(3), 'folder')
      const nested = directoryEntry(identity(4), 'nested')
      const file = fileEntry(identity(5), 'payload.bin', 4n)
      const selection = new SelectionPolicy(true)
      const catalog = catalogFixture([
        { id: identity(2), entries: [folder] },
        { id: folder.id, entries: [nested], omittedCount: 1n },
        { id: nested.id, entries: [file], omittedCount: 2n },
      ])
      const readers = readerFixture([file])
      const plans = planAuthorityFixture()
      const intent = await receiveIntentFixture({
        planKind, artifactKind: planKind === 'direct-tree' ? 'directory-tree' : 'zip-archive', selection,
      })
      const result = await transferJobFixture({
        catalog: catalog.catalog, selection, intent, plans,
        revisions: readers.revisions, broker: readers.broker,
      }).run()
      expect(result.worker.status).toBe('Succeeded')
      expect(result.worker.failureCount).toBe(0)
      expect(plans.output.commits).toEqual([file.idText])
      expect(result.measure).toMatchObject({
        discovery: 'complete', discoveredFiles: 1, discoveredBytes: 4n, sourceOmittedEntries: 3n,
      })
    },
  )

  it('does not report unselected siblings as missing from an explicit file download', async () => {
    const folder = directoryEntry(identity(3), 'folder')
    const file = fileEntry(identity(5), 'payload.bin', 4n)
    const selection = new SelectionPolicy(false)
    selection.toggle(file, [identityText(2), folder.idText])
    const catalog = catalogFixture([
      { id: identity(2), entries: [folder] },
      { id: folder.id, entries: [file], omittedCount: 3n },
    ])
    const readers = readerFixture([file])
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({ planKind: 'direct-tree', artifactKind: 'directory-tree', selection })
    const result = await transferJobFixture({
      catalog: catalog.catalog, selection, intent, plans,
      revisions: readers.revisions, broker: readers.broker,
    }).run()
    expect(result.worker.status).toBe('Succeeded')
    expect(plans.output.commits).toEqual([file.idText])
    expect(result.measure.sourceOmittedEntries).toBe(0n)

    const evidence = await collectExactSingleFileEvidence({
      catalog: catalog.catalog,
      descriptor: { shareInstance: identity(1), syntheticRoot: identity(2), syntheticRootId: identityText(2) },
      selection: selection.snapshot(),
      artifact: await createOriginalFileArtifact({ fileId: file.idText, sourcePath: 'folder/payload.bin', suggestedName: file.name }),
      signal: new AbortController().signal,
    })
    expect(evidence.fileId).toBe(file.idText)
    expect(evidence.catalogSize).toBe(4n)
  })

  it('keeps an excluded-only directory distinct from an enumeration failure', async () => {
    const folder = directoryEntry(identity(3), 'folder')
    const selection = new SelectionPolicy(true)
    const catalog = catalogFixture([
      { id: identity(2), entries: [folder] },
      { id: folder.id, entries: [], omittedCount: 1n },
    ])
    const readers = readerFixture([])
    const plans = planAuthorityFixture()
    const intent = await receiveIntentFixture({ planKind: 'direct-tree', artifactKind: 'directory-tree', selection })
    const result = await transferJobFixture({
      catalog: catalog.catalog, selection, intent, plans,
      revisions: readers.revisions, broker: readers.broker,
    }).run()
    expect(result.worker.status).toBe('Succeeded')
    expect(result.measure).toMatchObject({ discovery: 'complete', discoveredFiles: 0, sourceOmittedEntries: 1n })
  })
})
