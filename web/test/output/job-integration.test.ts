import { expect, it } from 'vitest'

import {
  directoryId,
  fileId,
  structuralCatalogNamePolicy,
  type CatalogFileNode,
} from '../../src/catalog/model'
import { ProgressiveCatalogTree } from '../../src/catalog/tree'
import {
  TransferJob,
  type DirectoryDiscoverySource,
  type FileTransferService,
  type PreparedFileTransfer,
} from '../../src/transfer/job'
import { SelectionRules } from '../../src/transfer/selection-rules'
import { PersistentTreeOutputSession } from '../../src/output/persistent-tree/session'
import { StreamingZipArchiveWriter } from '../../src/output/streams/streaming-zip'
import {
  ZipStreamOutputSession,
  type ZipCompletionReport,
} from '../../src/output/streams/zip'
import { MemoryOutputJournal, MemoryOutputTree } from './fakes'
import { MemoryZipCentralDirectorySpool } from './zip-spool-fake'

it('reports a quota-hit file through typed JobOutcome while committing its independent sibling', async () => {
  const root = directoryId('root')
  const failed = fileId('failed')
  const healthy = fileId('healthy')
  const catalog = new ProgressiveCatalogTree(root, structuralCatalogNamePolicy)
  catalog.publishDirectory({
    directoryId: root,
    generation: 'root-generation',
    children: [
      { kind: 'file', id: failed, name: 'failed.bin', expectedSize: 1n },
      { kind: 'file', id: healthy, name: 'healthy.bin', expectedSize: 1n },
    ],
  })
  const outputTree = new MemoryOutputTree()
  outputTree.writeError = new DOMException('quota', 'QuotaExceededError')
  const output = await PersistentTreeOutputSession.open({
    identity: { backend: 'memory-tree', outputSessionId: 'job-output' },
    tree: outputTree,
    journal: new MemoryOutputJournal(),
  })

  const result = await new TransferJob(
    catalog,
    new SelectionRules(true),
    new NoDiscovery(),
    new OneByteFiles(),
    output,
    { shareInstance: 'share', maximumConcurrentFiles: 1 },
  ).run()

  expect(result.outcome.status).toBe('CompletedWithErrors')
  expect(result.outcome.failures).toMatchObject([{ kind: 'file', fileId: failed }])
  expect(outputTree.has(['failed.bin'])).toBe(false)
  expect(outputTree.has(['healthy.bin'])).toBe(true)
})

it('reports post-commit failures through the canonical bounded outcome', async () => {
  const root = directoryId('zip-root')
  const committed = fileId('zip-committed')
  const catalog = new ProgressiveCatalogTree(root, structuralCatalogNamePolicy)
  catalog.publishDirectory({
    directoryId: root,
    generation: 'zip-generation',
    children: [{ kind: 'file', id: committed, name: 'committed.bin', expectedSize: 1n }],
  })
  let report: ZipCompletionReport | undefined
  const output = new ZipStreamOutputSession({
    outputSessionId: 'zip-job',
    archive: new StreamingZipArchiveWriter(
      new WritableStream<Uint8Array>(),
      new MemoryZipCentralDirectorySpool(),
    ),
    reportCompletion: (value) => { report = value },
  })

  const result = await new TransferJob(
    catalog,
    new SelectionRules(true),
    new NoDiscovery(),
    new ReleaseFailingOneByteFiles(),
    output,
    { shareInstance: 'share', maximumConcurrentFiles: 1 },
  ).run()

  expect(result.outcome.status).toBe('CompletedWithErrors')
  expect(result.outcome.failures).toMatchObject([{ kind: 'file', fileId: committed }])
  expect(report?.outcome).toBe(result.outcome)
})

class NoDiscovery implements DirectoryDiscoverySource {
  async listChildren(): Promise<never> {
    throw new Error('Committed root generation must not be rescanned')
  }
}

class OneByteFiles implements FileTransferService {
  async open(file: CatalogFileNode): Promise<PreparedFileTransfer> {
    return {
      source: {
        shareInstance: 'share',
        fileId: file.id,
        fileRevision: `revision-${file.id}`,
      },
      exactSize: 1n,
      transfer: async (transaction) => {
        await transaction.writeRange(0n, Uint8Array.of(1))
        await transaction.checkpoint()
      },
      release: async () => {},
    }
  }
}

class ReleaseFailingOneByteFiles extends OneByteFiles {
  override async open(file: CatalogFileNode): Promise<PreparedFileTransfer> {
    const prepared = await super.open(file)
    return {
      ...prepared,
      release: async () => { throw new Error('lease release failed') },
    }
  }
}
