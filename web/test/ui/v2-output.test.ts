import { describe, expect, it, vi } from 'vitest'

import {
  acquireBrowserV2Output,
  browserV2OutputAuthority,
  browserV2OutputCapabilities,
  openBrowserV2OutputSession,
  outputIntentAvailable,
  type V2BrowserOutputSessionFactory,
  type V2BrowserOutputWindow,
} from '../../src/ui/v2-output'
import type { AcquiredOutputCapability } from '../../src/output/capability/acquisition'
import type { OutputSession } from '../../src/transfer/output-session'
import type { V2OutputSelection } from '../../src/transfer/output-selection'

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

  it('isolates a changed selection after refreshing an FSA or OPFS authority', async () => {
    const capabilities: readonly AcquiredOutputCapability[] = [
      { kind: 'PersistentDirectory', root: {} as FileSystemDirectoryHandle },
      {
        kind: 'OriginPrivateStaging',
        root: {} as FileSystemDirectoryHandle,
        output: new WritableStream<Uint8Array>(),
      },
    ]
    for (const capability of capabilities) {
      const openedIds: string[] = []
      const ensuredPaths: string[] = []
      const sessions: V2BrowserOutputSessionFactory = {
        open: async (_acquired, outputSessionId) => {
          openedIds.push(outputSessionId)
          return recordingOutputSession(outputSessionId, ensuredPaths)
        },
      }
      const beforeRefresh = browserV2OutputAuthority(Promise.resolve(capability), sessions)
      const afterRefresh = browserV2OutputAuthority(Promise.resolve(capability), sessions)

      await beforeRefresh.openSelection(outputSelection('resume-first'), new AbortController().signal)
      await afterRefresh.openSelection(outputSelection('resume-second'), new AbortController().signal)

      expect(openedIds).toEqual(['resume-first', 'resume-second'])
      expect(ensuredPaths).toEqual(['folder', 'folder'])
    }
  })
})

function outputSelection(resumeIntentText: string): V2OutputSelection {
  const value = new Uint8Array(16)
  value[0] = 1
  const hash = new Uint8Array(32)
  hash[0] = 1
  return Object.freeze({
    shareInstance: value,
    syntheticRoot: value,
    rootGeneration: value,
    directories: Object.freeze([Object.freeze({
      path: Object.freeze(['folder']),
      directoryId: value,
      generation: value,
    })]),
    files: Object.freeze([]),
    selectionIdentity: hash,
    selectionIdentityText: 'selection',
    canonicalSelection: new Uint8Array(),
    resumeIntent: hash,
    resumeIntentText,
  })
}

function recordingOutputSession(
  outputSessionId: string,
  ensuredPaths: string[],
): OutputSession {
  return {
    identity: { backend: 'recording', outputSessionId },
    capabilities: {
      durability: 'ProcessRestart',
      randomWrite: true,
      fileFailureIsolation: true,
      modificationTime: false,
    },
    ensureDirectory: async (directory) => { ensuredPaths.push(directory.path.join('/')) },
    finalizeDirectory: async () => undefined,
    beginFile: async () => { throw new Error('Authority test does not open files') },
    finishJob: async () => undefined,
    abortJob: async () => undefined,
  }
}
