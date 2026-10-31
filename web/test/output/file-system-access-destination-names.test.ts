import { describe, expect, it } from 'vitest'
import { memoryFSAIdentities } from './fsa-mutation-lock-fixture'
import { reopenFileSystemAccessOutput } from '../../src/output/file-system-access/session'
import { decodeCompatibleNameSidecar } from '../../src/output/file-system-access/compatible-name/sidecar-codec'
import { identity } from './planning/fixture'
import {
  MemoryDirectory, MemoryFile, MemoryCompatibleNameLedger, MemoryLockManager,
  MemoryOperationRepository, bindTask, memoryCheckpointFactory, resultRootArtifact,
} from './file-system-access-lifecycle-fixture'

describe('destination-specific source names', () => {
  it.each([
    ...['NUL .txt', 'NUL', 'question?.txt', 'trailing-space ', 'trailing-dot.']
      .map(name => ({ name, path: [name] })),
    { name: 'folder?', path: ['folder?', 'nested.txt'] },
  ])(
    'saves all content and retains the mapping for $path across reopen', async ({ name, path }) => {
      const parent = new MemoryDirectory('downloads')
      const repository = new MemoryOperationRepository()
      const checkpointFactory = memoryCheckpointFactory()
      const locks = new MemoryLockManager()
      const ledger = new MemoryCompatibleNameLedger(repository)
      const compatibleNamePreparation = { platform: 'linux', randomBits: () => 0 }
      const session = await bindTask({
        parent, repository, checkpointFactory, locks,
        artifact: await resultRootArtifact(), operationSeed: 70,
        openCompatibleNameLedger: async () => ledger, compatibleNamePreparation,
      })
      const root = await parent.getDirectoryHandle(session.reservation.physicalName) as unknown as MemoryDirectory
      root.onEntryLookup = lookup => {
        if (lookup.name === name && !lookup.create) throw new TypeError('Destination refuses this name')
      }
      const request = {
        materializationRelativePath: path,
        openRevision: async () => ({ fileId: identity(132), fileRevision: identity(133), exactSize: 2n }),
      }
      if (path.length > 1) await session.ensureDirectory(path.slice(0, -1))
      const file = await session.beginFile(request)
      await file.writeRange(0n, Uint8Array.of(4, 2))
      await file.commit()
      const ordinary = await session.beginFile({
        materializationRelativePath: ['ordinary.txt'],
        openRevision: async () => ({ fileId: identity(134), fileRevision: identity(135), exactSize: 1n }),
      })
      await ordinary.writeRange(0n, Uint8Array.of(7))
      await ordinary.commit()
      await reportCaughtUp(session)
      expect(ledger.header?.repairSummary?.committedCount).toBe(1)
      const sidecarName = ledger.header!.pair.sidecar.physicalName
      const sidecar = await root.getFileHandle(sidecarName) as unknown as MemoryFile
      const report = decodeCompatibleNameSidecar(await sidecar.bytes())
      expect(report.mappings).toHaveLength(1)
      expect(report.mappings[0]?.logicalPath.at(-1)).toBe(name)
      const physicalName = report.mappings[0]!.physicalComponent
      const savedParent = path.length === 1
        ? root
        : await root.getDirectoryHandle(physicalName) as unknown as MemoryDirectory
      const savedName = path.length === 1 ? physicalName : path[1]!
      expect(await (await savedParent.getFileHandle(savedName) as unknown as MemoryFile).bytes()).toEqual(Uint8Array.of(4, 2))
      expect(await (await root.getFileHandle('ordinary.txt') as unknown as MemoryFile).bytes()).toEqual(Uint8Array.of(7))
      const intent = session.intent
      await session.close()
      const reopened = await reopenFileSystemAccessOutput({
        intent, operationRepository: repository, lockManager: locks,
        mutationIdentities: memoryFSAIdentities(locks), checkpointRepositoryFactory: checkpointFactory,
        openCompatibleNameLedger: async () => ledger, compatibleNamePreparation,
      })
      const retained = await reopened.beginFile(request)
      expect(retained.ownedObjectId).toBe(file.ownedObjectId)
      await retained.close()
      await reopened.close()
    },
  )
})

async function reportCaughtUp(session: Awaited<ReturnType<typeof bindTask>>): Promise<void> {
  const source = session.repairProjection
  if (source === undefined) throw new Error('Missing durable name-repair projection')
  let unsubscribe = () => {}
  try {
    await new Promise<void>(resolve => {
      unsubscribe = source.subscribe(summary => {
        if (summary.sidecarSync === 'current' &&
            summary.latestObservedFooter?.committedCount === summary.committedCount) resolve()
      })
    })
  } finally {
    unsubscribe()
  }
}
