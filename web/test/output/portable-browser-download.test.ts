import { describe, expect, it, vi } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MS,
  PORTABLE_HANDOFF_MAXIMUM_BYTES,
  BrowserHandoffNotStartedError,
  PortableHandoffError,
  browserSupportsPortableHandoff,
  createBrowserHandoffPublisher,
  issuePortableArtifactAdmission,
  openPortableHandoff,
  type BrowserHandoffPublisher,
  type BrowserHandoffTraceEvent,
  type PortableArtifactAdmission,
  type PortableHandoffWindow,
} from '../../src/output/portable/browser-download'
import {
  createPackagedArtifactHandoffPublisher,
} from '../../src/output/portable/packaged-handoff'
import {
  createPublicationAttempt,
  sealPackagedArtifact,
  type PackagedArtifactV1,
  type PublicationAttemptV1,
} from '../../src/output/workspace/aggregate'
import {
  BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
  createOriginalFileArtifact,
  createPortableBinding,
  createPortableHandoffPlan,
  createReceiveIntent,
  createSelectionSpec,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  type ReceiveIntent,
} from '../../src/transfer/intent'

describe('explicit portable browser handoff', () => {
  it('assembles exactly the admitted artifact and reports DownloadStarted only', async () => {
    const intent = await portableIntent()
    let request: Parameters<BrowserHandoffPublisher['handoff']>[0] | undefined
    const publisher: BrowserHandoffPublisher = {
      handoff(input) {
        request = input
        return input.context.attemptKind === 'workspace'
          ? Object.freeze({
              kind: 'download-started',
              suggestedName: input.suggestedName,
              retryableUntil: input.context.retryableUntil,
            })
          : Object.freeze({
              kind: 'download-started',
              suggestedName: input.suggestedName,
            })
      },
    }
    const snapshots: Array<{ bufferedBytes: number; retainedParts: number }> = []
    const session = await openPortableHandoff({
      intent,
      admission: admission(intent, 4n),
      attemptId: identity(9),
      publisher,
      assembly: {
        Blob,
        WritableStream,
        observeAssembly: (snapshot) => {
          snapshots.push(snapshot)
          if (snapshot.bufferedBytes === 2) throw new Error('diagnostic observer failed')
        },
      },
    })

    const writer = session.writable.getWriter()
    const first = Uint8Array.of(1, 2)
    await writer.write(first)
    first.fill(9)
    await writer.write(Uint8Array.of(3, 4))
    await writer.close()

    await expect(session.result).resolves.toEqual({
      kind: 'download-started',
      suggestedName: 'result.bin',
    })
    expect(request?.context).toEqual({
      attemptKind: 'portable',
      operationId: intent.operationId,
      attemptId: identity(9),
    })
    expect(request?.objectUrlLeaseMilliseconds).toBe(BROWSER_HANDOFF_OBJECT_URL_LEASE_MS)
    expect(new Uint8Array(await request!.source.arrayBuffer())).toEqual(Uint8Array.of(1, 2, 3, 4))
    expect(snapshots.at(-2)).toEqual({
      bufferedBytes: 4,
      retainedParts: 1,
      rejectedWriteBytes: 0,
    })
    expect(snapshots.at(-1)).toEqual({
      bufferedBytes: 0,
      retainedParts: 0,
      rejectedWriteBytes: 0,
    })
    expect('published' in (await session.result)).toBe(false)
  })

  it('rejects an over-limit admission before allocating or invoking a publisher', async () => {
    const intent = await portableIntent()
    const publisher = { handoff: vi.fn() } as unknown as BrowserHandoffPublisher
    const createBlob = vi.fn()
    class ObservedBlob extends Blob {
      constructor(parts?: BlobPart[], options?: BlobPropertyBag) {
        super(parts, options)
        createBlob()
      }
    }

    await expect(openPortableHandoff({
      intent,
      admission: admission(intent, BigInt(PORTABLE_HANDOFF_MAXIMUM_BYTES) + 1n),
      attemptId: identity(9),
      publisher,
      assembly: { Blob: ObservedBlob, WritableStream },
    })).rejects.toMatchObject({ name: 'NotSupportedError' })

    expect(createBlob).not.toHaveBeenCalled()
    expect(publisher.handoff).not.toHaveBeenCalled()
  })

  it('rejects structural admission copies that bypass the exact-preparation issuer', async () => {
    const intent = await portableIntent()
    const publisher = { handoff: vi.fn() } as unknown as BrowserHandoffPublisher
    const invalidAdmission: PortableArtifactAdmission = {
      ...admission(intent, 1n),
      sealedArtifact: {
        artifactKind: 'zip-archive',
        preparationManifestDigest: identity(11, 32),
        sealedZipLayoutDigest: identity(12, 32),
      },
    }

    await expect(openPortableHandoff({
      intent,
      admission: invalidAdmission,
      attemptId: identity(9),
      publisher,
      assembly: { Blob, WritableStream },
    })).rejects.toThrow(/was not issued by exact preparation/u)
    expect(publisher.handoff).not.toHaveBeenCalled()
  })

  it('fails closed when bytes differ from sealed admission and releases retained parts', async () => {
    const intent = await portableIntent()
    const publisher = { handoff: vi.fn() } as unknown as BrowserHandoffPublisher
    const snapshots: Array<{
      bufferedBytes: number
      retainedParts: number
      rejectedWriteBytes: number
    }> = []
    const session = await openPortableHandoff({
      intent,
      admission: admission(intent, 3n),
      attemptId: identity(9),
      publisher,
      assembly: {
        Blob,
        WritableStream,
        observeAssembly: (snapshot) => snapshots.push(snapshot),
      },
    })
    const result = session.result.catch((error: unknown) => error)
    const writer = session.writable.getWriter()
    await writer.write(Uint8Array.of(1, 2, 3))
    await expect(writer.write(Uint8Array.of(4))).rejects.toMatchObject({
      restartReason: 'preparation-invalidated',
    })
    await expect(result).resolves.toMatchObject({
      restartReason: 'preparation-invalidated',
    })
    expect(publisher.handoff).not.toHaveBeenCalled()
    expect(snapshots.at(-2)).toEqual({
      bufferedBytes: 3,
      retainedParts: 1,
      rejectedWriteBytes: 1,
    })
    expect(snapshots.at(-1)).toEqual({
      bufferedBytes: 0,
      retainedParts: 0,
      rejectedWriteBytes: 0,
    })

    const short = await openPortableHandoff({
      intent,
      admission: admission(intent, 3n),
      attemptId: identity(10),
      publisher,
      assembly: { Blob, WritableStream },
    })
    const shortResult = short.result.catch((error: unknown) => error)
    const shortWriter = short.writable.getWriter()
    await shortWriter.write(Uint8Array.of(1, 2))
    await expect(shortWriter.close()).rejects.toMatchObject({
      restartReason: 'preparation-invalidated',
    })
    await expect(shortResult).resolves.toBeInstanceOf(PortableHandoffError)
    expect(publisher.handoff).not.toHaveBeenCalled()
  })

  it('does not embed a fallback for a workspace or directory plan', async () => {
    const selection = await testSelection()
    const artifact = await createOriginalFileArtifact({
      fileId: identity(4),
      sourcePath: 'root/result.bin',
      suggestedName: 'result.bin',
    })
    const workspace = await createWorkspaceBinding({
      operationId: identity(6),
      workspaceId: identity(7),
      artifact,
      repositoryRef: identity(8, 32),
    })
    const plan = await createWorkspaceThenPublishPlan(artifact, workspace)
    const intent = await createReceiveIntent({ selection, artifact, plan })
    const publisher = { handoff: vi.fn() } as unknown as BrowserHandoffPublisher

    await expect(openPortableHandoff({
      intent,
      admission: admission(intent, 1n),
      attemptId: identity(9),
      publisher,
      assembly: { Blob, WritableStream },
    })).rejects.toThrow(/explicit portable materialization plan/u)
    expect(publisher.handoff).not.toHaveBeenCalled()
  })

  it('owns a finite object URL lease and treats the anchor click as the no-return boundary', () => {
    const order: string[] = []
    const traceEvents: BrowserHandoffTraceEvent[] = []
    let expire: (() => void) | undefined
    const anchor = {
      download: '',
      href: '',
      hidden: false,
      click: vi.fn(() => order.push('click')),
      remove: vi.fn(() => order.push('remove')),
    }
    const revokeObjectUrl = vi.fn(() => order.push('revoke'))
    const publisher = createBrowserHandoffPublisher({
      createObjectUrl: vi.fn(() => {
        order.push('create-url')
        return 'blob:portable-secret'
      }),
      revokeObjectUrl,
      createAnchor: () => anchor,
      appendAnchor: () => order.push('append'),
      scheduleObjectUrlLease: (callback, duration) => {
        expect(duration).toBe(BROWSER_HANDOFF_OBJECT_URL_LEASE_MS)
        order.push('lease')
        expire = callback
        return { cancel: vi.fn() }
      },
      trace: (event) => traceEvents.push(event),
    })

    expect(() => publisher.handoff({
      ...portableHandoffRequest(),
      objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MS + 1,
    })).toThrow(/frozen finite object URL lease/u)
    expect(order).toEqual([])

    const result = publisher.handoff({
      context: {
        attemptKind: 'portable',
        operationId: identity(6),
        attemptId: identity(9),
      },
      source: new Blob([Uint8Array.of(1)]),
      exactBytes: 1n,
      suggestedName: 'result.bin',
      objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MS,
    })

    expect(result).toEqual({
      kind: 'download-started',
      suggestedName: 'result.bin',
    })
    expect(order).toEqual(['create-url', 'lease', 'append', 'click', 'remove'])
    expect(revokeObjectUrl).not.toHaveBeenCalled()
    expire?.()
    expect(revokeObjectUrl).toHaveBeenCalledWith('blob:portable-secret')
    expect(traceEvents.map((event) => event.name)).toEqual([
      'receive.handoff.started',
      'receive.handoff.download_started',
    ])
    expect(JSON.stringify(traceEvents)).not.toContain('result.bin')
    expect(JSON.stringify(traceEvents)).not.toContain('blob:portable-secret')
  })

  it('does not retract DownloadStarted after the anchor click is invoked', () => {
    const events: BrowserHandoffTraceEvent[] = []
    const cancel = vi.fn()
    const revoke = vi.fn()
    let expire: (() => void) | undefined
    const publisher = createBrowserHandoffPublisher({
      createObjectUrl: () => 'blob:started',
      revokeObjectUrl: revoke,
      createAnchor: () => ({
        download: '',
        href: '',
        hidden: false,
        click: () => { throw new Error('outcome is unobservable after invocation') },
        remove: vi.fn(),
      }),
      appendAnchor: () => {},
      scheduleObjectUrlLease: (callback) => {
        expire = callback
        return { cancel }
      },
      trace: (event) => events.push(event),
    })

    expect(publisher.handoff(portableHandoffRequest())).toEqual({
      kind: 'download-started',
      suggestedName: 'result.bin',
    })
    expect(cancel).not.toHaveBeenCalled()
    expect(revoke).not.toHaveBeenCalled()
    expire?.()
    expect(revoke).toHaveBeenCalledWith('blob:started')
    expect(events.map((event) => event.name)).toEqual([
      'receive.handoff.started',
      'receive.handoff.download_started',
    ])
  })

  it('revokes before reporting a provably pre-click handoff failure', () => {
    const events: BrowserHandoffTraceEvent[] = []
    const cancel = vi.fn()
    const revoke = vi.fn()
    const click = vi.fn()
    const publisher = createBrowserHandoffPublisher({
      createObjectUrl: () => 'blob:not-started',
      revokeObjectUrl: revoke,
      createAnchor: () => ({
        download: '',
        href: '',
        hidden: false,
        click,
        remove: vi.fn(),
      }),
      appendAnchor: () => { throw new Error('anchor was not attached') },
      scheduleObjectUrlLease: () => ({ cancel }),
      trace: (event) => events.push(event),
    })

    expect(() => publisher.handoff(portableHandoffRequest()))
      .toThrow(BrowserHandoffNotStartedError)
    expect(click).not.toHaveBeenCalled()
    expect(cancel).toHaveBeenCalledOnce()
    expect(revoke).toHaveBeenCalledWith('blob:not-started')
    expect(events.map((event) => event.name)).toEqual([
      'receive.handoff.started',
      'receive.handoff.not_started',
    ])
  })
})

describe('browser handoff publisher integration seam', () => {
  it('exposes the same publisher port for a later immutable packaged File handoff', () => {
    const publisher = createBrowserHandoffPublisher({
      createObjectUrl: () => 'blob:package',
      revokeObjectUrl: () => {},
      createAnchor: () => ({
        download: '',
        href: '',
        hidden: false,
        click: () => {},
        remove: () => {},
      }),
      appendAnchor: () => {},
      scheduleObjectUrlLease: () => ({ cancel: () => {} }),
    })
    const retryableUntil = 1_800_000_000_000
    const result = publisher.handoff({
      context: {
        attemptKind: 'workspace',
        operationId: identity(6),
        attemptId: identity(9),
        packageDigest: identity(10, 32),
        retryableUntil,
      },
      source: new Blob([Uint8Array.of(7, 8)]),
      exactBytes: 2n,
      suggestedName: 'sealed.bin',
      objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MS,
    })

    expect(result).toEqual({
      kind: 'download-started',
      suggestedName: 'sealed.bin',
      retryableUntil,
    })
  })

  it('requires the complete browser primitives without probing storage or save pickers', () => {
    const runtime = {
      Blob,
      WritableStream,
      URL: { createObjectURL: vi.fn(), revokeObjectURL: vi.fn() },
      document: {
        createElement: vi.fn(),
        documentElement: { append: vi.fn() },
      },
      setTimeout: vi.fn(),
      clearTimeout: vi.fn(),
    } as unknown as PortableHandoffWindow

    expect(browserSupportsPortableHandoff(runtime)).toBe(true)
    expect(browserSupportsPortableHandoff({
      ...runtime,
      clearTimeout: undefined,
    } as unknown as PortableHandoffWindow)).toBe(false)
  })
})

describe('immutable packaged File browser handoff', () => {
  it('creates a fresh bounded URL for each attempt without changing package or expiry identity', async () => {
    const artifact = await packagedArtifact(3n)
    const firstAttempt = await packagedAttempt(artifact, 21, true)
    const secondAttempt = await packagedAttempt(artifact, 22, true)
    const retryableUntil = 1_800_000_000_000
    const files: TestFile[] = []
    const sources: Blob[] = []
    const objectUrls: string[] = []
    const revoked: string[] = []
    const leaseCallbacks: Array<() => void> = []
    const leaseDurations: number[] = []
    const leaseStarts = [4_000, 9_000]
    const packages = {
      readPackagedArtifact: vi.fn(async () => {
        const file = new TestFile([Uint8Array.of(4, 5, 6)], 'sealed-package.bin')
        files.push(file)
        return file
      }),
    }
    const browser = createBrowserHandoffPublisher({
      createObjectUrl: (source) => {
        sources.push(source)
        const objectUrl = `blob:package-${sources.length}`
        objectUrls.push(objectUrl)
        return objectUrl
      },
      revokeObjectUrl: (objectUrl) => revoked.push(objectUrl),
      createAnchor: () => ({
        download: '',
        href: '',
        hidden: false,
        click: () => {},
        remove: () => {},
      }),
      appendAnchor: () => {},
      scheduleObjectUrlLease: (callback, duration) => {
        leaseCallbacks.push(callback)
        leaseDurations.push(duration)
        return { cancel: () => {} }
      },
      now: () => leaseStarts[sources.length - 1]!,
    })
    const publisher = createPackagedArtifactHandoffPublisher({
      packages,
      browser,
      File: TestFile as unknown as typeof File,
    })
    const packageDigest = artifact.digest
    const receiveIntentDigest = artifact.receiveIntentDigest

    const first = await publisher.handoff({
      artifact,
      attempt: firstAttempt,
      retryableUntil,
    })
    const second = await publisher.handoff({
      artifact,
      attempt: secondAttempt,
      retryableUntil,
    })

    expect(first).toEqual({
      result: {
        kind: 'download-started',
        suggestedName: 'sealed-result.bin',
        retryableUntil,
      },
      urlLeaseStartedAt: 4_000,
      urlLeaseEndsAt: 64_000,
    })
    expect(second).toEqual({
      result: {
        kind: 'download-started',
        suggestedName: 'sealed-result.bin',
        retryableUntil,
      },
      urlLeaseStartedAt: 9_000,
      urlLeaseEndsAt: 69_000,
    })
    expect(files).toHaveLength(2)
    expect(files[0]).not.toBe(files[1])
    expect(sources).toEqual(files)
    expect(new Set(objectUrls).size).toBe(2)
    expect(leaseDurations).toEqual([
      BROWSER_HANDOFF_OBJECT_URL_LEASE_MS,
      BROWSER_HANDOFF_OBJECT_URL_LEASE_MS,
    ])
    expect(artifact.digest).toBe(packageDigest)
    expect(artifact.receiveIntentDigest).toBe(receiveIntentDigest)
    expect(packages.readPackagedArtifact).toHaveBeenNthCalledWith(1, artifact)
    expect(packages.readPackagedArtifact).toHaveBeenNthCalledWith(2, artifact)

    leaseCallbacks.forEach((expire) => expire())
    expect(revoked).toEqual(objectUrls)
  })

  it('suppresses only unsupported packaged File attempts and never falls back to a Blob', async () => {
    const artifact = await packagedArtifact(1n)
    const unsupportedAttempt = await packagedAttempt(artifact, 23, false)
    const supportedAttempt = await packagedAttempt(artifact, 24, true)
    const createObjectUrl = vi.fn(() => 'blob:must-not-start')
    const browser = createBrowserHandoffPublisher({
      createObjectUrl,
      revokeObjectUrl: vi.fn(),
      createAnchor: () => ({
        download: '',
        href: '',
        hidden: false,
        click: vi.fn(),
        remove: vi.fn(),
      }),
      appendAnchor: vi.fn(),
      scheduleObjectUrlLease: () => ({ cancel: vi.fn() }),
    })
    const unsupportedRead = vi.fn(async () =>
      new TestFile([Uint8Array.of(1)], 'sealed-result.bin'))
    const unsupported = createPackagedArtifactHandoffPublisher({
      packages: { readPackagedArtifact: unsupportedRead },
      browser,
      File: TestFile as unknown as typeof File,
    })

    await expect(unsupported.handoff({
      artifact,
      attempt: unsupportedAttempt,
      retryableUntil: 10_000,
    })).rejects.toMatchObject({ name: 'NotSupportedError' })
    expect(unsupportedRead).not.toHaveBeenCalled()

    const blobOnly = createPackagedArtifactHandoffPublisher({
      packages: {
        readPackagedArtifact: vi.fn(async () => new Blob([Uint8Array.of(1)])),
      },
      browser,
      File: TestFile as unknown as typeof File,
    })
    await expect(blobOnly.handoff({
      artifact,
      attempt: supportedAttempt,
      retryableUntil: 10_000,
    })).rejects.toMatchObject({ name: 'NotSupportedError' })
    expect(createObjectUrl).not.toHaveBeenCalled()
  })
})

async function packagedArtifact(exactBytes: bigint): Promise<PackagedArtifactV1> {
  return sealPackagedArtifact({
    operationId: identity(6),
    receiveIntentDigest: identity(13, 32),
    sealedMaterializationDigest: identity(14, 32),
    artifactSpecDigest: identity(15, 32),
    packageOwnedObjectId: identity(16, 32),
    exactBytes,
    artifactReceiptDigest: identity(17, 32),
    layoutDigest: identity(18, 32),
  })
}

async function packagedAttempt(
  artifact: PackagedArtifactV1,
  seed: number,
  packagedFileSupported: boolean,
): Promise<PublicationAttemptV1> {
  return createPublicationAttempt({
    publicationAttemptId: identity(seed),
    operationId: artifact.operationId,
    receiveIntentDigest: artifact.receiveIntentDigest,
    packagedArtifactDigest: artifact.digest,
    route: {
      kind: 'handoff',
      suggestedName: 'sealed-result.bin',
      packagedFileSupported,
      objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS,
    },
  })
}

class TestFile extends Blob {
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

async function portableIntent(): Promise<ReceiveIntent> {
  const selection = await testSelection()
  const artifact = await createOriginalFileArtifact({
    fileId: identity(4),
    sourcePath: 'root/result.bin',
    suggestedName: 'result.bin',
  })
  const portable = await createPortableBinding({
    operationId: identity(6),
    portablePlanId: identity(7),
    artifact,
  })
  const plan = await createPortableHandoffPlan(artifact, portable)
  return createReceiveIntent({ selection, artifact, plan })
}

async function testSelection() {
  return createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: {
      mode: 'node-id',
      defaultSelected: true,
      rules: [],
    },
  })
}

function portableHandoffRequest(): Parameters<BrowserHandoffPublisher['handoff']>[0] {
  return {
    context: {
      attemptKind: 'portable',
      operationId: identity(6),
      attemptId: identity(9),
    },
    source: new Blob([Uint8Array.of(1)]),
    exactBytes: 1n,
    suggestedName: 'result.bin',
    objectUrlLeaseMilliseconds: BROWSER_HANDOFF_OBJECT_URL_LEASE_MS,
  }
}

function admission(intent: ReceiveIntent, exactArtifactBytes: bigint): PortableArtifactAdmission {
  return issuePortableArtifactAdmission({
    receiveIntentDigest: intent.digest,
    artifactDigest: intent.artifact.digest,
    sealedArtifact: {
      artifactKind: 'original-file',
      preparationManifestDigest: identity(11, 32),
    },
    exactArtifactBytes,
  })
}

function identity(seed: number, width = 16): string {
  const bytes = new Uint8Array(width)
  bytes[0] = seed
  bytes[bytes.length - 1] = seed ^ 0xff
  return encodeBase64Url(bytes)
}
