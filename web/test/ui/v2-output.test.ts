import { describe, expect, it, vi } from 'vitest'

import {
  acquireBrowserV2Output,
  browserV2OutputAuthority,
  browserV2OutputCapabilities,
  openBrowserV2OutputSession,
  outputLocatorForCapability,
  outputIntentAvailable,
  type BrowserV2OutputIdentityProvider,
  type V2BrowserOutputSessionFactory,
  type V2BrowserOutputWindow,
} from '../../src/ui/v2-output'
import type { AcquiredOutputCapability } from '../../src/output/capability/acquisition'
import {
  FILE_SYSTEM_ACCESS_BACKEND,
  ORIGIN_PRIVATE_BACKEND,
  SINGLE_FILE_STREAM_BACKEND,
  ZIP_STREAM_BACKEND,
} from '../../src/output/capability/contract'
import { DirectoryAdmissionLedger } from '../../src/transfer/directory-admission-ledger'
import type { OutputSession } from '../../src/transfer/output-session'
import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createTransferIntentDraft,
  freezeTransferIntent,
  type TransferIntent,
  type TransferOutputLocator,
} from '../../src/transfer/intent'

interface OutputWindowFixture {
  readonly windowPort: V2BrowserOutputWindow
  readonly anchorClick: ReturnType<typeof vi.fn>
  readonly createObjectURL: ReturnType<typeof vi.fn>
}

function identity(seed: number): Uint8Array<ArrayBuffer> {
  return Uint8Array.from({ length: 32 }, (_, index) => (seed + index) & 0xff)
}

function identityText(seed: number): string {
  return encodeBase64Url(identity(seed))
}

function directoryIdentityText(seed: number): string {
  return encodeBase64Url(Uint8Array.from({ length: 16 }, (_, index) => (seed + index) & 0xff))
}

function testIdentityProvider(seed: number): BrowserV2OutputIdentityProvider {
  return {
    resolveRootIdentity: async () => identity(seed),
    createTargetIdentity: () => identity(seed),
  }
}

function durableCapability(
  kind: 'PersistentDirectory' | 'OriginPrivateStaging',
  seed: number,
): AcquiredOutputCapability {
  return kind === 'PersistentDirectory'
    ? {
        kind,
        root: {} as FileSystemDirectoryHandle,
        rootIdentity: identity(seed),
        targetKind: 2,
        backend: FILE_SYSTEM_ACCESS_BACKEND,
        format: 'directory',
      }
    : {
        kind,
        root: {} as FileSystemDirectoryHandle,
        output: new WritableStream<Uint8Array>(),
        rootIdentity: identity(seed),
        targetKind: 2,
        backend: ORIGIN_PRIVATE_BACKEND,
        format: 'zip',
      }
}

function intentForCapability(
  capability: AcquiredOutputCapability,
  jobIdentitySeed: number,
  overrides: Partial<TransferOutputLocator> = {},
): Promise<TransferIntent> {
  const locator = outputLocatorForCapability(capability)
  const draft = createTransferIntentDraft({
    shareInstance: directoryIdentityText(0x01),
    syntheticRoot: directoryIdentityText(0x11),
    selection: { mode: 'node-id', defaultSelected: true, rules: [] },
    transferJobId: directoryIdentityText(jobIdentitySeed),
  })
  return freezeTransferIntent(draft, { ...locator, ...overrides })
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
    expect(acquired).toMatchObject({
      kind: 'SingleFileStream',
      output: native,
      targetKind: 2,
      backend: SINGLE_FILE_STREAM_BACKEND,
      format: 'single-file',
    })
    expect(acquired.rootIdentity).toHaveLength(32)
    expect(createObjectURL).not.toHaveBeenCalled()
  })

  it('pairs origin-private staging with a pre-acquired portable destination', async () => {
    const root = {} as FileSystemDirectoryHandle
    const { windowPort } = outputWindow({ originPrivate: root })
    const acquired = await acquireBrowserV2Output(
      'download',
      { kind: 'Progressive' },
      windowPort,
      testIdentityProvider(0x20),
    )
    expect(acquired).toMatchObject({
      kind: 'OriginPrivateStaging',
      root,
      rootIdentity: identity(0x20),
      targetKind: 2,
      backend: ORIGIN_PRIVATE_BACKEND,
      format: 'zip',
    })
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

  it('uses a fresh output session per run and admits only the synthetic root at open time', async () => {
      const capabilities: readonly AcquiredOutputCapability[] = [
      durableCapability('PersistentDirectory', 0x30),
      durableCapability('OriginPrivateStaging', 0x40),
    ]
    for (const capability of capabilities) {
      const openedIds: string[] = []
      const ensuredPaths: string[] = []
      const sessions: V2BrowserOutputSessionFactory = {
        open: async (acquired, outputSessionId) => {
          openedIds.push(outputSessionId)
          return recordingOutputSession(outputSessionId, ensuredPaths, acquired)
        },
      }
      const beforeRefresh = browserV2OutputAuthority(Promise.resolve(capability), sessions)
      const afterRefresh = browserV2OutputAuthority(Promise.resolve(capability), sessions)

      await beforeRefresh.openOutput(await intentForCapability(capability, 0x21), new AbortController().signal)
      await afterRefresh.openOutput(await intentForCapability(capability, 0x22), new AbortController().signal)

      expect(openedIds).toHaveLength(2)
      expect(openedIds[0]).not.toBe(openedIds[1])
      expect(openedIds).not.toContain('resume-first')
      expect(openedIds).not.toContain('resume-second')
      expect(ensuredPaths).toEqual([])
    }
  })

  it('freezes the intent only after picker confirmation and records the selected format', async () => {
    let resolveCapability: ((capability: AcquiredOutputCapability) => void) | undefined
    const acquired = new Promise<AcquiredOutputCapability>((resolve) => {
      resolveCapability = resolve
    })
    const opened = vi.fn(async (capability: AcquiredOutputCapability, outputSessionId: string) => {
      expect(capability.kind).toBe('SingleFileStream')
      return recordingOutputSession(outputSessionId, [], capability)
    })
    const authority = browserV2OutputAuthority(acquired, { open: opened })
    const draft = createTransferIntentDraft({
      shareInstance: directoryIdentityText(0x01),
      syntheticRoot: directoryIdentityText(0x11),
      selection: { mode: 'node-id', defaultSelected: true, rules: [] },
      transferJobId: directoryIdentityText(0x23),
    })
    const confirmation = authority.confirmOutput(draft, new AbortController().signal)
    await Promise.resolve()
    expect(opened).not.toHaveBeenCalled()

    const streamIdentity = identity(0x70)
    if (resolveCapability === undefined) throw new Error('capability resolver was not installed')
    resolveCapability({
      kind: 'SingleFileStream',
      output: new WritableStream<Uint8Array>(),
      rootIdentity: streamIdentity,
      targetKind: 2,
      backend: SINGLE_FILE_STREAM_BACKEND,
      format: 'single-file',
    })
    const result = await confirmation
    expect(result.intent.output).toMatchObject({
      target: encodeBase64Url(streamIdentity),
      targetKind: 2,
      backend: 'single-file-stream',
      format: 'single-file',
    })
    expect(opened).toHaveBeenCalledOnce()
    await result.session.abortJob(new DOMException('test cleanup', 'AbortError'))
  })

  it('fails closed when a restored locator or root identity differs from the picker capability', async () => {
    const capability = durableCapability('PersistentDirectory', 0x90)
    const mismatches: readonly Partial<TransferOutputLocator>[] = [
      { target: identityText(0x91) },
      { backend: 'different-output-backend' },
      { format: 'zip' },
    ]
    for (const mismatch of mismatches) {
      const opened = vi.fn(async (acquired: AcquiredOutputCapability, outputSessionId: string) =>
        recordingOutputSession(outputSessionId, [], acquired))
      const authority = browserV2OutputAuthority(Promise.resolve(capability), { open: opened })
      await expect(authority.openOutput(
        await intentForCapability(capability, 0x24, mismatch),
        new AbortController().signal,
      )).rejects.toThrow(/locator does not match/u)
      expect(opened).not.toHaveBeenCalled()
    }
  })

  it('rejects forged final intent fields before the output factory can touch storage', async () => {
    const capability = durableCapability('PersistentDirectory', 0x92)
    const valid = await intentForCapability(capability, 0x25)
    const forged = { ...valid, digest: identityText(0x93) } as TransferIntent
    const opened = vi.fn(async (acquired: AcquiredOutputCapability, outputSessionId: string) =>
      recordingOutputSession(outputSessionId, [], acquired))
    const authority = browserV2OutputAuthority(Promise.resolve(capability), { open: opened })

    await expect(authority.openOutput(forged, new AbortController().signal)).rejects.toThrow(/digest/u)
    expect(opened).not.toHaveBeenCalled()
  })

  it('rejects and aborts a factory session whose backend or format violates the intent', async () => {
    const capability = durableCapability('PersistentDirectory', 0x94)
    const mismatches = [
      { backend: 'wrong-backend', format: 'directory' as const },
      { backend: capability.backend, format: 'zip' as const },
    ]
    for (const mismatch of mismatches) {
      const abortJob = vi.fn(async () => undefined)
      const opened = vi.fn(async (_acquired: AcquiredOutputCapability, outputSessionId: string) => ({
        ...recordingOutputSession(outputSessionId, [], mismatch),
        abortJob,
      }))
      const authority = browserV2OutputAuthority(Promise.resolve(capability), { open: opened })

      await expect(authority.openOutput(
        await intentForCapability(capability, 0x26),
        new AbortController().signal,
      )).rejects.toThrow(/backend or format/u)
      expect(opened).toHaveBeenCalledOnce()
      expect(abortJob).toHaveBeenCalledOnce()
    }
  })

  it('maps every picker capability to its durable output contract', () => {
    expect(outputLocatorForCapability({
      kind: 'PersistentDirectory',
      root: {} as FileSystemDirectoryHandle,
      rootIdentity: identity(0x80),
      targetKind: 2,
      backend: FILE_SYSTEM_ACCESS_BACKEND,
      format: 'directory',
    })).toMatchObject({
      target: identityText(0x80),
      backend: FILE_SYSTEM_ACCESS_BACKEND,
      format: 'directory',
      targetKind: 2,
    })
    expect(outputLocatorForCapability({
      kind: 'ZipStream',
      output: new WritableStream<Uint8Array>(),
      rootIdentity: identity(0x81),
      targetKind: 2,
      backend: ZIP_STREAM_BACKEND,
      format: 'zip',
    })).toMatchObject({
      target: identityText(0x81),
      backend: ZIP_STREAM_BACKEND,
      format: 'zip',
      targetKind: 2,
    })
    expect(outputLocatorForCapability({
      kind: 'OriginPrivateStaging',
      root: {} as FileSystemDirectoryHandle,
      output: new WritableStream<Uint8Array>(),
      rootIdentity: identity(0x82),
      targetKind: 2,
      backend: ORIGIN_PRIVATE_BACKEND,
      format: 'zip',
    })).toMatchObject({
      target: identityText(0x82),
      backend: ORIGIN_PRIVATE_BACKEND,
      format: 'zip',
      targetKind: 2,
    })
  })
})

function recordingOutputSession(
  outputSessionId: string,
  ensuredPaths: string[],
  contract: Pick<AcquiredOutputCapability, 'backend' | 'format'>,
): OutputSession {
  const directoryAdmissions = new DirectoryAdmissionLedger()
  const directStream = contract.format === 'single-file'
  return {
    identity: { backend: contract.backend, outputSessionId },
    format: contract.format,
    capabilities: {
      durability: directStream ? 'None' : 'ProcessRestart',
      randomWrite: !directStream,
      fileFailureIsolation: !directStream,
      modificationTime: false,
    },
    admitDirectory: async (directory, signal) => {
      const admission = await directoryAdmissions.admitDirectory(directory, signal)
      if (directory.path.length > 0) ensuredPaths.push(directory.path.join('/'))
      return admission
    },
    finalizeDirectory: async () => undefined,
    beginFile: async () => { throw new Error('Authority test does not open files') },
    finishJob: async () => undefined,
    abortJob: async () => undefined,
  }
}
