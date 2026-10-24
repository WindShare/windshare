import { describe, expect, it } from 'vitest'
import { DownloadMetrics } from '../../src/receiver/download-metrics'

import { createReceivedProtocolError } from '../../src/diagnostics/incident/fact'
import { byteRange, FileGeometry, type ByteRange } from '../../src/content/geometry'
import {
  type BlockRouteEligibility,
  type BlockSlice,
  BlockLaneAttemptsError,
  type LaneSet,
  type RouteAuthorizedBlockRangeReader,
} from '../../src/content/broker'
import { REVISION_CODE_QUOTA } from '../../src/content/flow'
import {
  type OpenedRevision,
  RevisionCapacityBusyError,
  RevisionChangedDuringRecoveryError,
  type RevisionService,
} from '../../src/content/session-services'
import type { FileRevisionDescriptor } from '../../src/content/records'
import type {
  ConnectivityActivation,
  ContentIntent,
  ReceiverConnectivity,
} from '../../src/connectivity/receiver-policy'
import { ConnectivityRouteAuthority } from '../../src/connectivity/receiver-policy'
import {
  type ContentGeneration,
  type ContentGenerationProvider,
  SupervisedContent,
} from '../../src/receiver/supervised-content'
import { SupervisedConnectivity } from '../../src/receiver/supervised-connectivity'
import {
  createProtocolOperationIdentity,
  createProtocolSessionIdentity,
} from '../../src/session/identities'

interface GenerationFixture {
  readonly generation: ContentGeneration
  readonly ranges: ByteRange[]
  readonly routeAuthorities: BlockRouteEligibility[]
  opens: number
  releases: number
}

class SwitchingGenerationProvider implements ContentGenerationProvider {
  current: ContentGeneration
  readonly initial: ContentGeneration
  readonly replacement: ContentGeneration
  recoveries = 0

  constructor(current: ContentGeneration, replacement: ContentGeneration) {
    this.current = current
    this.initial = current
    this.replacement = replacement
  }

  async execute<T>(
    signal: AbortSignal | undefined,
    operation: (generation: ContentGeneration) => Promise<T>,
  ): Promise<{ readonly generation: ContentGeneration; readonly value: T }> {
    signal?.throwIfAborted()
    const generation = this.current
    return { generation, value: await operation(generation) }
  }

  async recover(
    generation: ContentGeneration,
    _error: unknown,
    signal: AbortSignal | undefined,
  ): Promise<boolean> {
    signal?.throwIfAborted()
    if (generation !== this.current) return generation === this.initial
    this.recoveries += 1
    this.current = this.replacement
    return true
  }

  isCurrent(generation: ContentGeneration): boolean {
    return generation === this.current
  }

  contentLaneCount(routes: BlockRouteEligibility): number {
    return routes.active ? 1 : 0
  }
}

interface ConnectivityBeginCall {
  readonly intent: ContentIntent
  readonly routeAuthority: ConnectivityRouteAuthority | undefined
  delegateCloses: number
}

class FakeConnectivity {
  readonly begins: ConnectivityBeginCall[] = []
  closeCalls = 0

  begin(
    intent: ContentIntent,
    options: {
      readonly routeAuthority?: ConnectivityRouteAuthority
    } = {},
  ): ConnectivityActivation {
    const call: ConnectivityBeginCall = {
      intent,
      routeAuthority: options.routeAuthority,
      delegateCloses: 0,
    }
    this.begins.push(call)
    return {
      routes: options.routeAuthority ?? new ConnectivityRouteAuthority(),
      close: () => { call.delegateCloses += 1 },
    }
  }

  async close(): Promise<void> {
    this.closeCalls += 1
  }
}

function identity(first: number): Uint8Array<ArrayBuffer> {
  const value = new Uint8Array(16)
  value[0] = first
  return value
}

function capacityBusyError(retryAfterMilliseconds = 400): RevisionCapacityBusyError {
  const failure = Object.freeze({
    code: REVISION_CODE_QUOTA,
    retryable: true,
    retryAfterMilliseconds,
  })
  const protocolFailure = createReceivedProtocolError({
    requestKind: 'open_revisions', correlation: Object.freeze({
      protocolSessionId: createProtocolSessionIdentity(identity(30)),
      protocolOperationId: createProtocolOperationIdentity(identity(31)),
    }), content: {
      scope: 'revision', code: failure.code, retryable: failure.retryable, retryAfterMilliseconds: failure.retryAfterMilliseconds
    }
  })
  return new RevisionCapacityBusyError(protocolFailure)
}

function revision(revisionSeed = 3): FileRevisionDescriptor {
  return Object.freeze({
    shareInstance: identity(1),
    shareInstanceId: 'share',
    fileId: identity(2),
    fileIdText: 'file',
    fileRevision: identity(revisionSeed),
    fileRevisionText: `revision-${revisionSeed}`,
    exactSize: 4n,
    geometry: new FileGeometry(4n, 4n),
  })
}

function generationFixture(
  id: number,
  descriptor: FileRevisionDescriptor,
  serve: (range: ByteRange) => AsyncGenerator<BlockSlice>,
  openRevision?: () => Promise<OpenedRevision>,
): GenerationFixture {
  const fixture = {
    opens: 0,
    releases: 0,
    ranges: [] as ByteRange[],
    routeAuthorities: [] as BlockRouteEligibility[],
  }
  const revisions = {
    open: async (): Promise<OpenedRevision> => {
      fixture.opens += 1
      if (openRevision !== undefined) return openRevision()
      const leaseId = identity(20 + id + fixture.opens)
      return {
        descriptor,
        leaseId,
        release: async () => { fixture.releases += 1 },
      }
    },
  }
  const broker: RouteAuthorizedBlockRangeReader = {
    readRouteAuthorizedRange: (
      _descriptor,
      _leaseId,
      range,
      options,
    ) => {
      fixture.ranges.push(range)
      fixture.routeAuthorities.push(options.routes)
      return serve(range)
    },
  }
  const generation = {
    id,
    revisions: revisions as unknown as RevisionService,
    broker,
    lanes: { size: 1 } as unknown as LaneSet,
  }
  return Object.assign(fixture, { generation })
}

async function collect(reader: AsyncGenerator<BlockSlice>): Promise<BlockSlice[]> {
  const slices: BlockSlice[] = []
  for await (const slice of reader) slices.push(slice)
  return slices
}

describe('v2 supervised content generations', () => {
  it('attributes delivered slices across generations while output suspension never becomes stall', async () => {
    let now = 0
    const metrics = new DownloadMetrics('download', true, () => now)
    const stable = revision()
    const loss = new BlockLaneAttemptsError([new Error('lost direct')])
    const first = generationFixture(1, stable, async function* () {
      yield { offset: 0n, data: Uint8Array.of(1, 2), authenticatedRoute: 'direct' }
      now += 30
      throw loss
    })
    const second = generationFixture(2, stable, async function* () {
      now += 20
      yield { offset: 2n, data: Uint8Array.of(3, 4), authenticatedRoute: 'turn' }
    })
    const provider = Object.assign(new SwitchingGenerationProvider(first.generation, second.generation), {
      downloadMetrics: () => metrics,
    })
    const content = new SupervisedContent(provider, length => new Uint8Array(length).fill(9))
    const routes = new ConnectivityRouteAuthority()
    const scoped = content.forRoutes(routes)
    const opened = await scoped.revisions.open(stable.fileId)
    const reader = scoped.broker.readRange(opened.descriptor, opened.leaseId, byteRange(0n, 4n))
    await reader.next()
    metrics.availability(false)
    now += 60_000 // The generator is suspended in the output consumer.
    await reader.next()
    await reader.next()
    expect(scoped.lanes.downloadConnectivity?.(true)).toMatchObject({
      direct_bytes: '2', turn_bytes: '2', direct_fraction: 0.5,
      fallback_stall_ms: 50, first_direct_elapsed_ms: 0, final: true, incomplete: false,
    })
    await opened.release()
    content.close()
  })

  it('resumes at the first byte after the consumer has durably checkpointed a slice', async () => {
    const stable = revision()
    const loss = new BlockLaneAttemptsError([new Error('all generation-one lanes left')])
    const first = generationFixture(1, stable, async function* (range) {
      expect(range).toEqual(byteRange(0n, 4n))
      yield { offset: 0n, data: Uint8Array.of(1, 2) }
      throw loss
    })
    const second = generationFixture(2, stable, async function* (range) {
      expect(range).toEqual(byteRange(2n, 4n))
      yield { offset: 2n, data: Uint8Array.of(3, 4) }
    })
    const provider = new SwitchingGenerationProvider(first.generation, second.generation)
    const content = new SupervisedContent(
      provider,
      (length) => new Uint8Array(length).fill(8),
    )
    const routes = new ConnectivityRouteAuthority()
    const scoped = content.forRoutes(routes)
    const opened = await scoped.revisions.open(stable.fileId)

    const reader = scoped.broker.readRange(
      opened.descriptor,
      opened.leaseId,
      byteRange(0n, 4n),
    )
    const firstSlice = await reader.next()
    expect(firstSlice).toMatchObject({ done: false, value: { offset: 0n } })
    const checkpoints = [firstSlice.value?.data.byteLength]
    const secondSlice = await reader.next()
    expect(secondSlice).toMatchObject({ done: false, value: { offset: 2n } })
    checkpoints.push(secondSlice.value?.data.byteLength)
    expect(await reader.next()).toMatchObject({ done: true })

    expect(checkpoints).toEqual([2, 2])
    expect(first.ranges).toEqual([byteRange(0n, 4n)])
    expect(second.ranges).toEqual([byteRange(2n, 4n)])
    expect(first.routeAuthorities).toEqual([routes])
    expect(second.routeAuthorities).toEqual([routes])
    expect(provider.recoveries).toBe(1)
    expect(first.opens).toBe(1)
    expect(second.opens).toBe(1)

    await opened.release()
    expect(first.releases).toBe(0)
    expect(second.releases).toBe(1)
    content.close()
  })

  it('preserves capacity authority when a mid-range generation reopen is busy', async () => {
    const stable = revision()
    const capacity = capacityBusyError(425)
    const first = generationFixture(1, stable, async function* (range) {
      expect(range).toEqual(byteRange(0n, 4n))
      yield { offset: 0n, data: Uint8Array.of(1, 2) }
      throw new BlockLaneAttemptsError([new Error('generation lost after partial delivery')])
    })
    const second = generationFixture(
      2,
      stable,
      async function* () {
        yield* ([] as BlockSlice[])
      },
      async () => Promise.reject(capacity),
    )
    const provider = new SwitchingGenerationProvider(first.generation, second.generation)
    const content = new SupervisedContent(
      provider,
      (length) => new Uint8Array(length).fill(8),
    )
    const scoped = content.forRoutes(new ConnectivityRouteAuthority())
    const opened = await scoped.revisions.open(stable.fileId)
    const reader = scoped.broker.readRange(
      opened.descriptor,
      opened.leaseId,
      byteRange(0n, 4n),
    )

    await expect(reader.next()).resolves.toMatchObject({
      done: false,
      value: { offset: 0n, data: Uint8Array.of(1, 2) },
    })
    await expect(reader.next()).rejects.toBe(capacity)
    expect(capacity.retryAfterMilliseconds).toBe(425)
    expect(provider.recoveries).toBe(1)
    expect(first.opens).toBe(1)
    expect(second.opens).toBe(1)
    expect(second.ranges).toHaveLength(0)
    expect(first.releases).toBe(0)
    expect(second.releases).toBe(0)

    await opened.release()
    content.close()
  })

  it('keeps the existing lease when only physical lanes change in one ProtocolSession', async () => {
    const stable = revision()
    let attempts = 0
    const generation = generationFixture(1, stable, async function* (range) {
      attempts += 1
      if (attempts === 1) {
        throw new BlockLaneAttemptsError([new Error('content lane changed')])
      }
      yield { offset: range.start, data: Uint8Array.of(1, 2, 3, 4) }
    })
    const provider = new SwitchingGenerationProvider(generation.generation, generation.generation)
    const content = new SupervisedContent(
      provider,
      (length) => new Uint8Array(length).fill(8),
    )
    const scoped = content.forRoutes(new ConnectivityRouteAuthority())
    const opened = await scoped.revisions.open(stable.fileId)

    const slices = await collect(scoped.broker.readRange(
      opened.descriptor,
      opened.leaseId,
      byteRange(0n, 4n),
    ))

    expect(slices).toHaveLength(1)
    expect(generation.opens).toBe(1)
    expect(provider.recoveries).toBe(1)
    await opened.release()
    expect(generation.releases).toBe(1)
    content.close()
  })

  it('singleflights revision reopening for concurrent preview and download recovery', async () => {
    const stable = revision()
    const loss = new BlockLaneAttemptsError([new Error('generation lost')])
    const first = generationFixture(1, stable, async function* () {
      yield* ([] as BlockSlice[])
      throw loss
    })
    const second = generationFixture(2, stable, async function* (range) {
      const data = range.start === 0n ? Uint8Array.of(1, 2) : Uint8Array.of(3, 4)
      yield { offset: range.start, data }
    })
    const provider = new SwitchingGenerationProvider(first.generation, second.generation)
    const content = new SupervisedContent(
      provider,
      (length) => new Uint8Array(length).fill(8),
    )
    const scoped = content.forRoutes(new ConnectivityRouteAuthority())
    const opened = await scoped.revisions.open(stable.fileId)

    const [preview, download] = await Promise.all([
      collect(scoped.broker.readRange(
        opened.descriptor,
        opened.leaseId,
        byteRange(0n, 2n),
        { priority: 'preview' },
      )),
      collect(scoped.broker.readRange(
        opened.descriptor,
        opened.leaseId,
        byteRange(2n, 4n),
        { priority: 'download' },
      )),
    ])

    expect(preview[0]?.data).toEqual(Uint8Array.of(1, 2))
    expect(download[0]?.data).toEqual(Uint8Array.of(3, 4))
    expect(second.opens).toBe(1)
    await opened.release()
    expect(second.releases).toBe(1)
    content.close()
  })

  it('releases an opened revision when its content activation ends', async () => {
    const stable = revision()
    const fixture = generationFixture(1, stable, async function* (range) {
      yield { offset: range.start, data: Uint8Array.of(1, 2, 3, 4) }
    })
    const provider = new SwitchingGenerationProvider(fixture.generation, fixture.generation)
    const content = new SupervisedContent(
      provider,
      (length) => new Uint8Array(length).fill(8),
    )
    const routes = new ConnectivityRouteAuthority()
    const scoped = content.forRoutes(routes)
    const opened = await scoped.revisions.open(stable.fileId)

    routes.close()
    for (let turn = 0; turn < 4; turn += 1) await Promise.resolve()
    expect(fixture.releases).toBe(1)
    await expect(collect(scoped.broker.readRange(
      opened.descriptor,
      opened.leaseId,
      byteRange(0n, 4n),
    ))).rejects.toMatchObject({ name: 'AbortError' })
    content.close()
  })

  it('rejects a revision identity change instead of splicing generations', async () => {
    const original = revision(3)
    const changed = revision(4)
    const first = generationFixture(1, original, async function* () {
      yield* ([] as BlockSlice[])
      throw new BlockLaneAttemptsError([new Error('generation lost')])
    })
    const second = generationFixture(2, changed, async function* () {
      yield* ([] as BlockSlice[])
      throw new Error('Changed revision content must never be read')
    })
    const provider = new SwitchingGenerationProvider(first.generation, second.generation)
    const content = new SupervisedContent(
      provider,
      (length) => new Uint8Array(length).fill(8),
    )
    const scoped = content.forRoutes(new ConnectivityRouteAuthority())
    const opened = await scoped.revisions.open(original.fileId)

    await expect(collect(scoped.broker.readRange(
      opened.descriptor,
      opened.leaseId,
      byteRange(0n, 4n),
    ))).rejects.toBeInstanceOf(RevisionChangedDuringRecoveryError)

    expect(provider.recoveries).toBe(1)
    expect(second.opens).toBe(1)
    expect(second.ranges).toHaveLength(0)
    expect(second.releases).toBe(1)
    content.close()
  })
})

describe('v2 supervised connectivity activation ownership', () => {
  it('preserves one route authority while replacing generation delegates', async () => {
    const supervised = new SupervisedConnectivity()
    const first = new FakeConnectivity()
    const second = new FakeConnectivity()
    const third = new FakeConnectivity()
    supervised.bind(first as unknown as ReceiverConnectivity)

    const activation = supervised.begin('preview')
    expect(first.begins[0]?.routeAuthority).toBe(activation.routes)
    expect(activation.routes.allows('application-relay')).toBe(true)
    expect(activation.routes.allows('direct')).toBe(true)

    supervised.bind(second as unknown as ReceiverConnectivity)
    expect(first.begins[0]?.delegateCloses).toBe(1)
    expect(second.begins[0]?.routeAuthority).toBe(activation.routes)
    expect(activation.routes.active).toBe(true)

    supervised.bind(third as unknown as ReceiverConnectivity)
    expect(second.begins[0]?.delegateCloses).toBe(1)
    expect(third.begins[0]).toMatchObject({
      intent: 'preview',
      routeAuthority: activation.routes,
    })

    activation.close()
    expect(activation.routes.active).toBe(false)
    expect(third.begins[0]?.delegateCloses).toBe(1)
    await supervised.close()
    expect(third.closeCalls).toBe(1)
  })
})
