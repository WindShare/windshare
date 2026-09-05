import type { CompatibleNameRepairSummary } from './model'
import type { CompatibleNameOwnedSidecarWriter } from './projector'
import type { CompatibleNameRepairProjectionSource } from './path-authority'

export class DurableCompatibleNameRepairProjection implements CompatibleNameRepairProjectionSource {
  readonly #listeners = new Set<(summary: CompatibleNameRepairSummary) => void>()
  #latest: CompatibleNameRepairSummary | undefined

  subscribe(listener: (summary: CompatibleNameRepairSummary) => void): () => void {
    this.#listeners.add(listener)
    if (this.#latest !== undefined) listener(this.#latest)
    return () => { this.#listeners.delete(listener) }
  }

  publish(summary: CompatibleNameRepairSummary): void {
    if (this.#latest !== undefined && !summaryFollows(this.#latest, summary)) {
      throw new DOMException('Compatible-name repair projection regressed', 'InvalidStateError')
    }
    if (sameRepairSummary(this.#latest, summary)) return
    this.#latest = summary
    for (const listener of this.#listeners) {
      try {
        listener(summary)
      } catch {
        // A presentation listener cannot interfere with durable projection.
      }
    }
  }
}

export class FSAOwnedSidecarWriter implements CompatibleNameOwnedSidecarWriter {
  readonly #openOwned: () => Promise<FileSystemFileHandle>
  #tail = Promise.resolve()

  constructor(openOwned: () => Promise<FileSystemFileHandle>) {
    this.#openOwned = openOwned
  }

  readOwnedBytes(): Promise<Uint8Array> {
    return this.#run(async () => readFileBytes(await this.#openOwned()))
  }

  appendOwnedCheckpoint(bytes: Uint8Array): Promise<void> {
    return this.#run(async () => {
      const handle = await this.#openOwned()
      const byteLength = (await handle.getFile()).size
      await mutateOwnedFile(handle, async writer => {
        await writer.write({ type: 'write', position: byteLength, data: bytes.slice() })
      }, true)
    })
  }

  truncateOwnedBytes(byteLength: number): Promise<void> {
    return this.#run(async () => {
      const handle = await this.#openOwned()
      await mutateOwnedFile(handle, writer => writer.truncate(byteLength), true)
    })
  }

  replaceOwnedCheckpoint(bytes: Uint8Array): Promise<void> {
    return this.#run(async () => {
      const handle = await this.#openOwned()
      await mutateOwnedFile(handle, writer => writer.write(bytes.slice()), false)
    })
  }

  #run<T>(operation: () => Promise<T>): Promise<T> {
    const result = this.#tail.then(operation)
    this.#tail = result.then(() => undefined, () => undefined)
    return result
  }
}

export async function replaceAndCloseFile(handle: FileSystemFileHandle, bytes: Uint8Array): Promise<void> {
  const writer = await handle.createWritable()
  const writableBytes = new Uint8Array(bytes.byteLength)
  writableBytes.set(bytes)
  try {
    await writer.write({ type: 'write', position: 0, data: writableBytes })
    await writer.close()
  } catch (error) {
    try {
      await writer.abort(error)
    } catch {
      // The initiating write/close failure remains authoritative.
    }
    throw error
  }
}

async function mutateOwnedFile(
  handle: FileSystemFileHandle,
  mutate: (writer: FileSystemWritableFileStream) => Promise<void>,
  keepExistingData: boolean,
): Promise<void> {
  const writer = await handle.createWritable({ keepExistingData })
  try {
    await mutate(writer)
    await writer.close()
  } catch (error) {
    try {
      await writer.abort(error)
    } catch {
      // The initiating sidecar mutation failure remains authoritative.
    }
    throw error
  }
}

export async function readFileBytes(handle: FileSystemFileHandle): Promise<Uint8Array> {
  return new Uint8Array(await (await handle.getFile()).arrayBuffer())
}

export function equalBytes(left: Uint8Array, right: Uint8Array): boolean {
  if (left.byteLength !== right.byteLength) return false
  return left.every((value, index) => value === right[index])
}

export function sameLogicalPath(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((component, index) => component === right[index])
}

export function sameRepairSummary(
  left: CompatibleNameRepairSummary | undefined,
  right: CompatibleNameRepairSummary | undefined,
): boolean {
  if (left === undefined || right === undefined) return left === right
  return left.committedCount === right.committedCount && left.placement === right.placement &&
    left.sidecarSync === right.sidecarSync && left.terminalSettlement === right.terminalSettlement &&
    left.pairDisplayNames.script === right.pairDisplayNames.script &&
    left.pairDisplayNames.sidecar === right.pairDisplayNames.sidecar &&
    left.latestObservedFooter?.committedCount === right.latestObservedFooter?.committedCount &&
    left.latestObservedFooter?.state === right.latestObservedFooter?.state &&
    left.logicalPathSample.length === right.logicalPathSample.length &&
    left.logicalPathSample.every((path, index) => {
      const expected = right.logicalPathSample[index]
      return expected !== undefined && sameLogicalPath(path, expected)
    })
}

function summaryFollows(
  prior: CompatibleNameRepairSummary,
  next: CompatibleNameRepairSummary,
): boolean {
  if (next.committedCount < prior.committedCount || next.placement !== prior.placement ||
      next.pairDisplayNames.script !== prior.pairDisplayNames.script ||
      next.pairDisplayNames.sidecar !== prior.pairDisplayNames.sidecar) return false
  const priorFooter = prior.latestObservedFooter
  const nextFooter = next.latestObservedFooter
  if (priorFooter !== undefined &&
      (nextFooter === undefined || nextFooter.committedCount < priorFooter.committedCount)) return false
  if (priorFooter !== undefined && priorFooter.state !== 'active' &&
      (nextFooter?.state !== priorFooter.state || nextFooter.committedCount !== priorFooter.committedCount)) {
    return false
  }
  return prior.logicalPathSample.every((path, index) => {
    const candidate = next.logicalPathSample[index]
    return candidate !== undefined && sameLogicalPath(path, candidate)
  })
}
