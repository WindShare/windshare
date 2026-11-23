import { describe, expect, it, vi } from 'vitest'
import { byteRange } from '../../src/content/geometry'
import { reopenFSASession } from '../../src/output/backends/fsa'
import { FSAOutputFile } from '../../src/output/backends/fsa-file'
import type { ReceiveStore } from '../../src/output/store/database'
import { openFolderFixture } from './fsa-fixture'

describe('FSA durable coverage', () => {
  it('bounds prefix copies across outages by the increase in sparse coverage', async () => {
    const gib = 1024n ** 3n
    const mib = 1024n ** 2n
    let length = 0n
    let mtime = 0
    const handle = {
      getFile: async () => ({ size: Number(length), lastModified: mtime }),
      createWritable: vi.fn(async () => ({
        write: async (chunk: { position: number }) => { length = BigInt(chunk.position + 1) },
        close: async () => { mtime++ },
      })),
    }
    const putFile = vi.fn(async () => undefined)
    const trace = vi.fn()
    const writer = new FSAOutputFile({
      taskId: 'large', request: { fileId: 'f', path: ['f'], sourcePath: ['f'], exactSize: 120n * gib, revision: 'r' },
      handle: handle as unknown as FileSystemFileHandle, parentDir: {} as FileSystemDirectoryHandle,
      physicalName: 'f', openedAtMs: 0, existing: undefined, store: { putFile } as unknown as ReceiveStore,
      diagnostics: { backend: 'file_system_access', trace: { current: trace } },
    })
    // The sparse fake tracks the highest offset and physical timestamp without allocating the logical prefix.
    const first = 50n * gib, small = first + 20n * mib
    await writer.writeAt(first - 1n, Uint8Array.of(1))
    expect(await writer.checkpoint([byteRange(0n, first)], 'disconnect')).toBe('saved')
    await writer.writeAt(small - 1n, Uint8Array.of(2))
    expect(await writer.checkpoint([byteRange(0n, small)], 'disconnect')).toBe('skipped')
    expect(putFile).toHaveBeenCalledTimes(1)
    expect(trace).toHaveBeenCalledWith(expect.objectContaining({ payload: expect.objectContaining({
      outcome: 'skipped', file_id: 'f', previous_saved_bytes: String(first), added_bytes: String(20n * mib),
    }) }))
    await writer.writeAt(2n * first - 1n, Uint8Array.of(3))
    expect(await writer.checkpoint([byteRange(0n, 2n * first)], 'disconnect')).toBe('saved')
    expect(handle.createWritable.mock.calls).toEqual([[{ keepExistingData: false }], [{ keepExistingData: true }]])
  })

  it.each(['close', 'fingerprint', 'database'] as const)('quarantines the writer after a failed %s', async stage => {
    const { open, session, root, memory } = await openFolderFixture()
    const file = await open('large')
    const handle = await root.getFileHandle('large')
    const cause = new Error('failed ' + stage)
    const abort = vi.fn(async () => undefined)
    if (stage === 'close') {
      const stream = await handle.createWritable()
      vi.spyOn(handle, 'createWritable').mockResolvedValueOnce({ ...stream, close: async () => { throw cause }, abort })
    }
    if (stage === 'fingerprint') vi.spyOn(handle, 'getFile').mockRejectedValueOnce(cause)
    if (stage === 'database') vi.spyOn(memory, 'putFile').mockRejectedValueOnce(cause)
    await file.writeAt(0n, Uint8Array.of(1, 2))
    const ranges = [byteRange(0n, 2n)]
    await expect(file.checkpoint(ranges, 'disconnect')).rejects.toMatchObject({ cause, fault: { scope: 'file-local' } })
    await expect(file.writeAt(2n, Uint8Array.of(3))).rejects.toMatchObject({ cause })
    await expect(file.pause(ranges)).rejects.toMatchObject({ cause })
    await expect(file.commit(ranges)).rejects.toMatchObject({ cause })
    await file.abort()
    if (stage === 'close') expect(abort).toHaveBeenCalledOnce()
    expect(handle.writableCalls).toEqual([false])
    expect(await memory.getFile(session.taskId, 'large')).toBeUndefined()
    await session.close()
  })

  it('continues the disconnect threshold from a verified checkpoint after reopening the session', async () => {
    const { open, session, store, root } = await openFolderFixture()
    const first = await open('large')
    const ranges = [byteRange(0n, 4n)]
    await first.writeAt(0n, Uint8Array.of(1, 2, 3, 4))
    await first.checkpoint(ranges, 'disconnect')
    const task = await session.settle({
      status: 'paused', completedFiles: 0n, completedBytes: 0n, discoveredFiles: 1n, discoveredBytes: 8n,
      discoveryComplete: true, failedFiles: 0n, failedDirectories: 0n,
    })
    await session.close()
    const reopened = await reopenFSASession({ task, store, allowPermissionPrompt: false })
    const file = await reopened.transferOutput.openFile({
      fileId: 'large', revision: 'revision', path: ['large'], sourcePath: ['large'], exactSize: 8n,
    }, new AbortController().signal)
    expect(file.completedRanges).toEqual(ranges)
    await file.writeAt(4n, Uint8Array.of(5))
    expect(await file.checkpoint([byteRange(0n, 5n)], 'disconnect')).toBe('skipped')
    await file.abort()
    expect([...(await root.getFileHandle('large')).bytes]).toEqual([1, 2, 3, 4])
    await reopened.close()
  })

  it('rejects a disk snapshot shorter than the ranges about to be published', async () => {
    const { open, root, session, store } = await openFolderFixture()
    const file = await open('large')
    await file.writeAt(0n, Uint8Array.of(1, 2))
    const handle = await root.getFileHandle('large')
    vi.spyOn(handle, 'getFile').mockResolvedValueOnce(new File([Uint8Array.of(1)], 'large'))
    await expect(file.checkpoint([byteRange(0n, 2n)], 'disconnect')).rejects.toMatchObject({
      fault: { scope: 'file-local', code: 'target-collision' },
    })
    expect(await store.getFile(session.taskId, 'large')).toBeUndefined()
    await file.abort()
    await session.close()
  })

  it('preserves a same-size edit between a disconnect cut and the next write', async () => {
    const { open, session, root } = await openFolderFixture()
    const file = await open('large')
    await file.writeAt(0n, Uint8Array.of(1, 2))
    await file.checkpoint([byteRange(0n, 2n)], 'disconnect')
    const handle = await root.getFileHandle('large')
    handle.overwriteExternally(Uint8Array.of(8, 9))
    await expect(file.writeAt(2n, Uint8Array.of(3))).rejects.toMatchObject({ fault: { code: 'target-collision' } })
    await file.abort()
    expect([...handle.bytes]).toEqual([8, 9])
    expect(handle.writableCalls).toEqual([false])
    await session.close()
  })

  it('cleans verified unfinished files before partial settlement and preserves completed and edited files', async () => {
    const { open, session, root } = await openFolderFixture()
    for (const name of ['finished', 'incomplete', 'changed']) {
      const file = await open(name)
      await file.writeAt(0n, Uint8Array.of(1, 2))
      if (name === 'finished') await file.commit([byteRange(0n, 2n)])
      else await file.checkpoint([byteRange(0n, 2n)], 'disconnect')
    }
    const changed = await root.getFileHandle('changed')
    changed.overwriteExternally(Uint8Array.of(7, 8))
    const record = await session.settle({
      status: 'partial', completedBytes: 2n, completedFiles: 1n, discoveredBytes: 24n, discoveredFiles: 3n,
      discoveryComplete: true, failedFiles: 2n, failedDirectories: 0n,
    })
    expect(record.lifecycle.kind).toBe('partial')
    expect([...root.children.keys()]).toEqual(['finished', 'changed'])
    expect([...changed.bytes]).toEqual([7, 8])
    await session.close()
  })
})
