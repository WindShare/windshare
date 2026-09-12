import { describe, expect, it } from 'vitest'
import { byteRange, FileGeometry, type ByteRange } from '../../src/content/geometry'
import type { V2BlockSlice, V2LaneSet } from '../../src/content/v2-broker'
import { V2ConnectivityRouteAuthority } from '../../src/connectivity/v2-receiver-policy'
import type { V2FileRevisionDescriptor } from '../../src/content/v2-records'
import {
  V2RemoteOperationError,
  V2RevisionChangedDuringRecoveryError,
  V2RevisionLeaseExpiredError,
  type V2RevisionService,
} from '../../src/content/v2-session-services'
import {
  V2SupervisedContent,
  type V2ContentGeneration,
  type V2ContentGenerationProvider,
} from '../../src/receiver/v2-supervised-content'
import { createReceivedProtocolError } from '../../src/diagnostics/incident/fact'
import { createV2ProtocolOperationIdentity, createV2ProtocolSessionIdentity } from '../../src/session/v2-identities'
import { V2_REVISION_CODE_LEASE_EXPIRED } from '../../src/content/v2-flow'

function id(n: number): Uint8Array<ArrayBuffer> { return new Uint8Array(16).fill(n) }
function descriptor(): V2FileRevisionDescriptor {
  return {
    shareInstance: id(1), shareInstanceId: 'share', fileId: id(2), fileIdText: 'file',
    fileRevision: id(3), fileRevisionText: 'revision', exactSize: 4n, geometry: new FileGeometry(4n, 4n),
  }
}
function remoteFailure(code = V2_REVISION_CODE_LEASE_EXPIRED): V2RemoteOperationError {
  return new V2RemoteOperationError(createReceivedProtocolError({
    requestKind: 'renew_lease', correlation: {
      protocolSessionId: createV2ProtocolSessionIdentity(id(10)),
      protocolOperationId: createV2ProtocolOperationIdentity(id(11))
    }, content: {
      scope: 'revision', code: code, retryable: false
    }
  }))
}
function deferred() {
  let resolve!: () => void
  const promise = new Promise<void>(done => { resolve = done })
  return { promise, resolve }
}
function fixture(options: {
  serve: (lease: number, range: ByteRange) => AsyncGenerator<V2BlockSlice>
  open?: (count: number, signal?: AbortSignal) => Promise<V2FileRevisionDescriptor>
  release?: (lease: number) => Promise<void>
}) {
  const stable = descriptor()
  let opens = 0
  let recoveries = 0
  const events: string[] = []
  const generation: V2ContentGeneration = {
    id: 1,
    revisions: {
      open: async (_file: Uint8Array, _routes: unknown, signal?: AbortSignal) => {
        const lease = ++opens
        events.push('open:' + lease)
        const next = await options.open?.(lease, signal) ?? stable
        return {
          descriptor: next, leaseId: id(lease),
          release: async () => { events.push('release:' + lease); await options.release?.(lease) },
        }
      },
    } as unknown as V2RevisionService,
    broker: {
      readRouteAuthorizedRange: (_descriptor, lease, range) => options.serve(lease[0]!, range),
    },
    lanes: { size: 1 } as V2LaneSet,
  }
  const provider: V2ContentGenerationProvider = {
    execute: async (signal, operation) => {
      signal?.throwIfAborted()
      return { generation, value: await operation(generation) }
    },
    recover: async () => { recoveries++; return false },
    isCurrent: value => value === generation,
    contentLaneCount: () => 1,
  }
  const content = new V2SupervisedContent(provider, () => id(99))
  const scoped = content.forRoutes(new V2ConnectivityRouteAuthority())
  return { stable, content, scoped, events, opens: () => opens, recoveries: () => recoveries }
}
async function collect(source: AsyncGenerator<V2BlockSlice>) {
  const result: V2BlockSlice[] = []
  for await (const value of source) result.push(value)
  return result
}

describe('lease replacement within a healthy protocol generation', () => {
  it.each([new V2RevisionLeaseExpiredError(), remoteFailure(), remoteFailure(0x3008)])('continues after %s without repeating delivered bytes', async expiry => {
    const ranges: ByteRange[] = []
    const f = fixture({ serve: async function* (lease, range) {
      ranges.push(range)
      if (lease === 1) { yield { offset: 0n, data: Uint8Array.of(1, 2) }; throw expiry }
      yield { offset: 2n, data: Uint8Array.of(3, 4) }
    } })
    const opened = await f.scoped.revisions.open(f.stable.fileId)
    const slices = await collect(f.scoped.broker.readRange(opened.descriptor, opened.leaseId, byteRange(0n, 4n)))
    expect(slices.map(slice => [...slice.data])).toEqual([[1, 2], [3, 4]])
    expect(ranges).toEqual([byteRange(0n, 4n), byteRange(2n, 4n)])
    expect(f.opens()).toBe(2)
    expect(f.recoveries()).toBe(0)
    expect(f.events).toEqual(['open:1', 'open:2', 'release:1'])
    await opened.release()
    expect(f.events.at(-1)).toBe('release:2')
    f.content.close()
  })

  it('shares one replacement between concurrent readers', async () => {
    const gate = deferred()
    let failedReaders = 0
    const f = fixture({ serve: async function* (lease) {
      if (lease === 1) {
        yield { offset: 0n, data: Uint8Array.of(1, 2) }
        if (++failedReaders === 2) gate.resolve()
        await gate.promise
        throw remoteFailure()
      }
      yield { offset: 2n, data: Uint8Array.of(3, 4) }
    } })
    const opened = await f.scoped.revisions.open(f.stable.fileId)
    const read = () => collect(f.scoped.broker.readRange(opened.descriptor, opened.leaseId, byteRange(0n, 4n)))
    const values = await Promise.all([read(), read()])
    expect(values.every(slices => slices.length === 2)).toBe(true)
    expect(f.opens()).toBe(2)
    await opened.release()
    expect(f.events.filter(value => value.startsWith('release'))).toEqual(['release:1', 'release:2'])
    f.content.close()
  })

  it.each(['revision', 'geometry', 'modified time'])('rejects a changed %s before reading replacement bytes', async field => {
    const f = fixture({
      serve: async function* () { yield await Promise.reject(remoteFailure()) },
      open: async count => {
        const value = descriptor()
        if (count === 1) return value
        if (field === 'revision') return { ...value, fileRevision: id(42) }
        if (field === 'geometry') return { ...value, geometry: new FileGeometry(4n, 2n) }
        return { ...value, modifiedTime: { seconds: 1n, nanoseconds: 0, precision: 1, milliseconds: 1000n } }
      },
    })
    const opened = await f.scoped.revisions.open(f.stable.fileId)
    await expect(collect(f.scoped.broker.readRange(opened.descriptor, opened.leaseId, byteRange(0n, 4n))))
      .rejects.toBeInstanceOf(V2RevisionChangedDuringRecoveryError)
    await opened.release()
    expect(f.events).toContain('release:2')
    expect(f.events).toContain('release:1')
    f.content.close()
  })

  it('keeps old-lease retirement from blocking useful replacement reads', async () => {
    const retirement = deferred()
    const f = fixture({
      serve: async function* (lease) {
        if (lease === 1) throw remoteFailure()
        yield { offset: 0n, data: new Uint8Array(4) }
      },
      release: async lease => { if (lease === 1) await retirement.promise },
    })
    const opened = await f.scoped.revisions.open(f.stable.fileId)
    expect(await collect(f.scoped.broker.readRange(opened.descriptor, opened.leaseId, byteRange(0n, 4n)))).toHaveLength(1)
    let released = false
    const release = opened.release().then(() => { released = true })
    await Promise.resolve()
    expect(released).toBe(false)
    retirement.resolve()
    await release
    f.content.close()
  })

  it('joins prior retirement even when releasing the current lease fails', async () => {
    const retirement = deferred()
    const failure = new Error('current lease release failed')
    const f = fixture({
      serve: async function* (lease) {
        if (lease === 1) throw remoteFailure()
        yield { offset: 0n, data: new Uint8Array(4) }
      },
      release: async lease => {
        if (lease === 1) await retirement.promise
        else throw failure
      },
    })
    const opened = await f.scoped.revisions.open(f.stable.fileId)
    await collect(f.scoped.broker.readRange(opened.descriptor, opened.leaseId, byteRange(0n, 4n)))
    let settled = false
    const result = opened.release().catch((error: unknown) => { settled = true; return error })
    await new Promise(resolve => setTimeout(resolve))
    expect(settled).toBe(false)
    retirement.resolve()
    expect(await result).toBe(failure)
    f.content.close()
  })

  it('bounds expired fresh leases and does not reinterpret another rejection as expiry', async () => {
    for (const failure of [remoteFailure(), remoteFailure(0x3007), remoteFailure(0x3008)]) {
      const f = fixture({ serve: async function* () { yield await Promise.reject(failure) } })
      const opened = await f.scoped.revisions.open(f.stable.fileId)
      await expect(collect(f.scoped.broker.readRange(opened.descriptor, opened.leaseId, byteRange(0n, 4n)))).rejects.toBe(failure)
      expect(f.opens()).toBe(failure.code === 0x3007 ? 1 : 2)
      await opened.release()
      f.content.close()
    }
  })

  it('lets a canceled replacement waiter leave without canceling its sibling', async () => {
    const opening = deferred()
    const resume = deferred()
    const f = fixture({
      serve: async function* (lease) {
        if (lease === 1) throw remoteFailure()
        yield { offset: 0n, data: new Uint8Array(4) }
      },
      open: async count => {
        if (count > 1) { opening.resolve(); await resume.promise }
        return descriptor()
      },
    })
    const opened = await f.scoped.revisions.open(f.stable.fileId)
    const controller = new AbortController()
    const first = collect(f.scoped.broker.readRange(opened.descriptor, opened.leaseId, byteRange(0n, 4n), { signal: controller.signal }))
    const rejected = expect(first).rejects.toMatchObject({ name: 'AbortError' })
    await opening.promise
    const second = collect(f.scoped.broker.readRange(opened.descriptor, opened.leaseId, byteRange(0n, 4n)))
    controller.abort()
    await rejected
    resume.resolve()
    expect(await second).toHaveLength(1)
    expect(f.opens()).toBe(2)
    await opened.release()
    f.content.close()
  })
})
