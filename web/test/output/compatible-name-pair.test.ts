import { describe, expect, it } from 'vitest'
import { directoryId } from '../../src/catalog/model'
import { catalogNameCollisionKey } from '../../src/catalog/path-policy'
import { allocateRestorationPair } from '../../src/output/file-system-access/compatible-name/root-repair'
import {
  COMPATIBLE_NAME_COLLISION_RETRY_LIMIT,
  compatibleNameCandidate,
  compatibleNameRestorationPairCandidate,
} from '../../src/output/file-system-access/compatible-name/naming'
import { MemoryDirectory } from './file-system-access-memory-fs'

const INPUT = { operationId: 'AQIDBAUGBwgJCgsMDQ4PEA', primaryToken: 'abcdef' }

describe('restoration pair namespace allocation', () => {
  it.each(['script', 'sidecar', 'both'] as const)('retries both names when native %s is occupied', async conflict => {
    const parent = new MemoryDirectory('downloads')
    const first = await compatibleNameRestorationPairCandidate({ ...INPUT, attempt: 0 })
    if (conflict !== 'sidecar') await parent.getFileHandle(first.script, { create: true })
    if (conflict !== 'script') await parent.getDirectoryHandle(first.sidecar, { create: true })
    const claims = new WeakMap<FileSystemDirectoryHandle, Set<string>>()
    const handle = parent as unknown as FileSystemDirectoryHandle
    const pair = await allocateRestorationPair({ ...INPUT, parent: handle, claims })
    expect(pair).toEqual(await compatibleNameRestorationPairCandidate({ ...INPUT, attempt: 1 }))
    expect(claims.get(handle)).toEqual(new Set([
      catalogNameCollisionKey(pair.script), catalogNameCollisionKey(pair.sidecar),
    ]))
    expect(pair.script.replace(/\.ps1$/u, '.data')).toBe(pair.sidecar)
  })

  it.each(['script', 'sidecar'] as const)('retries the whole pair for asymmetric declared %s siblings', async conflict => {
    const parent = new MemoryDirectory('downloads') as unknown as FileSystemDirectoryHandle
    const first = await compatibleNameRestorationPairCandidate({ ...INPUT, attempt: 0 })
    const pair = await allocateRestorationPair({
      ...INPUT, parent, claims: new WeakMap(),
      membership: { directoryId: directoryId(INPUT.operationId), generation: INPUT.operationId, hasCommittedName: async name => name === first[conflict] },
    })
    expect(pair).toEqual(await compatibleNameRestorationPairCandidate({ ...INPUT, attempt: 1 }))
  })

  it('does not claim half a pair when every bounded candidate is unavailable', async () => {
    const parent = new MemoryDirectory('downloads') as unknown as FileSystemDirectoryHandle
    const claims = new WeakMap<FileSystemDirectoryHandle, Set<string>>()
    let checks = 0
    await expect(allocateRestorationPair({
      ...INPUT, parent, claims,
      membership: { directoryId: directoryId(INPUT.operationId), generation: INPUT.operationId, hasCommittedName: async () => { checks += 1; return true } },
    })).rejects.toThrow('namespace is exhausted')
    expect(checks).toBe(COMPATIBLE_NAME_COLLISION_RETRY_LIMIT + 1)
    expect(claims.get(parent)?.size).toBe(0)
  })

  it('keeps content retries and pair identity independent and claims scoped to parents', async () => {
    const parent = new MemoryDirectory('downloads') as unknown as FileSystemDirectoryHandle
    const other = new MemoryDirectory('elsewhere') as unknown as FileSystemDirectoryHandle
    const claims = new WeakMap<FileSystemDirectoryHandle, Set<string>>()
    const first = await allocateRestorationPair({ ...INPUT, parent, claims })
    await compatibleNameCandidate({
      ...INPUT, logicalPath: ['root'], entryKind: 'directory', attempt: 1,
    })
    expect(await allocateRestorationPair({ ...INPUT, parent: other, claims })).toEqual(first)
    expect((await allocateRestorationPair({ ...INPUT, parent, claims })).token).not.toBe(first.token)
    expect(first.token).not.toBe(INPUT.primaryToken)
  })
})
