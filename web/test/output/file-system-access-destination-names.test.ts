import { describe, expect, it } from 'vitest'
import { memoryFSAIdentities } from './fsa-mutation-lock-fixture'
import { reopenFileSystemAccessOutput } from '../../src/output/file-system-access/session'
import { isSafeComponentName } from '../../src/output/file-system-access/safe-name'
import { identity } from './planning/fixture'
import {
  MemoryDirectory, MemoryFile, MemoryLockManager,
  MemoryOperationRepository, bindTask, memoryCheckpointFactory, resultRootArtifact,
} from './file-system-access-lifecycle-fixture'

describe('destination-specific source names', () => {
  it.each([
    ...['NUL .txt', 'NUL', 'question?.txt', 'trailing-space ', 'trailing-dot.']
      .map(name => ({ name, path: [name] })),
    { name: 'folder?', path: ['folder?', 'nested.txt'] },
  ])(
    'saves all content under a deterministic safe name and reopens $path', async ({ name, path }) => {
      const parent = new MemoryDirectory('downloads')
      const repository = new MemoryOperationRepository()
      const checkpointFactory = memoryCheckpointFactory()
      const locks = new MemoryLockManager()
      const session = await bindTask({
        parent, repository, checkpointFactory, locks,
        artifact: await resultRootArtifact(), operationSeed: 70,
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

      const summary = session.safeRenamedSummary()
      expect(summary?.count).toBe(1)
      expect(summary?.sample[0]?.logicalPath.at(-1)).toBe(name)
      const physicalName = summary!.sample[0]!.physicalName
      expect(isSafeComponentName(physicalName)).toBe(true)
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
      })
      const retained = await reopened.beginFile(request)
      expect(retained.ownedObjectId).toBe(file.ownedObjectId)
      await retained.close()
      await reopened.close()
    },
  )
})
