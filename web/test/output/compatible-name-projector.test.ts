import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import { compatibleNameMappingV1, type CompatibleNameMappingV1 } from '../../src/output/file-system-access/compatible-name/model'
import {
  createCompatibleNameProjector,
  type CompatibleNameOwnedSidecarWriter,
  type CompatibleNameProjectorLedger,
} from '../../src/output/file-system-access/compatible-name/projector'
import {
  decodeCompatibleNameSidecar,
  encodeCompatibleNameSidecarFooter,
  encodeCompatibleNameSidecarHeader,
  encodeCompatibleNameSidecarMapping,
} from '../../src/output/file-system-access/compatible-name/sidecar-codec'

const TEXT_ENCODER = new TextEncoder()
const TEXT_DECODER = new TextDecoder()
const OPERATION_ID = identity(16, 1)
const HEADER = encodeCompatibleNameSidecarHeader({
  operationId: OPERATION_ID,
  placement: 'inside',
})

describe('compatible-name sidecar projector', () => {
  it('repairs a torn tail and resumes strictly after the last closed footer', async () => {
    const first = committedMapping(1, ['root'])
    const second = committedMapping(2, ['root', 'second.txt'])
    const closedPrefix = sidecar([first], 'active')
    const tornTail = encodeCompatibleNameSidecarMapping(projectedMapping(second)).slice(0, -3)
    const writer = new MemoryOwnedSidecarWriter(bytes(closedPrefix + tornTail))
    const ledger = new MemoryProjectorLedger([first, second])

    const projector = await createCompatibleNameProjector(projectorOptions(ledger, writer))

    expect(projector.observeFooter()).toEqual({ committedCount: 2, state: 'active' })
    expect(ledger.cursors).toEqual([1])
    expect(writer.mutations.map(mutation => mutation.kind)).toEqual(['truncate', 'append'])
    expect(decodeCompatibleNameSidecar(writer.bytes).mappings.map(mapping => mapping.ordinal))
      .toEqual([1, 2])
    expect(writer.maxConcurrentMutations).toBe(1)
  })

  it('rebuilds an unreadable tail only through the ownership-verifying writer', async () => {
    const mappings = [
      committedMapping(1, ['root']),
      committedMapping(2, ['root', 'second.txt']),
    ]
    const writer = new MemoryOwnedSidecarWriter(Uint8Array.of(0xff))
    const ledger = new MemoryProjectorLedger(mappings)

    const projector = await createCompatibleNameProjector(projectorOptions(ledger, writer))
    const checkpoint = decodeCompatibleNameSidecar(writer.bytes)

    expect(projector.observeFooter()).toEqual({ committedCount: 2, state: 'active' })
    expect(ledger.cursors).toEqual([0])
    expect(writer.mutations).toMatchObject([{ kind: 'replace', ownershipVerified: true }])
    expect(checkpoint.mappings).toEqual(mappings.map(projectedMapping))
    expect(checkpoint.trailingByteLength).toBe(0)
  })

  it('coalesces repeated dirty marks into one running batch and at most one repeat', async () => {
    const writer = new MemoryOwnedSidecarWriter(bytes(sidecar([], 'active')))
    const ledger = new MemoryProjectorLedger()
    const caughtUp = deferred<void>()
    const projector = await createCompatibleNameProjector({
      ...projectorOptions(ledger, writer),
      trace: event => {
        if (event.decision === 'batch-appended' && event.committedCount === 3) caughtUp.resolve()
      },
    })

    ledger.committed.push(committedMapping(1, ['first.txt']))
    const firstAppend = writer.blockNextAppend()
    expect(projector.markDirty()).toBeUndefined()
    await firstAppend.started

    ledger.committed.push(
      committedMapping(2, ['second.txt']),
      committedMapping(3, ['third.txt']),
    )
    for (let notification = 0; notification < 32; notification += 1) projector.markDirty()
    firstAppend.release()
    await writer.waitForCompletedAppendCount(2)
    await caughtUp.promise

    expect(writer.appendPayloads).toHaveLength(2)
    expect(mappingLines(writer.appendPayloads[0])).toHaveLength(1)
    expect(mappingLines(writer.appendPayloads[1])).toHaveLength(2)
    expect(ledger.cursors).toEqual([0, 0, 1])
    expect(projector.observeFooter()).toEqual({ committedCount: 3, state: 'active' })
    expect(writer.maxConcurrentMutations).toBe(1)
  })

  it('keeps pause observation non-draining and performs terminal catch-up exactly once', async () => {
    const writer = new MemoryOwnedSidecarWriter(bytes(sidecar([], 'active')))
    const ledger = new MemoryProjectorLedger()
    const projector = await createCompatibleNameProjector(projectorOptions(ledger, writer))
    ledger.committed.push(
      committedMapping(1, ['first.txt']),
      committedMapping(2, ['second.txt']),
    )

    // Pause uses observation only: it neither waits for nor creates a projector flush.
    expect(projector.observeFooter()).toEqual({ committedCount: 0, state: 'active' })
    expect(writer.mutations).toHaveLength(0)

    const firstDrain = projector.drainTerminal('stopped')
    const repeatedDrain = projector.drainTerminal('stopped')
    expect(repeatedDrain).toBe(firstDrain)
    await expect(firstDrain).resolves.toEqual({ committedCount: 2, state: 'stopped' })

    expect(writer.appendPayloads).toHaveLength(1)
    expect(mappingLines(writer.appendPayloads[0])).toHaveLength(2)
    expect(writer.appendPayloads[0]).toMatch(/F\t2\tstopped\n$/u)
    expect(ledger.cursors).toEqual([0, 0])
    await expect(projector.drainTerminal('completed')).rejects.toThrow('different outcome')
    expect(writer.appendPayloads).toHaveLength(1)
  })

  it('waits for a running active write, repairs its tail, and makes the terminal footer last', async () => {
    const writer = new MemoryOwnedSidecarWriter(bytes(sidecar([], 'active')))
    const ledger = new MemoryProjectorLedger()
    const projector = await createCompatibleNameProjector(projectorOptions(ledger, writer))
    ledger.committed.push(committedMapping(1, ['first.txt']))
    const activeAppend = writer.blockNextAppend()
    projector.markDirty()
    await activeAppend.started

    ledger.committed.push(committedMapping(2, ['second.txt']))
    const terminal = projector.drainTerminal('completed')
    activeAppend.release()
    await expect(terminal).resolves.toEqual({ committedCount: 2, state: 'completed' })

    const checkpoint = decodeCompatibleNameSidecar(writer.bytes)
    expect(checkpoint.footer).toEqual({ committedCount: 2, state: 'completed' })
    expect(checkpoint.trailingByteLength).toBe(0)
    expect(writer.appendPayloads.at(-1)).toMatch(/F\t2\tcompleted\n$/u)
    expect(writer.maxConcurrentMutations).toBe(1)
  })

  it('accepts an already validated matching terminal footer without appending it again', async () => {
    const mapping = committedMapping(1, ['first.txt'])
    const writer = new MemoryOwnedSidecarWriter(bytes(sidecar([mapping], 'failed')))
    const ledger = new MemoryProjectorLedger([mapping])
    const projector = await createCompatibleNameProjector(projectorOptions(ledger, writer))

    await expect(projector.drainTerminal('failed'))
      .resolves.toEqual({ committedCount: 1, state: 'failed' })
    expect(writer.mutations).toHaveLength(0)
    expect(ledger.cursors).toEqual([1, 1])

    const mismatchedWriter = new MemoryOwnedSidecarWriter(bytes(sidecar([mapping], 'failed')))
    const mismatchedProjector = await createCompatibleNameProjector(projectorOptions(
      new MemoryProjectorLedger([mapping]),
      mismatchedWriter,
    ))
    await expect(mismatchedProjector.drainTerminal('completed'))
      .rejects.toThrow('disagrees with durable ledger')
    expect(mismatchedWriter.mutations).toHaveLength(0)
  })
})

class MemoryProjectorLedger implements CompatibleNameProjectorLedger {
  readonly committed: CompatibleNameMappingV1[]
  readonly cursors: number[] = []

  constructor(committed: readonly CompatibleNameMappingV1[] = []) {
    this.committed = [...committed]
  }

  scanCommittedMappings(
    operationId: string,
    afterOrdinal = 0,
  ): Promise<readonly CompatibleNameMappingV1[]> {
    if (operationId !== OPERATION_ID) return Promise.reject(new Error('wrong operation'))
    this.cursors.push(afterOrdinal)
    const snapshot = this.committed.filter(mapping =>
      (mapping.commitOrdinal ?? 0) > afterOrdinal)
    return Promise.resolve(Object.freeze(snapshot))
  }
}

type Mutation = Readonly<{
  kind: 'append' | 'truncate' | 'replace'
  ownershipVerified: boolean
}>

class MemoryOwnedSidecarWriter implements CompatibleNameOwnedSidecarWriter {
  bytes: Uint8Array
  readonly mutations: Mutation[] = []
  readonly appendPayloads: string[] = []
  maxConcurrentMutations = 0

  #activeMutations = 0
  #nextAppendGate: Deferred<void> | undefined
  readonly #completedAppendWaiters: Array<Readonly<{
    count: number
    deferred: Deferred<void>
  }>> = []

  constructor(initialBytes: Uint8Array) {
    this.bytes = initialBytes.slice()
  }

  readOwnedBytes(): Promise<Uint8Array> {
    return Promise.resolve(this.bytes.slice())
  }

  async appendOwnedCheckpoint(value: Uint8Array): Promise<void> {
    this.#beginMutation('append')
    const gate = this.#nextAppendGate
    this.#nextAppendGate = undefined
    gate?.resolveStarted()
    try {
      if (gate !== undefined) await gate.promise
      this.appendPayloads.push(TEXT_DECODER.decode(value))
      this.bytes = concatenate(this.bytes, value)
    } finally {
      this.#endMutation()
      this.#resolveCompletedAppendWaiters()
    }
  }

  truncateOwnedBytes(byteLength: number): Promise<void> {
    this.#beginMutation('truncate')
    try {
      this.bytes = this.bytes.slice(0, byteLength)
      return Promise.resolve()
    } finally {
      this.#endMutation()
    }
  }

  replaceOwnedCheckpoint(value: Uint8Array): Promise<void> {
    this.#beginMutation('replace')
    try {
      this.bytes = value.slice()
      return Promise.resolve()
    } finally {
      this.#endMutation()
    }
  }

  blockNextAppend(): Readonly<{
    started: Promise<void>
    release: () => void
  }> {
    const gate = deferred<void>()
    this.#nextAppendGate = gate
    return Object.freeze({
      started: gate.started,
      release: () => gate.resolve(),
    })
  }

  waitForCompletedAppendCount(count: number): Promise<void> {
    if (this.appendPayloads.length >= count) return Promise.resolve()
    const waiter = deferred<void>()
    this.#completedAppendWaiters.push(Object.freeze({ count, deferred: waiter }))
    return waiter.promise
  }

  #beginMutation(kind: Mutation['kind']): void {
    this.#activeMutations += 1
    this.maxConcurrentMutations = Math.max(this.maxConcurrentMutations, this.#activeMutations)
    this.mutations.push(Object.freeze({ kind, ownershipVerified: true }))
  }

  #endMutation(): void {
    this.#activeMutations -= 1
  }

  #resolveCompletedAppendWaiters(): void {
    for (const waiter of this.#completedAppendWaiters) {
      if (this.appendPayloads.length >= waiter.count) waiter.deferred.resolve()
    }
  }
}

interface Deferred<T> {
  readonly promise: Promise<T>
  readonly started: Promise<void>
  resolve(value: T): void
  resolveStarted(): void
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  let resolveStarted!: () => void
  return {
    promise: new Promise<T>(complete => { resolve = complete }),
    started: new Promise<void>(complete => { resolveStarted = complete }),
    resolve,
    resolveStarted,
  }
}

function projectorOptions(
  ledger: CompatibleNameProjectorLedger,
  writer: CompatibleNameOwnedSidecarWriter,
) {
  return {
    operationId: OPERATION_ID,
    pairPlacement: 'inside-logical-root' as const,
    ledger,
    writer,
  }
}

function committedMapping(
  ordinal: number,
  logicalPath: readonly string[],
): CompatibleNameMappingV1 {
  return compatibleNameMappingV1({
    operationId: OPERATION_ID,
    logicalPath,
    entryKind: logicalPath.at(-1)?.includes('.') === true ? 'file' : 'directory',
    physicalComponent: `compatible-${ordinal}.windshare-aaaaaa`,
    attempt: 0,
    token: 'aaaaaa',
    ownershipState: 'owned',
    ownedObjectId: identity(32, ordinal + 1),
    commitState: 'committed',
    commitOrdinal: ordinal,
  })
}

function projectedMapping(mapping: CompatibleNameMappingV1) {
  return {
    ordinal: mapping.commitOrdinal as number,
    entryKind: mapping.entryKind,
    logicalPath: mapping.logicalPath,
    physicalComponent: mapping.physicalComponent,
  }
}

function sidecar(
  mappings: readonly CompatibleNameMappingV1[],
  state: 'active' | 'completed' | 'stopped' | 'failed',
): string {
  return HEADER + mappings.map(mapping =>
    encodeCompatibleNameSidecarMapping(projectedMapping(mapping))).join('') +
    encodeCompatibleNameSidecarFooter({ committedCount: mappings.length, state })
}

function mappingLines(value: string | undefined): readonly string[] {
  return value?.split('\n').filter(line => line.startsWith('M\t')) ?? []
}

function concatenate(left: Uint8Array, right: Uint8Array): Uint8Array {
  const combined = new Uint8Array(left.byteLength + right.byteLength)
  combined.set(left)
  combined.set(right, left.byteLength)
  return combined
}

function bytes(value: string): Uint8Array<ArrayBuffer> {
  return TEXT_ENCODER.encode(value)
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
