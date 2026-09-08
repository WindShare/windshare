import { describe, expect, it } from 'vitest'
import type { TaskEntry } from '../../../src/output/origin-private/task-checkpoint/model'
import { zipFinalizationBatches } from '../../../src/output/progressive-zip/finalization'
import { normalizeZipEntry, planZipEntry } from '../../../src/output/zip-layout/policy'

function directory(sequence: bigint, path: readonly string[], offset: bigint): TaskEntry {
  const plan = planZipEntry(normalizeZipEntry({ kind: 'directory', path }), offset)
  const payloadOffset = offset + plan.localHeaderBytes
  return {
    entryId: String(sequence), kind: 'directory', path, ranges: [], zipPlan: plan,
    source: { shareInstance: 'share', directoryId: 'root', generation: 'generation', sourcePath: path },
    zipLayout: {
      entryId: String(sequence), sequence, localHeaderOffset: offset, payloadOffset,
      exactSize: 0n, descriptorOffset: payloadOffset,
      endOffset: offset + plan.entryStreamBytes, encodingVersion: 1,
    },
  }
}

async function* stream(entries: readonly TaskEntry[]) { yield* entries }

describe('ZIP finalization metadata bounds', () => {
  it('cuts long-path batches by byte size while preserving the resume cursor and contiguous central directory', async () => {
    const entries: TaskEntry[] = []
    let offset = 0n
    for (let index = 0; index < 12; index++) {
      const path = [...Array.from({ length: 120 }, () => 'x'.repeat(250)), String(index)]
      const entry = directory(BigInt(index + 128), path, offset)
      entries.push(entry)
      offset = entry.zipLayout!.endOffset
    }
    let cursor = 128n
    let centralOffset = offset
    let batches = 0
    for await (const batch of zipFinalizationBatches(stream(entries), offset)) {
      const writtenBytes = batch.writes.reduce((sum, write) => sum + write.bytes.byteLength, 0)
      expect(writtenBytes).toBeLessThanOrEqual(256 * 1024)
      expect(batch.nextEntry).toBeGreaterThan(cursor)
      const central = batch.writes.at(-1)!
      expect(central.offset).toBe(centralOffset)
      expect(batch.committedLength).toBe(centralOffset + BigInt(central.bytes.byteLength))
      centralOffset = batch.committedLength
      cursor = batch.nextEntry
      batches++
    }
    expect(cursor).toBe(140n)
    expect(batches).toBeGreaterThan(1)
    expect(batches).toBeLessThan(entries.length)
  })

  it('does not produce a write batch when a resumed central directory already covers all entries', async () => {
    const batches = []
    for await (const batch of zipFinalizationBatches(stream([]), 100n)) batches.push(batch)
    expect(batches).toEqual([])
  })
})
