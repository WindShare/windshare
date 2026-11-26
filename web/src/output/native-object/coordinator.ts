import { FaultScope, OutputFaultCode, outputFailure } from '../../transfer/fault'
import type { NativeObjectIO } from './contracts'

export type NativeObjectTrace = (event: Readonly<{
  operationId: string; objectId: string; stage: string; reason?: string
}>) => void

/** One queue owns the cut across every entry sharing an object's native handle. */
export class ObjectCheckpointCoordinator implements NativeObjectIO {
  readonly #io: NativeObjectIO
  readonly #identity: Readonly<{ operationId: string; objectId: string }>
  readonly #trace: NativeObjectTrace | undefined
  #tail: Promise<unknown> = Promise.resolve()
  #failure: unknown
  #closed = false

  constructor(input: {
    io: NativeObjectIO; operationId: string; objectId: string; trace?: NativeObjectTrace
  }) {
    this.#io = input.io
    this.#identity = { operationId: input.operationId, objectId: input.objectId }
    this.#trace = input.trace
  }

  get failed(): boolean { return this.#failure !== undefined }

  mutate<T>(operation: (io: NativeObjectIO) => Promise<T>): Promise<T> {
    return this.#enqueue(() => operation(this.#io))
  }

  writeAt(offset: bigint, bytes: Uint8Array): Promise<void> {
    return this.mutate(io => io.writeAt(offset, bytes))
  }
  truncate(length: bigint): Promise<void> { return this.mutate(io => io.truncate(length)) }
  size(): Promise<bigint> { return this.mutate(io => io.size()) }
  flush(): Promise<void> { return this.mutate(io => io.flush()) }

  checkpoint<T>(reason: string, commit: () => Promise<T>): Promise<T> {
    return this.#enqueue(() => this.#flushAndCommit(reason, commit))
  }

  async #flushAndCommit<T>(reason: string, commit: () => Promise<T>): Promise<T> {
    this.#emit('cut-started', reason)
    await this.#io.flush()
    this.#emit('flush-succeeded', reason)
    const result = await commit()
    this.#emit('commit-succeeded', reason)
    return result
  }

  close(): Promise<void> {
    const result = this.#tail.then(async () => {
      if (this.#closed) return
      this.#closed = true
      await this.#io.close()
      this.#emit('closed')
    })
    this.#tail = result.catch(() => undefined)
    return result
  }

  #enqueue<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.#tail.then(async () => {
      if (this.#failure !== undefined) throw this.#failure
      if (this.#closed) throw new Error('Native object coordinator is closed')
      try { return await operation() } catch (error) {
        this.#failure = outputFailure(FaultScope.Job, error, OutputFaultCode.WriteFailed)
        this.#closed = true
        this.#emit('stopped')
        try { await this.#io.close() } catch { /* Preserve the failed cut's cause. */ }
        throw this.#failure
      }
    })
    this.#tail = result.catch(() => undefined)
    return result
  }

  #emit(stage: string, reason?: string): void {
    try { this.#trace?.({ ...this.#identity, stage, ...(reason === undefined ? {} : { reason }) }) }
    catch { /* Observers cannot change durable authority. */ }
  }
}
