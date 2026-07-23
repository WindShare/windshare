import { describe, expect, it, vi } from 'vitest'

import {
  PORTABLE_DOWNLOAD_MAXIMUM_BYTES,
  PORTABLE_DOWNLOAD_MAXIMUM_PARTS,
  PORTABLE_DOWNLOAD_PART_BYTES,
  browserSupportsPortableDownload,
  createBoundedPortableDownloadStream,
  createPortableBrowserDownload,
  type PortableDownloadWindow,
} from '../../src/output/portable/browser-download'

describe('bounded portable browser download', () => {
  it('owns written bytes and publishes one Blob only after close', async () => {
    let published: { readonly name: string; readonly blob: Blob } | undefined
    const stream = createBoundedPortableDownloadStream('result.bin', {
      createBlob: (parts) => new Blob([...parts]),
      publish: (name, blob) => {
        published = { name, blob }
      },
    }, 4)
    const writer = stream.getWriter()
    const first = Uint8Array.of(1, 2)
    await writer.write(first)
    first.fill(9)
    expect(published).toBeUndefined()
    await writer.write(Uint8Array.of(3, 4))
    await writer.close()
    expect(published?.name).toBe('result.bin')
    const bytes = new Uint8Array(await published!.blob.arrayBuffer())
    expect([...bytes]).toEqual([1, 2, 3, 4])
  })

  it('fails closed at the exact byte ceiling without publishing a prefix', async () => {
    const publish = vi.fn()
    const writer = createBoundedPortableDownloadStream('bounded.bin', {
      createBlob: (parts) => new Blob([...parts]),
      publish,
    }, 3).getWriter()
    await writer.write(Uint8Array.of(1, 2, 3))
    await expect(writer.write(Uint8Array.of(4))).rejects.toMatchObject({
      name: 'QuotaExceededError',
    })
    expect(publish).not.toHaveBeenCalled()
  })

  it('coalesces tiny writes into fixed parts with a cap-derived object bound', async () => {
    let blobParts = -1
    let maximumRetainedParts = 0
    const writer = createBoundedPortableDownloadStream('coalesced.bin', {
      createBlob: (parts) => {
        blobParts = parts.length
        return new Blob([...parts])
      },
      publish: () => {},
      observeAssembly: (snapshot) => {
        maximumRetainedParts = Math.max(maximumRetainedParts, snapshot.retainedParts)
      },
    }).getWriter()
    for (let index = 0; index < 10_000; index += 1) await writer.write(Uint8Array.of(index & 0xff))
    await writer.close()

    expect(PORTABLE_DOWNLOAD_PART_BYTES).toBe(1024 * 1024)
    expect(PORTABLE_DOWNLOAD_MAXIMUM_PARTS).toBe(64)
    expect(blobParts).toBe(1)
    expect(maximumRetainedParts).toBe(1)
  })

  it('discards buffered bytes when the transfer aborts', async () => {
    const publish = vi.fn()
    const writer = createBoundedPortableDownloadStream('aborted.bin', {
      createBlob: (parts) => new Blob([...parts]),
      publish,
    }).getWriter()
    await writer.write(Uint8Array.of(1, 2, 3))
    await writer.abort(new DOMException('stopped', 'AbortError'))
    expect(publish).not.toHaveBeenCalled()
  })

  it('publishes through a temporary object URL and revokes it after a safe delay', async () => {
    let revoke: (() => void) | undefined
    const anchor = {
      download: '', href: '', hidden: false,
      click: vi.fn(),
      remove: vi.fn(),
    }
    const windowPort = {
      Blob,
      WritableStream,
      URL: {
        createObjectURL: vi.fn(() => 'blob:portable'),
        revokeObjectURL: vi.fn(),
      },
      document: {
        createElement: vi.fn(() => anchor),
        documentElement: { append: vi.fn() },
      },
      setTimeout: vi.fn((callback: () => void) => {
        revoke = callback
        return 1
      }),
    } as unknown as PortableDownloadWindow

    expect(browserSupportsPortableDownload(windowPort)).toBe(true)
    const writer = createPortableBrowserDownload('archive.zip', 1n, windowPort).getWriter()
    await writer.write(Uint8Array.of(1))
    await writer.close()
    expect(anchor).toMatchObject({ download: 'archive.zip', href: 'blob:portable', hidden: true })
    expect(anchor.click).toHaveBeenCalledOnce()
    expect(anchor.remove).toHaveBeenCalledOnce()
    expect(windowPort.URL.revokeObjectURL).not.toHaveBeenCalled()
    revoke?.()
    expect(windowPort.URL.revokeObjectURL).toHaveBeenCalledWith('blob:portable')
  })

  it('rejects declared output above the portable memory ceiling before creating a stream', () => {
    const windowPort = {
      Blob,
      WritableStream,
      URL: { createObjectURL: vi.fn(), revokeObjectURL: vi.fn() },
      document: { createElement: vi.fn(), documentElement: {} },
      setTimeout: vi.fn(),
    } as unknown as PortableDownloadWindow
    expect(() => createPortableBrowserDownload(
      'oversized.bin',
      BigInt(PORTABLE_DOWNLOAD_MAXIMUM_BYTES) + 1n,
      windowPort,
    )).toThrow(expect.objectContaining({ name: 'NotSupportedError' }))
    expect(() => createPortableBrowserDownload('negative.bin', -1n, windowPort)).toThrow(RangeError)
    expect(() => createBoundedPortableDownloadStream('invalid-limit.bin', {
      createBlob: (parts) => new Blob([...parts]),
      publish: () => {},
    }, PORTABLE_DOWNLOAD_MAXIMUM_BYTES + 1)).toThrow('fixed assembly bound')
  })
})
