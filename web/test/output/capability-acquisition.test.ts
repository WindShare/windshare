import { describe, expect, it, vi } from 'vitest'

import type { BrowserHandoffCapabilityRuntime } from '../../src/output/portable/packaged-handoff'
import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
  DEFAULT_PORTABLE_MAXIMUM_PARTS,
} from '../../src/transfer/intent'
import {
  authorizeFSAParent,
  fsaParentOffer,
  probeBrowserEnvironment,
  startFSAParentPicker,
} from '../../src/output/capability/acquisition'

describe('browser output capability adapters', () => {
  it('reports only the frozen FSA tree guarantee profile', () => {
    const snapshot = probeBrowserEnvironment({
      showDirectoryPicker: async () => fakeDirectory(),
    })

    expect(snapshot.fsaParent).toMatchObject({
      kind: 'fsa-parent-directory',
      persistence: 'durable-after-repository-commit',
      hardMaximumOutputBytes: null,
      legalProfile: 'fsa-tree',
      guarantees: {
        nameAuthority: 'application-chosen',
        replacement: 'coordinated-no-replace',
        delivery: 'managed-target',
        visibility: 'prefix-visible',
        rollback: 'none',
      },
    })
    expect(snapshot.offers.targets).toEqual([snapshot.fsaParent])
  })

  it('does not invent an FSA offer when the directory picker is absent', () => {
    const snapshot = probeBrowserEnvironment({})
    expect(snapshot.fsaParent).toBeNull()
    expect(snapshot.offers.targets).toEqual([])
  })

  it('records actual File and portable object-URL capability as independent offer facts', () => {
    const supported = fakeBrowserHandoffRuntime(true)
    const snapshot = probeBrowserEnvironment({
      browserHandoff: supported.runtime,
    })

    expect(snapshot.browserHandoff).toMatchObject({
      kind: 'browser-handoff',
      legalProfile: 'browser-handoff',
      persistence: 'none',
      hardMaximumOutputBytes: null,
      objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
      supportsWorkspacePackage: true,
      supportsPortableArtifact: true,
    })
    expect(snapshot.offers.targets).toEqual([snapshot.browserHandoff])
    expect(supported.createObjectURL).toHaveBeenCalledTimes(2)
    expect(supported.revokeObjectURL).toHaveBeenCalledTimes(2)
  })

  it('suppresses workspace redownload when File probing fails but retains bounded portable', () => {
    const unsupportedFile = fakeBrowserHandoffRuntime(false)
    const snapshot = probeBrowserEnvironment({
      browserHandoff: unsupportedFile.runtime,
    }, {
      portable: {
        id: 'bounded-portable-memory',
        kind: 'portable-memory',
        persistence: 'none',
        maximumArtifactBytes: DEFAULT_PORTABLE_ARTIFACT_LIMIT,
        assemblyPartBytes: DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
        maximumParts: DEFAULT_PORTABLE_MAXIMUM_PARTS,
        objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
      },
    })

    expect(snapshot.browserHandoff).toMatchObject({
      kind: 'browser-handoff',
      supportsWorkspacePackage: false,
      supportsPortableArtifact: true,
    })
    expect(snapshot.offers.targets).toEqual([snapshot.browserHandoff])
    expect(snapshot.offers.portable).toMatchObject({
      kind: 'portable-memory',
      maximumArtifactBytes: DEFAULT_PORTABLE_ARTIFACT_LIMIT,
    })
    expect(unsupportedFile.createObjectURL).toHaveBeenCalledTimes(2)
    expect(unsupportedFile.revokeObjectURL).toHaveBeenCalledOnce()
  })

  it('starts the picker synchronously and returns parent authority without choosing an artifact', async () => {
    const events: string[] = []
    const parent = fakeDirectory()
    const promise = startFSAParentPicker({
      showDirectoryPicker: (options) => {
        events.push(`picker:${options.mode}`)
        return Promise.resolve(parent)
      },
    }, fsaParentOffer(), (decision) => events.push(decision.name))
    events.push('returned')

    expect(events).toEqual(['picker:readwrite', 'returned'])
    await expect(promise).resolves.toMatchObject({
      kind: 'fsa-parent-directory-authority',
      parent,
      offer: { legalProfile: 'fsa-tree' },
    })
    expect(events).toEqual([
      'picker:readwrite',
      'returned',
      'receive.authority.acquired',
    ])
  })

  it('reauthorizes a persisted parent and rejects permission denial', async () => {
    const granted = fakeDirectory({ query: 'prompt', request: 'granted' })
    await expect(authorizeFSAParent({
      kind: 'fsa-parent-directory-authority',
      environmentTargetOfferId: fsaParentOffer().id,
      offer: fsaParentOffer(),
      parent: granted,
    })).resolves.toBeUndefined()
    expect(granted.requestPermission).toHaveBeenCalledOnce()

    const denied = fakeDirectory({ query: 'denied', request: 'denied' })
    await expect(authorizeFSAParent({
      kind: 'fsa-parent-directory-authority',
      environmentTargetOfferId: fsaParentOffer().id,
      offer: fsaParentOffer(),
      parent: denied,
    })).rejects.toMatchObject({ name: 'NotAllowedError' })
  })

  it('contains no generic download, selection-shape, or OPFS strategy surface', async () => {
    const module = await import('../../src/output/capability/acquisition')
    expect(module).not.toHaveProperty('acquireOutputCapability')
    expect(module).not.toHaveProperty('OPFS_STAGING_PREFERENCE_BYTES')
    expect(module).not.toHaveProperty('DEFAULT_ARCHIVE_NAME')
  })
})

function fakeBrowserHandoffRuntime(packagedFileSupported: boolean): Readonly<{
  runtime: BrowserHandoffCapabilityRuntime
  createObjectURL: ReturnType<typeof vi.fn>
  revokeObjectURL: ReturnType<typeof vi.fn>
}> {
  let nextObjectUrl = 1
  const createObjectURL = vi.fn((source: Blob) => {
    if (source instanceof CapabilityProbeFile && !packagedFileSupported) {
      throw new DOMException('File object URLs are unavailable', 'NotSupportedError')
    }
    return `blob:capability-${nextObjectUrl++}`
  })
  const revokeObjectURL = vi.fn()
  const runtime = {
    Blob,
    File: CapabilityProbeFile as unknown as typeof File,
    WritableStream,
    URL: { createObjectURL, revokeObjectURL },
    document: {
      createElement: vi.fn(),
      documentElement: { append: vi.fn() },
    },
    setTimeout: vi.fn(() => 1),
    clearTimeout: vi.fn(),
  } as unknown as BrowserHandoffCapabilityRuntime
  return Object.freeze({ runtime, createObjectURL, revokeObjectURL })
}

class CapabilityProbeFile extends Blob {
  readonly name: string
  readonly lastModified: number

  constructor(
    fileBits: BlobPart[],
    fileName: string,
    options: FilePropertyBag = {},
  ) {
    super(fileBits, options)
    this.name = fileName
    this.lastModified = options.lastModified ?? 0
  }
}

function fakeDirectory(permission: Readonly<{
  query: PermissionState
  request: PermissionState
}> = { query: 'granted', request: 'granted' }): FileSystemDirectoryHandle & {
  requestPermission: ReturnType<typeof vi.fn>
} {
  const handle = {
    kind: 'directory' as const,
    name: 'received',
    isSameEntry: vi.fn(async (other: FileSystemHandle) => other === handle),
    queryPermission: vi.fn(async () => permission.query),
    requestPermission: vi.fn(async () => permission.request),
  }
  return handle as unknown as FileSystemDirectoryHandle & {
    requestPermission: ReturnType<typeof vi.fn>
  }
}
