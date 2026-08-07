import { describe, expect, it } from 'vitest'

import {
  OPFS_STAGING_PREFERENCE_BYTES,
  acquireOutputCapability,
} from '../../src/output/capability/acquisition'
import {
  FILE_SYSTEM_ACCESS_BACKEND,
  ORIGIN_PRIVATE_BACKEND,
  SINGLE_FILE_STREAM_BACKEND,
  ZIP_STREAM_BACKEND,
} from '../../src/output/capability/contract'

function identity(seed: number): Uint8Array<ArrayBuffer> {
  return Uint8Array.from({ length: 32 }, (_, index) => (seed + index) & 0xff)
}

describe('progressive output capability acquisition', () => {
  it('starts the directory picker synchronously before discovery can await', async () => {
    const events: string[] = []
    const root = {} as FileSystemDirectoryHandle
    const acquired = acquireOutputCapability(
      'DirectoryTree',
      { kind: 'Progressive' },
      {
        showDirectoryPicker: async () => {
          events.push('picker')
          return root
        },
        resolveRootIdentity: async () => identity(1),
      },
    )
    events.push('returned')

    expect(events).toEqual(['picker', 'returned'])
    await expect(acquired).resolves.toEqual({
      kind: 'PersistentDirectory',
      root,
      rootIdentity: identity(1),
      targetKind: 2,
      backend: FILE_SYSTEM_ACCESS_BACKEND,
      format: 'directory',
    })
  })

  it('acquires an archive save handle synchronously for unknown descendants', async () => {
    const events: string[] = []
    const stream = new WritableStream<Uint8Array>()
    const handle = {
      createWritable: async () => stream,
    } as FileSystemFileHandle
    const acquired = acquireOutputCapability(
      'BrowserDownload',
      { kind: 'Progressive' },
      {
        showSaveFilePicker: async (options) => {
          events.push(`picker:${options?.suggestedName}`)
          return handle
        },
        createTargetIdentity: () => identity(2),
      },
    )
    events.push('returned')

    expect(events).toEqual(['picker:windshare.zip', 'returned'])
    await expect(acquired).resolves.toEqual({
      kind: 'ZipStream',
      output: stream,
      rootIdentity: identity(2),
      targetKind: 2,
      backend: ZIP_STREAM_BACKEND,
      format: 'zip',
    })
  })

  it('pre-acquires final output while staging an unknown selection in OPFS', async () => {
    const events: string[] = []
    const root = {} as FileSystemDirectoryHandle
    const output = new WritableStream<Uint8Array>()
    const handle = { createWritable: async () => output } as FileSystemFileHandle
    const acquired = acquireOutputCapability(
      'BrowserDownload',
      { kind: 'Progressive' },
      {
        showSaveFilePicker: async () => {
          events.push('picker')
          return handle
        },
        getOriginPrivateDirectory: async () => {
          events.push('opfs')
          return root
        },
        resolveRootIdentity: async () => identity(3),
      },
    )
    events.push('returned')

    expect(events).toEqual(['picker', 'opfs', 'returned'])
    await expect(acquired).resolves.toEqual({
      kind: 'OriginPrivateStaging',
      root,
      output,
      rootIdentity: identity(3),
      targetKind: 2,
      backend: ORIGIN_PRIVATE_BACKEND,
      format: 'zip',
    })
  })

  it('selects direct single-file output only when the selection is already known', async () => {
    const stream = new WritableStream<Uint8Array>()
    const acquired = acquireOutputCapability(
      'BrowserDownload',
      { kind: 'KnownSingleFile', suggestedName: 'file.bin', exactBytes: 1n },
      { createDirectStream: () => stream, createTargetIdentity: () => identity(4) },
    )
    await expect(acquired).resolves.toEqual({
      kind: 'SingleFileStream',
      output: stream,
      rootIdentity: identity(4),
      targetKind: 2,
      backend: SINGLE_FILE_STREAM_BACKEND,
      format: 'single-file',
    })
  })

  it('pre-acquires fallback output before persistent staging for unknown or large output', async () => {
    const root = {} as FileSystemDirectoryHandle
    const streams: WritableStream<Uint8Array>[] = []
    const runtime = {
      getOriginPrivateDirectory: async () => root,
      resolveRootIdentity: async () => identity(5),
      createTargetIdentity: () => identity(6),
      createDirectStream: () => {
        const stream = new WritableStream<Uint8Array>()
        streams.push(stream)
        return stream
      },
    }

    await expect(acquireOutputCapability(
      'BrowserDownload',
      { kind: 'Progressive' },
      runtime,
    )).resolves.toEqual({
      kind: 'OriginPrivateStaging',
      root,
      output: streams[0],
      rootIdentity: identity(5),
      targetKind: 2,
      backend: ORIGIN_PRIVATE_BACKEND,
      format: 'zip',
    })
    await expect(acquireOutputCapability(
      'BrowserDownload',
      { kind: 'Progressive', terminalBytes: OPFS_STAGING_PREFERENCE_BYTES },
      runtime,
    )).resolves.toEqual({
      kind: 'OriginPrivateStaging',
      root,
      output: streams[1],
      rootIdentity: identity(5),
      targetKind: 2,
      backend: ORIGIN_PRIVATE_BACKEND,
      format: 'zip',
    })
    expect(streams).toHaveLength(2)
  })

  it('does not mislabel origin-private storage without a final destination as output', async () => {
    await expect(acquireOutputCapability(
      'BrowserDownload',
      { kind: 'Progressive' },
      { getOriginPrivateDirectory: async () => ({} as FileSystemDirectoryHandle) },
    )).rejects.toMatchObject({ name: 'NotSupportedError' })
  })

  it('passes the minimum produced bytes to a bounded fallback factory', async () => {
    const calls: Array<{ readonly name: string; readonly minimumBytes: bigint }> = []
    await acquireOutputCapability(
      'BrowserDownload',
      { kind: 'KnownSingleFile', suggestedName: 'bounded.bin', exactBytes: 17n },
      {
        createDirectStream: (name, minimumBytes) => {
          calls.push({ name, minimumBytes })
          return new WritableStream<Uint8Array>()
        },
      },
    )
    expect(calls).toEqual([{ name: 'bounded.bin', minimumBytes: 17n }])
  })

  it('uses ZIP streaming below the named staging threshold', async () => {
    const stream = new WritableStream<Uint8Array>()
    const acquired = acquireOutputCapability(
      'BrowserDownload',
      { kind: 'Progressive', terminalBytes: OPFS_STAGING_PREFERENCE_BYTES - 1n },
      {
        getOriginPrivateDirectory: async () => ({} as FileSystemDirectoryHandle),
        createDirectStream: () => stream,
        createTargetIdentity: () => identity(7),
      },
    )
    await expect(acquired).resolves.toEqual({
      kind: 'ZipStream',
      output: stream,
      rootIdentity: identity(7),
      targetKind: 2,
      backend: ZIP_STREAM_BACKEND,
      format: 'zip',
    })
  })
})
