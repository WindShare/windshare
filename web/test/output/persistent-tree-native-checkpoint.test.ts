import { describe, expect, it, vi } from 'vitest'
import { materializationFixture, revision } from './persistent-tree-session-fixture'

async function nativeFixture() {
  const fixture = await materializationFixture()
  const createFile = fixture.tree.createFileAfterRevisionOpen.bind(fixture.tree)
  vi.spyOn(fixture.tree, 'createFileAfterRevisionOpen').mockImplementation(async (...args) => {
    const file = await createFile(...args)
    Object.defineProperty(file, 'durability', { value: 'native-in-place' })
    return file
  })
  const transaction = await fixture.session.beginFile({
    materializationRelativePath: ['native.bin'], openRevision: async () => revision(6n),
  })
  const file = fixture.tree.file(['native.bin'])
  const close = vi.spyOn(file, 'close')
  return { ...fixture, file, transaction, close }
}

describe('native persistent tree durability', () => {
  it('automatically checkpoints without copy admission or reopening and closes on final commit', async () => {
    const fixture = await nativeFixture()
    expect(fixture.transaction.checkpointPolicy).toEqual({ kind: 'incremental',
      pendingBytes: 16n * 1024n * 1024n, pendingMilliseconds: 5_000 })
    await fixture.transaction.writeRange(0n, Uint8Array.of(1, 2, 3))
    await expect(fixture.transaction.automaticCheckpoint('pending-bytes')).resolves.toMatchObject({
      kind: 'advanced', durableRanges: [{ start: 0n, end: 3n }],
      cost: { prefixCopyBytes: 0n, writeAmplificationBytes: 0n, temporaryBytes: 0n },
    })
    expect(fixture.close).not.toHaveBeenCalled()
    await fixture.transaction.writeRange(3n, Uint8Array.of(4, 5, 6))
    await fixture.transaction.automaticCheckpoint('pending-bytes')
    await fixture.transaction.commit()
    expect(fixture.file.writerModes).toEqual(['truncate'])
    expect(fixture.file.preservingCostCount).toBe(0)
    expect(fixture.file.flushCount).toBe(2)
    expect(fixture.close).toHaveBeenCalledOnce()
  })

  it('retains the native writer through the final metadata commit after the last uncheckpointed write', async () => {
    const fixture = await nativeFixture()
    await fixture.transaction.writeRange(0n, Uint8Array.of(1, 2, 3, 4, 5, 6))
    const commitFinalFile = fixture.checkpoints.commitFinalFile.bind(fixture.checkpoints)
    const finalMetadata = vi.spyOn(fixture.checkpoints, 'commitFinalFile').mockImplementation(async input => {
      expect(fixture.file.flushCount).toBe(1)
      expect(fixture.close).not.toHaveBeenCalled()
      return commitFinalFile(input)
    })
    await fixture.transaction.commit()
    expect(finalMetadata).toHaveBeenCalledOnce()
    expect(fixture.close).toHaveBeenCalledOnce()
  })

  it('skips authenticated retries without mutating committed revision bytes', async () => {
    const { transaction, file } = await nativeFixture()
    await transaction.writeRange(0n, Uint8Array.of(1, 2, 3))
    await transaction.automaticCheckpoint('pending-bytes')
    await transaction.writeRange(0n, Uint8Array.of(9, 9, 9))
    expect([...file.snapshot()]).toEqual([1, 2, 3])
    await expect(transaction.writeRange(2n, Uint8Array.of(8, 8))).rejects.toThrow('cannot overwrite')
    expect([...file.snapshot()]).toEqual([1, 2, 3])
  })

  it.each(['flush', 'metadata'])('stops after %s failure and keeps only previously committed ranges', async stage => {
    const { transaction, file, checkpoints } = await nativeFixture()
    await transaction.writeRange(0n, Uint8Array.of(1, 2, 3))
    await transaction.automaticCheckpoint('pending-bytes')
    await transaction.writeRange(3n, Uint8Array.of(4, 5, 6))
    if (stage === 'flush') file.failNextFlush()
    else vi.spyOn(checkpoints, 'commitDurableCut').mockRejectedValueOnce(new Error('metadata failed'))
    await expect(transaction.automaticCheckpoint('pending-bytes')).rejects.toThrow()
    expect(transaction.verifiedRanges).toEqual([{ start: 0n, end: 3n }])
    await expect(transaction.writeRange(3n, Uint8Array.of(4, 5, 6))).rejects.toThrow('settled')
    expect(file.abortCount).toBe(1)
  })

  it('pause flushes accepted data before metadata and releases the native handle', async () => {
    const { transaction, file, close } = await nativeFixture()
    await transaction.writeRange(0n, Uint8Array.of(1, 2, 3))
    await expect(transaction.pause()).resolves.toEqual([{ start: 0n, end: 3n }])
    expect(file.flushCount).toBe(1)
    expect(close).toHaveBeenCalledOnce()
  })

  it('retirement settles accepted writes but grants no rollback or new durable authority', async () => {
    const { transaction, file } = await nativeFixture()
    const write = transaction.writeRange(0n, Uint8Array.of(1, 2, 3))
    const retired = transaction.retire()
    await Promise.all([write, retired])
    expect([...file.snapshot()]).toEqual([1, 2, 3])
    expect(transaction.verifiedRanges).toEqual([])
    expect(file.abortCount).toBe(1)
  })
})
