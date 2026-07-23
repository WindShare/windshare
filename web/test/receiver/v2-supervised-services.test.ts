import { describe, expect, it } from 'vitest'

import { byteRange, FileGeometry, type ByteRange } from '../../src/content/geometry'
import {
  type V2BlockRouteEligibility,
  type V2BlockSlice,
  V2BlockLaneAttemptsError,
  type V2LaneSet,
  type V2RouteAuthorizedBlockRangeReader,
} from '../../src/content/v2-broker'
import {
  type V2OpenedRevision,
  V2RevisionChangedDuringRecoveryError,
  type V2RevisionService,
} from '../../src/content/v2-session-services'
import type { V2FileRevisionDescriptor } from '../../src/content/v2-records'
import type {
  V2ConnectivityActivation,
  V2ContentIntent,
  V2ContentSizeClass,
  V2ReceiverConnectivity,
} from '../../src/connectivity/v2-receiver-policy'
import { V2ConnectivityRouteAuthority } from '../../src/connectivity/v2-receiver-policy'
import {
  type V2ContentGeneration,
  type V2ContentGenerationProvider,
  V2SupervisedContent,
} from '../../src/receiver/v2-supervised-content'
import { V2SupervisedConnectivity } from '../../src/receiver/v2-supervised-connectivity'

interface GenerationFixture {
  readonly generation: V2ContentGeneration
  readonly ranges: ByteRange[]
  readonly routeAuthorities: V2BlockRouteEligibility[]
  opens: number
  releases: number
}

class SwitchingGenerationProvider implements V2ContentGenerationProvider {
  current: V2ContentGeneration
  readonly initial: V2ContentGeneration
  readonly replacement: V2ContentGeneration
  recoveries = 0

  constructor(current: V2ContentGeneration, replacement: V2ContentGeneration) {
    this.current = current
    this.initial = current
    this.replacement = replacement
  }

  async execute<T>(
    signal: AbortSignal | undefined,
    operation: (generation: V2ContentGeneration) => Promise<T>,
  ): Promise<{ readonly generation: V2ContentGeneration; readonly value: T }> {
    signal?.throwIfAborted()
    const generation = this.current
    return { generation, value: await operation(generation) }
  }

  async recover(
    generation: V2ContentGeneration,
    _error: unknown,
    signal: AbortSignal | undefined,
  ): Promise<boolean> {
    signal?.throwIfAborted()
    if (generation !== this.current) return generation === this.initial
    this.recoveries += 1
    this.current = this.replacement
    return true
  }

  isCurrent(generation: V2ContentGeneration): boolean {
    return generation === this.current
  }

  contentLaneCount(routes: V2BlockRouteEligibility): number {
    return routes.active ? 1 : 0
  }
}

interface ConnectivityBeginCall {
  readonly intent: V2ContentIntent
  readonly sizeClass: V2ContentSizeClass
  readonly relayFallbackMilliseconds: number | undefined
  readonly routeAuthority: V2ConnectivityRouteAuthority | undefined
  readonly observed: V2ContentSizeClass[]
  delegateCloses: number
}

class FakeConnectivity {
  readonly begins: ConnectivityBeginCall[] = []
  closeCalls = 0

  begin(
    intent: V2ContentIntent,
    sizeClass: V2ContentSizeClass,
    options: {
      readonly relayFallbackMilliseconds?: number
      readonly routeAuthority?: V2ConnectivityRouteAuthority
    } = {},
  ): V2ConnectivityActivation {
    const call: ConnectivityBeginCall = {
      intent,
      sizeClass,
      relayFallbackMilliseconds: options.relayFallbackMilliseconds,
      routeAuthority: options.routeAuthority,
      observed: [],
      delegateCloses: 0,
    }
    this.begins.push(call)
    return {
      routes: options.routeAuthority ?? new V2ConnectivityRouteAuthority(),
      observeSizeClass: (observed) => { call.observed.push(observed) },
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

function revision(revisionSeed = 3): V2FileRevisionDescriptor {
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
  descriptor: V2FileRevisionDescriptor,
  serve: (range: ByteRange) => AsyncGenerator<V2BlockSlice>,
): GenerationFixture {
  const fixture = {
    opens: 0,
    releases: 0,
    ranges: [] as ByteRange[],
    routeAuthorities: [] as V2BlockRouteEligibility[],
  }
  const revisions = {
    open: async (): Promise<V2OpenedRevision> => {
      fixture.opens += 1
      const leaseId = identity(20 + id + fixture.opens)
      return {
        descriptor,
        leaseId,
        release: async () => { fixture.releases += 1 },
      }
    },
  }
  const broker: V2RouteAuthorizedBlockRangeReader = {
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
    revisions: revisions as unknown as V2RevisionService,
    broker,
    lanes: { size: 1 } as unknown as V2LaneSet,
  }
  return Object.assign(fixture, { generation })
}

async function collect(reader: AsyncGenerator<V2BlockSlice>): Promise<V2BlockSlice[]> {
  const slices: V2BlockSlice[] = []
  for await (const slice of reader) slices.push(slice)
  return slices
}

describe('v2 supervised content generations', () => {
  it('resumes at the first byte after the consumer has durably checkpointed a slice', async () => {
    const stable = revision()
    const loss = new V2BlockLaneAttemptsError([new Error('all generation-one lanes left')])
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
    const content = new V2SupervisedContent(
      provider,
      (length) => new Uint8Array(length).fill(8),
    )
    const routes = new V2ConnectivityRouteAuthority()
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

  it('keeps the existing lease when only physical lanes change in one ProtocolSession', async () => {
    const stable = revision()
    let attempts = 0
    const generation = generationFixture(1, stable, async function* (range) {
      attempts += 1
      if (attempts === 1) {
        throw new V2BlockLaneAttemptsError([new Error('content lane changed')])
      }
      yield { offset: range.start, data: Uint8Array.of(1, 2, 3, 4) }
    })
    const provider = new SwitchingGenerationProvider(generation.generation, generation.generation)
    const content = new V2SupervisedContent(
      provider,
      (length) => new Uint8Array(length).fill(8),
    )
    const scoped = content.forRoutes(new V2ConnectivityRouteAuthority())
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
    const loss = new V2BlockLaneAttemptsError([new Error('generation lost')])
    const first = generationFixture(1, stable, async function* () {
      yield* ([] as V2BlockSlice[])
      throw loss
    })
    const second = generationFixture(2, stable, async function* (range) {
      const data = range.start === 0n ? Uint8Array.of(1, 2) : Uint8Array.of(3, 4)
      yield { offset: range.start, data }
    })
    const provider = new SwitchingGenerationProvider(first.generation, second.generation)
    const content = new V2SupervisedContent(
      provider,
      (length) => new Uint8Array(length).fill(8),
    )
    const scoped = content.forRoutes(new V2ConnectivityRouteAuthority())
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
    const content = new V2SupervisedContent(
      provider,
      (length) => new Uint8Array(length).fill(8),
    )
    const routes = new V2ConnectivityRouteAuthority()
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
      yield* ([] as V2BlockSlice[])
      throw new V2BlockLaneAttemptsError([new Error('generation lost')])
    })
    const second = generationFixture(2, changed, async function* () {
      yield* ([] as V2BlockSlice[])
      throw new Error('Changed revision content must never be read')
    })
    const provider = new SwitchingGenerationProvider(first.generation, second.generation)
    const content = new V2SupervisedContent(
      provider,
      (length) => new Uint8Array(length).fill(8),
    )
    const scoped = content.forRoutes(new V2ConnectivityRouteAuthority())
    const opened = await scoped.revisions.open(original.fileId)

    await expect(collect(scoped.broker.readRange(
      opened.descriptor,
      opened.leaseId,
      byteRange(0n, 4n),
    ))).rejects.toBeInstanceOf(V2RevisionChangedDuringRecoveryError)

    expect(provider.recoveries).toBe(1)
    expect(second.opens).toBe(1)
    expect(second.ranges).toHaveLength(0)
    expect(second.releases).toBe(1)
    content.close()
  })
})

describe('v2 supervised connectivity activation clock', () => {
  it('preserves the original zero/eight-second window across generation binds', async () => {
    let now = 0
    const supervised = new V2SupervisedConnectivity(() => now)
    const first = new FakeConnectivity()
    const second = new FakeConnectivity()
    const third = new FakeConnectivity()
    supervised.bind(first as unknown as V2ReceiverConnectivity)

    const activation = supervised.begin('preview')
    expect(first.begins[0]?.relayFallbackMilliseconds).toBe(8_000)
    expect(first.begins[0]?.routeAuthority).toBe(activation.routes)

    now = 5_000
    supervised.bind(second as unknown as V2ReceiverConnectivity)
    expect(first.begins[0]?.delegateCloses).toBe(1)
    expect(second.begins[0]?.relayFallbackMilliseconds).toBe(3_000)
    expect(second.begins[0]?.routeAuthority).toBe(activation.routes)
    expect(activation.routes.active).toBe(true)

    activation.observeSizeClass('large')
    now = 8_000
    supervised.bind(third as unknown as V2ReceiverConnectivity)
    expect(second.begins[0]?.delegateCloses).toBe(1)
    expect(third.begins[0]).toMatchObject({
      intent: 'preview',
      sizeClass: 'large',
      relayFallbackMilliseconds: 0,
    })

    activation.close()
    expect(activation.routes.active).toBe(false)
    expect(third.begins[0]?.delegateCloses).toBe(1)
    await supervised.close()
    expect(third.closeCalls).toBe(1)
  })
})
