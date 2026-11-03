import { describe, expect, it } from 'vitest'

import {
  inspectFileSystemComponent,
  type FileSystemComponentKind,
} from '../../src/output/browser/filesystem-component-inspection'
import { fitsPortableOutputNameBudget } from '../../src/output/names/policy'
import { prepareCompatibleNameRootRepair } from '../../src/output/file-system-access/compatible-name/coordinator'
import { reserveNewFileSystemAccessOutput, reopenFileSystemAccessOutput } from '../../src/output/file-system-access/session'
import { fsaParentOffer } from '../../src/output/capability/acquisition'
import { acquireFSARootMutationLease, memoryFSAIdentities } from './fsa-mutation-lock-fixture'
import {
  createCompleteDirectoryResultRoot,
  createResultRootDirectoryTreeArtifact,
} from '../../src/transfer/intent'
import { appendProtectedSuffix } from '../../src/transfer/intent/selection'
import { identity } from './planning/fixture'
import {
  MemoryDirectory, MemoryCompatibleNameLedger, MemoryLockManager, MemoryOperationRepository,
  bindTask, memoryCheckpointFactory, resultRootArtifact, singleFileArtifact,
} from './file-system-access-lifecycle-fixture'

const EXPANDED_NAME = '\u0958'.repeat(130).normalize('NFC')
const ENCODER = new TextEncoder()

function inspection(parent: MemoryDirectory, component: string, expectedKind: FileSystemComponentKind = 'file') {
  return {
    verifiedParent: parent as unknown as FileSystemDirectoryHandle, component, expectedKind,
    stage: expectedKind === 'file' ? 'fsa.file.entry.inspect' as const : 'fsa.directory.entry.inspect' as const,
    mode: 'classify-rejection' as const,
  }
}

describe('FSA physical component byte admission', () => {
  it.each(['file', 'directory'] as const)('repairs an absent overlong %s before creating it', async kind => {
    const parent = new MemoryDirectory('downloads')
    const lookups: boolean[] = []
    parent.onEntryLookup = entry => { lookups.push(entry.create) }
    await expect(inspectFileSystemComponent(inspection(parent, EXPANDED_NAME, kind))).rejects.toMatchObject({
      name: 'PathComponentRejectedError', evidenceKind: 'component-byte-budget',
      canonicalComponent: EXPANDED_NAME, expectedKind: kind, preMutation: true,
    })
    expect(lookups).toEqual([false, false])
    expect(parent.entryNames()).toEqual([])
  })

  it('counts encoded bytes and preserves names at the portable limit', async () => {
    const parent = new MemoryDirectory('downloads')
    await expect(inspectFileSystemComponent(inspection(parent, '\u754c'.repeat(85)))).resolves.toBe('absent')
    await expect(inspectFileSystemComponent(inspection(parent, '\u754c'.repeat(86)))).rejects.toMatchObject({
      evidenceKind: 'component-byte-budget',
    })
  })

  it('preserves occupied entries and diagnostic lookups beyond the new-entry budget', async () => {
    const parent = new MemoryDirectory('downloads')
    await parent.getFileHandle(EXPANDED_NAME, { create: true })
    await expect(inspectFileSystemComponent(inspection(parent, EXPANDED_NAME))).resolves.toBe('occupied')
    await expect(inspectFileSystemComponent({
      ...inspection(parent, EXPANDED_NAME + 'x'), mode: 'diagnostic',
    })).resolves.toBe('absent')
  })

  it.each(['NotAllowedError', 'InvalidStateError', 'UnknownError'])(
    'does not turn %s into name repair even for an overlong component', async name => {
      const parent = new MemoryDirectory('downloads')
      const failure = new DOMException('parent unavailable', name)
      parent.onEntryLookup = () => { throw failure }
      await expect(inspectFileSystemComponent(inspection(parent, EXPANDED_NAME))).rejects.toBe(failure)
    },
  )

  it.each(['file', 'directory', 'collision'] as const)('freezes a compatible %s root before namespace mutation', async scenario => {
    const kind = scenario === 'directory' ? 'directory' : 'file'
    const parent = new MemoryDirectory('downloads')
    const locks = new MemoryLockManager()
    const rootLease = await acquireFSARootMutationLease(parent as unknown as FileSystemDirectoryHandle, locks)
    let name = EXPANDED_NAME
    if (scenario === 'collision') name = 'x.' + 'y'.repeat(253)
    if (kind === 'directory') name = appendProtectedSuffix('a'.repeat(250), '-selection')
    if (scenario === 'collision') await parent.getFileHandle(name, { create: true })
    const artifact = kind === 'file'
      ? await singleFileArtifact(name)
      : await createResultRootDirectoryTreeArtifact(createCompleteDirectoryResultRoot(identity(70), name))
    const offer = fsaParentOffer()
    try {
      const reserved = await reserveNewFileSystemAccessOutput({
        authority: {
          kind: 'fsa-parent-directory-authority', targetRouteId: offer.routeId, offer,
          parent: parent as unknown as FileSystemDirectoryHandle,
        },
        artifact, rootLease, operationId: identity(40), reservationId: identity(41), authorityRef: identity(42, 32),
        prepareCompatibleNameRootRepair: input => prepareCompatibleNameRootRepair(input, {
          platform: 'linux', templateProvider: { select: () => { throw new Error('No script template') } },
          randomBits: () => 0,
        }),
      })
      expect(reserved.reservation.requestedName).toBe(name)
      expect(reserved.reservation.collisionIndex).toBe(scenario === 'collision' ? 1 : 0)
      expect(fitsPortableOutputNameBudget(reserved.reservation.logicalReservedName)).toBe(false)
      expect(fitsPortableOutputNameBudget(reserved.reservation.physicalName)).toBe(true)
      expect(reserved.compatibleNameRepair?.bootstrap.initialMapping).toMatchObject({
        logicalPath: [reserved.reservation.logicalReservedName],
        physicalComponent: reserved.reservation.physicalName, entryKind: kind,
        ownershipState: 'selected', commitState: 'uncommitted',
      })
      expect(parent.entryNames()).toEqual(scenario === 'collision' ? [name] : [])
    } finally {
      await rootLease.release()
    }
  })

  it('saves long descendants and reopens their existing mappings and file identity', async () => {
    const parent = new MemoryDirectory('downloads')
    const repository = new MemoryOperationRepository()
    const checkpointFactory = memoryCheckpointFactory()
    const locks = new MemoryLockManager()
    const ledger = new MemoryCompatibleNameLedger(repository)
    const session = await bindTask({
      parent, repository, checkpointFactory, locks, artifact: await resultRootArtifact(), operationSeed: 50,
      openCompatibleNameLedger: async () => ledger,
    })
    const root = await parent.getDirectoryHandle(session.reservation.physicalName) as unknown as MemoryDirectory
    root.onEntryLookup = entry => {
      if (entry.create && ENCODER.encode(entry.name).byteLength > 255) {
        throw new DOMException('name too long', 'NotFoundError')
      }
    }
    const directory = await session.ensureDirectory([EXPANDED_NAME])
    const path = [EXPANDED_NAME, EXPANDED_NAME]
    const request = {
      materializationRelativePath: path,
      openRevision: async () => ({ fileId: identity(61), fileRevision: identity(62), exactSize: 2n }),
    }
    const file = await session.beginFile(request)
    await file.writeRange(0n, Uint8Array.of(4, 2))
    await file.commit()
    const mappings = [...ledger.mappings.values()]
    expect(mappings).toHaveLength(2)
    expect(mappings.every(mapping => mapping.commitState === 'committed' &&
      fitsPortableOutputNameBudget(mapping.physicalComponent))).toBe(true)
    const mappedDirectory = mappings.find(mapping => mapping.entryKind === 'directory')!
    const mappedFile = mappings.find(mapping => mapping.entryKind === 'file')!
    const savedParent = await root.getDirectoryHandle(mappedDirectory.physicalComponent) as unknown as MemoryDirectory
    expect(await savedParent.fileBytes(mappedFile.physicalComponent)).toEqual(Uint8Array.of(4, 2))
    await session.close()
    const reopened = await reopenFileSystemAccessOutput({
      intent: session.intent, operationRepository: repository, checkpointRepositoryFactory: checkpointFactory,
      lockManager: locks, mutationIdentities: memoryFSAIdentities(locks), openCompatibleNameLedger: async () => ledger,
    })
    expect((await reopened.ensureDirectory([EXPANDED_NAME])).ownedObjectId).toBe(directory.ownedObjectId)
    const reopenedFile = await reopened.beginFile(request)
    expect(reopenedFile.ownedObjectId).toBe(file.ownedObjectId)
    expect([...ledger.mappings.values()]).toEqual(mappings)
    await reopenedFile.close()
    await reopened.close()
  })
})
