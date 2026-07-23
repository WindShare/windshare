import { describe, expect, it, vi } from 'vitest'

import {
  acquireBrowserV2Output,
  browserV2OutputCapabilities,
  openBrowserV2OutputSession,
  outputIntentAvailable,
  type V2BrowserOutputWindow,
} from '../../src/ui/v2-output'

interface OutputWindowFixture {
  readonly windowPort: V2BrowserOutputWindow
  readonly anchorClick: ReturnType<typeof vi.fn>
  readonly createObjectURL: ReturnType<typeof vi.fn>
}

function outputWindow(options: {
  readonly directory?: FileSystemDirectoryHandle
  readonly saveStream?: WritableStream<Uint8Array>
  readonly originPrivate?: FileSystemDirectoryHandle
  readonly omitStorageManager?: boolean
} = {}): OutputWindowFixture {
  const anchorClick = vi.fn()
  const createObjectURL = vi.fn(() => 'blob:unit-output')
  const anchor = { download: '', href: '', hidden: false, click: anchorClick, remove: vi.fn() }
  const windowPort = {
    Blob,
    WritableStream,
    URL: { createObjectURL, revokeObjectURL: vi.fn() },
    document: {
      createElement: vi.fn(() => anchor),
      documentElement: { append: vi.fn() },
    },
    navigator: options.omitStorageManager === true
      ? {}
      : {
          storage: options.originPrivate === undefined
            ? {}
            : { getDirectory: async () => options.originPrivate },
        },
    setTimeout: vi.fn(() => 1),
    ...(options.directory === undefined
      ? {}
      : { showDirectoryPicker: async () => options.directory }),
    ...(options.saveStream === undefined
      ? {}
      : {
          showSaveFilePicker: async () => ({
            createWritable: async () => options.saveStream,
          } as FileSystemFileHandle),
        }),
  } as unknown as V2BrowserOutputWindow
  return { windowPort, anchorClick, createObjectURL }
}

describe('v2 browser output capabilities', () => {
  it('reports native, portable, and origin-private capabilities independently', () => {
    const directory = {} as FileSystemDirectoryHandle
    const originPrivate = {} as FileSystemDirectoryHandle
    const { windowPort } = outputWindow({ directory, originPrivate })
    const capabilities = browserV2OutputCapabilities(windowPort)
    expect(capabilities).toEqual({
      nativeDirectory: true,
      nativeSave: false,
      portableDownload: true,
      originPrivateStaging: true,
    })
    expect(outputIntentAvailable(capabilities, 'directory')).toBe(true)
    expect(outputIntentAvailable(capabilities, 'download')).toBe(true)
  })

  it('keeps portable output available when StorageManager itself is absent', async () => {
    const { windowPort, anchorClick } = outputWindow({ omitStorageManager: true })
    expect(browserV2OutputCapabilities(windowPort)).toEqual({
      nativeDirectory: false,
      nativeSave: false,
      portableDownload: true,
      originPrivateStaging: false,
    })

    const acquired = await acquireBrowserV2Output(
      'download',
      { kind: 'KnownSingleFile', suggestedName: 'portable.bin', exactBytes: 2n },
      windowPort,
    )
    expect(acquired.kind).toBe('SingleFileStream')
    if (acquired.kind !== 'SingleFileStream') throw new Error('portable output kind mismatch')
    const writer = acquired.output.getWriter()
    await writer.write(Uint8Array.of(1, 2))
    await writer.close()
    expect(anchorClick).toHaveBeenCalledOnce()
  })

  it('uses the bounded portable download when native save is absent', async () => {
    const { windowPort, anchorClick } = outputWindow()
    const acquired = await acquireBrowserV2Output(
      'download',
      { kind: 'KnownSingleFile', suggestedName: 'portable.bin', exactBytes: 2n },
      windowPort,
    )
    expect(acquired.kind).toBe('SingleFileStream')
    if (acquired.kind !== 'SingleFileStream') throw new Error('portable output kind mismatch')
    const writer = acquired.output.getWriter()
    await writer.write(Uint8Array.of(1, 2))
    await writer.close()
    expect(anchorClick).toHaveBeenCalledOnce()
  })

  it('keeps native save authoritative when both save paths exist', async () => {
    const native = new WritableStream<Uint8Array>()
    const { windowPort, createObjectURL } = outputWindow({ saveStream: native })
    const acquired = await acquireBrowserV2Output(
      'download',
      { kind: 'KnownSingleFile', suggestedName: 'native.bin', exactBytes: 1n },
      windowPort,
    )
    expect(acquired).toEqual({ kind: 'SingleFileStream', output: native })
    expect(createObjectURL).not.toHaveBeenCalled()
  })

  it('pairs origin-private staging with a pre-acquired portable destination', async () => {
    const root = {} as FileSystemDirectoryHandle
    const { windowPort } = outputWindow({ originPrivate: root })
    const acquired = await acquireBrowserV2Output(
      'download',
      { kind: 'Progressive' },
      windowPort,
    )
    expect(acquired).toMatchObject({ kind: 'OriginPrivateStaging', root })
    expect(acquired.kind === 'OriginPrivateStaging' && acquired.output).toBeInstanceOf(WritableStream)
  })

  it('does not claim durability for a portable ZIP stream', async () => {
    const { windowPort } = outputWindow()
    const acquired = await acquireBrowserV2Output(
      'download',
      { kind: 'Progressive', terminalBytes: 1n },
      windowPort,
    )
    expect(acquired.kind).toBe('ZipStream')
    const output = await openBrowserV2OutputSession(acquired, 'portable-zip')
    expect(output.capabilities).toMatchObject({
      durability: 'None',
      randomWrite: false,
      fileFailureIsolation: false,
    })
    await output.abortJob(new DOMException('test cleanup', 'AbortError'))
  })
})
