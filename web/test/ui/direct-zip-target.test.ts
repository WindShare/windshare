import { describe, expect, it } from 'vitest'
import { decodeBase64Url } from '../../src/crypto/bytes'
import { DirectZipEpochWriterV1 } from '../../src/output/direct-zip/writer'
import type { DirectZipEpochProofV1, DirectZipWriterCheckpointV1 } from '../../src/output/direct-zip/writer'
import type { DirectZipFileSystemPort } from '../../src/output/direct-zip/target'
import { BrowserDirectZipTarget, type BrowserDirectZipBinding, type DirectZipNamespaceMutationPort } from '../../src/ui/browser-receive/direct-zip/target'
import { StagedFsaModel, type StagedFileHandle } from '../output/direct-zip/target/staged-fsa-model'
import { createWriterHarness, fileAdmission, fileSource } from '../output/direct-zip/writer/fault-model'

const TARGET_NAME = 'root.windshare-owned.zip'
const PAYLOAD_BYTES = 65_536
const PAYLOAD = new Uint8Array(PAYLOAD_BYTES).fill(0xa7)
const ROLLBACK_PAYLOAD_BYTES = 4_096
const ROLLBACK_PAYLOAD = PAYLOAD.subarray(0, ROLLBACK_PAYLOAD_BYTES)

describe('browser Direct ZIP target bridge', () => {
  it('completes a real writer close with the final candidate observation before journal promotion', async () => {
    const fixture = await targetFixture()
    const writer = fixture.writer()
    const member = await writer.beginFile(fileAdmission(fixture.checkpoint, fileSource('revision-1', BigInt(PAYLOAD_BYTES))))
    await member.write(PAYLOAD)
    await member.close()
    const saved = await writer.pause()
    const pages = await fixture.harness.pages.snapshot()
    const completion = await writer.closeArchive({
      entryCount: pages.centralRecordCount,
      centralDirectoryBytes: pages.centralBytes,
      layoutRoot: pages.layoutRoot,
      centralRoot: pages.centralRoot,
      predecessorEpochRoot: saved.checkpoint.epochRoot,
    })

    expect(completion.exactArchiveBytes).toBe(BigInt(fixture.model.fileBytes(TARGET_NAME)!.byteLength))
    expect(fixture.harness.cuts.promoted.at(-1)?.completion).toBeDefined()
    await expect(fixture.reopen().verifyCheckpoint(completion.checkpoint)).resolves.toBeUndefined()
  })

  it('uses bounded observation after a successful live close instead of rereading the payload', async () => {
    const fixture = await targetFixture()
    const writer = fixture.writer()
    const member = await writer.beginFile(fileAdmission(fixture.checkpoint, fileSource('revision-1', BigInt(PAYLOAD_BYTES))))
    await member.write(PAYLOAD)
    fixture.reads.length = 0
    const saved = await writer.pause()

    expect(saved.checkpoint.safeResumeBytes).toBe(BigInt(PAYLOAD_BYTES))
    expect(fixture.reads.every(read => read.end - read.start < BigInt(PAYLOAD_BYTES))).toBe(true)
  })

  it('rehashes bytes when close throws after publication, retaining verified progress', async () => {
    const fixture = await targetFixture()
    const writer = fixture.writer()
    const member = await writer.beginFile(fileAdmission(fixture.checkpoint, fileSource('revision-1', BigInt(PAYLOAD_BYTES))))
    await member.write(PAYLOAD)
    fixture.model.faultOnce('close-after-publication', new DOMException('Ambiguous close', 'UnknownError'))
    fixture.reads.length = 0
    const saved = await writer.pause()

    expect(saved.checkpoint.safeResumeBytes).toBe(BigInt(PAYLOAD_BYTES))
    expect(fixture.reads.some(read => read.end - read.start >= BigInt(PAYLOAD_BYTES))).toBe(true)
  })

  it('recovers a closed but unpromoted candidate from a new target instance without losing payload', async () => {
    const fixture = await targetFixture()
    const writer = fixture.writer()
    const member = await writer.beginFile(fileAdmission(fixture.checkpoint, fileSource('revision-1', BigInt(PAYLOAD_BYTES))))
    await member.write(PAYLOAD)
    fixture.harness.cuts.failPromotionCount = 1
    await expect(writer.pause()).rejects.toThrow('injected journal promotion failure')
    const candidate = fixture.harness.cuts.staged.at(-1)!
    const restored = fixture.writer(fixture.checkpoint, fixture.reopen())
    const result = await restored.recoverCandidate(candidate)

    expect(result.kind).toBe('promoted')
    expect(result.checkpoint.safeResumeBytes).toBe(BigInt(PAYLOAD_BYTES))
    expect(fixture.harness.cuts.promoted).toHaveLength(1)
  })

  it('rejects changed payload on restored checkpoint even when locator and timestamp still match', async () => {
    const fixture = await targetFixture()
    const writer = fixture.writer()
    const member = await writer.beginFile(fileAdmission(fixture.checkpoint, fileSource('revision-1', BigInt(PAYLOAD_BYTES))))
    await member.write(PAYLOAD)
    const saved = await writer.pause()
    const originalTimestamp = fixture.file.node.lastModified
    const lastByte = fixture.file.node.bytes.length - 1
    fixture.file.node.bytes[lastByte] = fixture.file.node.bytes[lastByte]! ^ 0xff
    fixture.file.node.lastModified = originalTimestamp

    await expect(fixture.reopen().verifyCheckpoint(saved.checkpoint)).rejects.toThrow('committed bytes')
    expect(fixture.model.calls.filter(call => call.startsWith('remove:'))).toHaveLength(0)
  })

  it('rejects a foreign marker even when restored handles still identify the same path', async () => {
    const fixture = await targetFixture()
    fixture.model.replaceFile(TARGET_NAME, new Uint8Array(Number(fixture.checkpoint.committedLength)))

    await expect(fixture.target.openEpoch(fixture.checkpoint))
      .resolves.toEqual({ kind: 'target-verification-required' })
    await expect(fixture.target.deleteOwned(fixture.checkpoint)).rejects.toThrow()
    expect(fixture.model.calls.some(call => call.startsWith('writable:') || call.startsWith('remove:'))).toBe(false)
  })

  it('deletes a verified pending candidate without opening another writer or truncating progress', async () => {
    const fixture = await targetFixture()
    const writer = fixture.writer()
    const member = await writer.beginFile(fileAdmission(fixture.checkpoint, fileSource('revision-1', BigInt(PAYLOAD_BYTES))))
    await member.write(PAYLOAD)
    fixture.harness.cuts.failPromotionCount = 1
    await expect(writer.pause()).rejects.toThrow('injected journal promotion failure')
    const candidate = fixture.harness.cuts.staged.at(-1)!
    const writesBeforeCleanup = fixture.model.calls.filter(call => call.startsWith('writable:')).length

    await fixture.reopen().deleteOwned(fixture.checkpoint, candidate)

    expect(fixture.model.fileBytes(TARGET_NAME)).toBeUndefined()
    expect(fixture.model.calls.filter(call => call.startsWith('writable:'))).toHaveLength(writesBeforeCleanup)
  })

  it('refuses deletion when the final bounded observation detects new bytes after the ownership proof', async () => {
    const fixture = await targetFixture()
    const snapshot = fixture.fileSystem.snapshot
    let observations = 0
    fixture.fileSystem.snapshot = async handle => {
      observations += 1
      if (observations === 2) {
        const extended = new Uint8Array(fixture.file.node.bytes.length + 1)
        extended.set(fixture.file.node.bytes)
        extended[extended.length - 1] = 0x77
        fixture.file.node.bytes = extended
        fixture.file.node.lastModified += 1
      }
      return snapshot(handle)
    }

    await expect(fixture.target.deleteOwned(fixture.checkpoint)).rejects.toThrow()
    expect(fixture.model.fileBytes(TARGET_NAME)).toHaveLength(Number(fixture.checkpoint.committedLength) + 1)
    expect(fixture.model.calls.some(call => call.startsWith('remove:'))).toBe(false)
  })

  it('keeps archive proof scans outside the directory mutation fence and removes only inside it', async () => {
    let inNamespace = false
    const fixture = await targetFixture({
      run: async operation => {
        expect(fixture.reads.some(read => read.end - read.start >= BigInt(PAYLOAD_BYTES))).toBe(true)
        fixture.reads.length = 0
        inNamespace = true
        try { return await operation() } finally { inNamespace = false }
      },
    })
    const writer = fixture.writer()
    const member = await writer.beginFile(fileAdmission(fixture.checkpoint, fileSource('revision-1', BigInt(PAYLOAD_BYTES))))
    await member.write(PAYLOAD)
    const saved = await writer.pause()
    const remove = fixture.fileSystem.removeExactName
    fixture.fileSystem.removeExactName = async (parent, name) => {
      expect(inNamespace).toBe(true)
      expect(fixture.reads.every(read => read.end - read.start < BigInt(PAYLOAD_BYTES))).toBe(true)
      await remove(parent, name)
    }
    fixture.reads.length = 0

    await fixture.reopen().deleteOwned(saved.checkpoint)

    expect(fixture.model.fileBytes(TARGET_NAME)).toBeUndefined()
    expect(inNamespace).toBe(false)
  })

  it('refuses a replacement that appears while deletion waits for namespace authority', async () => {
    const fixture = await targetFixture({
      run: async operation => {
        fixture.model.replaceFile(TARGET_NAME, Uint8Array.of(9, 8, 7))
        return operation()
      },
    })

    await expect(fixture.target.deleteOwned(fixture.checkpoint)).rejects.toMatchObject({ name: 'DataError' })
    expect(fixture.model.fileBytes(TARGET_NAME)).toEqual(Uint8Array.of(9, 8, 7))
    expect(fixture.model.calls.some(call => call.startsWith('remove:'))).toBe(false)
  })

  it('distinguishes lost permission and missing targets without creating or deleting files', async () => {
    const fixture = await targetFixture()
    fixture.model.queryPermissionState = 'denied'
    await expect(fixture.target.openEpoch(fixture.checkpoint)).resolves.toEqual({ kind: 'authorization-required' })
    fixture.model.queryPermissionState = 'granted'
    fixture.model.deleteFile(TARGET_NAME)
    await expect(fixture.target.openEpoch(fixture.checkpoint)).resolves.toEqual({ kind: 'target-deleted' })
    expect(fixture.model.calls.some(call => call.startsWith('writable:') || call.startsWith('remove:'))).toBe(false)
  })
})

describe('browser Direct ZIP durable member rollback', () => {
  it('retains the verified completed prefix and accepts a fresh recovery without truncating twice', async () => {
    const fixture = await rollbackFixture()
    const expected = fixture.file.node.bytes.slice(0, Number(fixture.rollback.committedLength))
    const observation = await fixture.recover()

    expect(fixture.file.node.bytes).toEqual(expected)
    expect(observation).toEqual(await fixture.target.observe(fixture.rollback.epochRoot))
    const opened = fixture.opened()
    await expect(fixture.reopen().recoverMemberRollback(fixture.previous, fixture.rollback, fixture.proofs))
      .resolves.toEqual(observation)
    expect(fixture.opened()).toBe(opened)
    await expect(fixture.target.verifyPredecessor(fixture.rollback)).resolves.toEqual({ kind: 'accepted-fast' })
  })

  it.each(['retained-prefix', 'discarded-suffix'] as const)(
    'refuses changed %s bytes even when length, timestamp and marker remain identical', async region => {
      const fixture = await rollbackFixture()
      const offset = region === 'retained-prefix' ? Number(fixture.rollback.committedLength) - 1
        : fixture.file.node.bytes.length - 1
      fixture.file.node.bytes[offset] = fixture.file.node.bytes[offset]! ^ 0xff
      const before = Uint8Array.from(fixture.file.node.bytes)
      const opened = fixture.opened()

      await expect(fixture.recover()).rejects.toMatchObject({ name: 'DataError' })
      expect(fixture.file.node.bytes).toEqual(before)
      expect(fixture.opened()).toBe(opened)
    },
  )

  it('requires the proposed prefix proofs as well as the complete old proofs before mutation', async () => {
    const fixture = await rollbackFixture()
    const opened = fixture.opened()
    const changedRoot = Uint8Array.from(fixture.rollback.epochRoot)
    changedRoot[0] = changedRoot[0]! ^ 0xff

    await expect(fixture.target.recoverMemberRollback(fixture.previous,
      { ...fixture.rollback, epochRoot: changedRoot }, fixture.proofs)).rejects.toMatchObject({ name: 'DataError' })
    expect(fixture.opened()).toBe(opened)
    expect(BigInt(fixture.file.node.bytes.length)).toBe(fixture.previous.committedLength)
  })

  it.each(['unknown-tail', 'intermediate-length', 'shorter-prefix'] as const)(
    'refuses %s without opening a writable', async shape => {
      const fixture = await rollbackFixture()
      const lengths = {
        'unknown-tail': fixture.previous.committedLength + 1n,
        'intermediate-length': fixture.rollback.committedLength + 1n,
        'shorter-prefix': fixture.rollback.committedLength - 1n,
      }
      const length = lengths[shape]
      const bytes = new Uint8Array(Number(length))
      bytes.set(fixture.file.node.bytes.subarray(0, bytes.length))
      fixture.file.node.bytes = bytes
      const opened = fixture.opened()

      await expect(fixture.recover()).rejects.toMatchObject({ name: 'DataError' })
      expect(fixture.file.node.bytes).toEqual(bytes)
      expect(fixture.opened()).toBe(opened)
    },
  )

  it('rechecks the bounded observation after waiting for namespace mutation authority', async () => {
    let replaceBeforeMutation = false
    const fixture = await rollbackFixture({
      run: async operation => {
        if (replaceBeforeMutation) fixture.file.node.lastModified += 1
        return operation()
      },
    })
    replaceBeforeMutation = true
    const opened = fixture.opened()

    await expect(fixture.recover()).rejects.toMatchObject({ name: 'DataError' })
    expect(fixture.opened()).toBe(opened)
  })

  it('rechecks ownership again after opening a writable and before truncating', async () => {
    const fixture = await rollbackFixture()
    const open = fixture.fileSystem.createWritable
    let truncated = false
    fixture.fileSystem.createWritable = async (handle, keep) => {
      const writable = await open(handle, keep)
      fixture.file.node.lastModified += 1
      return { ...writable, truncate: async length => {
        truncated = true
        await writable.truncate(length)
      } }
    }

    await expect(fixture.recover()).rejects.toMatchObject({ name: 'DataError' })
    expect(truncated).toBe(false)
    expect(BigInt(fixture.file.node.bytes.length)).toBe(fixture.previous.committedLength)
  })

  it('refuses a changed prefix on restart after truncation without reopening the file', async () => {
    const fixture = await rollbackFixture()
    await fixture.recover()
    const last = fixture.file.node.bytes.length - 1
    fixture.file.node.bytes[last] = fixture.file.node.bytes[last]! ^ 0xff
    const opened = fixture.opened()

    await expect(fixture.reopen().recoverMemberRollback(fixture.previous, fixture.rollback, fixture.proofs))
      .rejects.toMatchObject({ name: 'DataError' })
    expect(fixture.opened()).toBe(opened)
  })

  it.each(['permission', 'missing', 'foreign-marker'] as const)(
    'refuses a %s target before opening or truncating', async kind => {
      const fixture = await rollbackFixture()
      if (kind === 'permission') fixture.model.queryPermissionState = 'denied'
      else if (kind === 'missing') fixture.model.deleteFile(TARGET_NAME)
      else fixture.file.node.bytes.fill(0, 0, Number(fixture.checkpoint.committedLength))
      const opened = fixture.opened()
      const names = { permission: 'NotAllowedError', missing: 'NotFoundError', 'foreign-marker': 'DataError' }

      await expect(fixture.recover()).rejects.toMatchObject({ name: names[kind] })
      expect(fixture.opened()).toBe(opened)
    },
  )

  it.each(['NotAllowedError', 'QuotaExceededError'])(
    'preserves the %s writer gate when close fails before publication and supports retry', async name => {
      const fixture = await rollbackFixture()
      fixture.model.faultOnce('close-before-publication', new DOMException('Injected close failure', name))

      await expect(fixture.recover()).rejects.toMatchObject({ name })
      expect(BigInt(fixture.file.node.bytes.length)).toBe(fixture.previous.committedLength)
      await fixture.recover()
      expect(BigInt(fixture.file.node.bytes.length)).toBe(fixture.rollback.committedLength)
    },
  )

  it('accepts exact verified rollback bytes after close throws following publication', async () => {
    const fixture = await rollbackFixture()
    fixture.model.faultOnce('close-after-publication', new DOMException('Ambiguous close', 'UnknownError'))

    await fixture.recover()
    const opened = fixture.opened()
    await fixture.reopen().recoverMemberRollback(fixture.previous, fixture.rollback, fixture.proofs)
    expect(BigInt(fixture.file.node.bytes.length)).toBe(fixture.rollback.committedLength)
    expect(fixture.opened()).toBe(opened)
  })

  it('refuses a damaged retained prefix after ambiguous publication without attempting old restoration', async () => {
    const fixture = await rollbackFixture()
    fixture.model.hookOnce('close-after-publication', () => {
      const last = fixture.file.node.bytes.length - 1
      fixture.file.node.bytes[last] = fixture.file.node.bytes[last]! ^ 0xff
    })
    fixture.model.faultOnce('close-after-publication', new DOMException('Ambiguous close', 'UnknownError'))
    const opened = fixture.opened()

    await expect(fixture.recover()).rejects.toMatchObject({ name: 'DataError' })
    expect(BigInt(fixture.file.node.bytes.length)).toBe(fixture.rollback.committedLength)
    expect(fixture.opened()).toBe(opened + 1)
  })

  it.each(['NotAllowedError', 'QuotaExceededError'])(
    'preserves the %s gate when opening a writable fails', async name => {
      const fixture = await rollbackFixture()
      fixture.model.faultOnce('writable-open', new DOMException('Injected writable failure', name))

      await expect(fixture.recover()).rejects.toMatchObject({ name })
      expect(BigInt(fixture.file.node.bytes.length)).toBe(fixture.previous.committedLength)
    },
  )

  it('stops an aborted proof scan before opening a writable', async () => {
    const fixture = await rollbackFixture()
    const cancellation = new AbortController()
    const opened = fixture.opened()
    async function* abortingProofs() {
      yield* fixture.proofs()
      cancellation.abort()
    }

    await expect(fixture.target.recoverMemberRollback(fixture.previous, fixture.rollback,
      abortingProofs, cancellation.signal)).rejects.toMatchObject({ name: 'AbortError' })
    expect(fixture.opened()).toBe(opened)
  })

  it('aborts a newly opened writable before truncate when cancellation arrives during open', async () => {
    const fixture = await rollbackFixture()
    const cancellation = new AbortController()
    const open = fixture.fileSystem.createWritable
    let truncated = false
    fixture.fileSystem.createWritable = async (handle, keep) => {
      const writable = await open(handle, keep)
      cancellation.abort()
      return { ...writable, truncate: async length => {
        truncated = true
        await writable.truncate(length)
      } }
    }

    await expect(fixture.target.recoverMemberRollback(fixture.previous, fixture.rollback,
      fixture.proofs, cancellation.signal)).rejects.toMatchObject({ name: 'AbortError' })
    expect(truncated).toBe(false)
    expect(BigInt(fixture.file.node.bytes.length)).toBe(fixture.previous.committedLength)
  })

  it('returns the published observation after cancellation during close for coherent journal promotion', async () => {
    const fixture = await rollbackFixture()
    const cancellation = new AbortController()
    fixture.model.hookOnce('close-after-publication', () => cancellation.abort())

    const observation = await fixture.target.recoverMemberRollback(fixture.previous, fixture.rollback,
      fixture.proofs, cancellation.signal)
    expect(observation).toEqual(await fixture.target.observe(fixture.rollback.epochRoot))
    expect(BigInt(fixture.file.node.bytes.length)).toBe(fixture.rollback.committedLength)
  })

  it.each(['previous', 'rollback'] as const)(
    'deletes an exact verified %s shape with no additional writable', async shape => {
      const fixture = await rollbackFixture()
      if (shape === 'rollback') await fixture.recover()
      const opened = fixture.opened()

      await fixture.reopen().deleteOwnedMemberRollback(fixture.previous, fixture.rollback, fixture.proofs)
      await fixture.reopen().deleteOwnedMemberRollback(fixture.previous, fixture.rollback, fixture.proofs)
      expect(fixture.model.fileBytes(TARGET_NAME)).toBeUndefined()
      expect(fixture.opened()).toBe(opened)
    },
  )

  it('refuses pending rollback deletion when the discarded suffix was modified', async () => {
    const fixture = await rollbackFixture()
    const last = fixture.file.node.bytes.length - 1
    fixture.file.node.bytes[last] = fixture.file.node.bytes[last]! ^ 0xff

    await expect(fixture.target.deleteOwnedMemberRollback(fixture.previous, fixture.rollback, fixture.proofs))
      .rejects.toMatchObject({ name: 'DataError' })
    expect(fixture.model.calls.some(call => call.startsWith('remove:'))).toBe(false)
  })
})

async function targetFixture(namespaceMutations: DirectZipNamespaceMutationPort = { run: async operation => operation() }) {
  const harness = createWriterHarness()
  const model = new StagedFsaModel()
  const file = model.installFile(TARGET_NAME, harness.target.visible)
  // Native FSA may retain path identity across external replacement, so this
  // locator deliberately stays equal and leaves ownership decisions to bytes.
  const browserFile = { ...file, isSameEntry: async () => true } as unknown as FileSystemFileHandle
  const parent = model.parent as unknown as FileSystemDirectoryHandle
  const reads: { start: bigint; end: bigint }[] = []
  const fileSystem: DirectZipFileSystemPort<FileSystemDirectoryHandle, FileSystemFileHandle> = {
    queryPermission: () => model.queryPermission(),
    requestPermission: () => model.requestPermission(),
    lookupExactName: async (_parent, name) => await model.lookupExactName(model.parent, name) as Awaited<ReturnType<
      DirectZipFileSystemPort<FileSystemDirectoryHandle, FileSystemFileHandle>['lookupExactName']
    >>,
    createFile: async (_parent, name) => await model.createFile(model.parent, name) as unknown as FileSystemFileHandle,
    snapshot: async handle => {
      const snapshot = await model.snapshot(handle as unknown as StagedFileHandle)
      return { ...snapshot, read: async (start, end) => {
        reads.push({ start, end })
        return snapshot.read(start, end)
      } }
    },
    createWritable: (handle, keep) => model.createWritable(handle as unknown as StagedFileHandle, keep),
    removeExactName: (_parent, name) => model.removeExactName(model.parent, name),
  }
  const binding: BrowserDirectZipBinding = {
    operationId: harness.marker.operationId,
    candidateId: harness.marker.candidateId,
    resultRootComponent: 'root',
    stableName: TARGET_NAME,
    ownershipNonce: harness.marker.ownershipNonce,
    targetRef: harness.marker.bindingDigest,
    bindingDigest: harness.marker.bindingDigest,
    marker: harness.marker,
    parentBinding: { handleRef: 'parent', bindingDigest: harness.marker.bindingDigest, persistedHandle: parent },
    fileBinding: { handleRef: 'file', bindingDigest: harness.marker.bindingDigest, persistedHandle: browserFile },
    bootstrapPrefixLength: BigInt(harness.target.visible.length),
  }
  const reopen = () => new BrowserDirectZipTarget({
    binding, fileSystem, proofs: () => harness.pages.committedEpochProofs(),
    namespaceMutations,
  })
  const target = reopen()
  const observation = await target.observe(harness.checkpoint.epochRoot)
  const checkpoint = {
    ...harness.checkpoint,
    targetObservationDigest: decodeBase64Url(observation.digest)!,
  }
  let identity = 0
  const writer = (restored: DirectZipWriterCheckpointV1 = checkpoint, writerTarget = target) => new DirectZipEpochWriterV1({
    context: { ownershipMarker: harness.marker, rootComponent: 'root' },
    checkpoint: restored, pages: harness.pages, cuts: harness.cuts, target: writerTarget,
    identities: {
      nextEpochId: () => `epoch-${++identity}`,
      nextCandidateId: () => `candidate-${++identity}`,
    },
  })
  return { harness, model, file, fileSystem, reads, target, checkpoint, reopen, writer }
}

async function rollbackFixture(namespaceMutations?: DirectZipNamespaceMutationPort) {
  const fixture = await targetFixture(namespaceMutations)
  const firstWriter = fixture.writer()
  const first = await firstWriter.beginFile(fileAdmission(fixture.checkpoint,
    fileSource('completed-revision', BigInt(ROLLBACK_PAYLOAD_BYTES))))
  await first.write(ROLLBACK_PAYLOAD)
  await first.close()
  const retained = (await firstWriter.pause()).checkpoint
  const retainedProofs: DirectZipEpochProofV1[] = []
  for await (const proof of fixture.harness.pages.committedEpochProofs()) retainedProofs.push(proof)
  const secondWriter = fixture.writer(retained, fixture.reopen())
  const second = await secondWriter.beginFile(fileAdmission(retained,
    fileSource('changed-revision', BigInt(ROLLBACK_PAYLOAD_BYTES) * 2n)))
  await second.write(ROLLBACK_PAYLOAD)
  const previous = (await secondWriter.pause()).checkpoint
  const rollback: DirectZipWriterCheckpointV1 = { ...retained, generation: previous.generation + 1n }
  async function* proofs() { yield* retainedProofs }
  const target = fixture.reopen()
  return {
    ...fixture, target, previous, rollback, proofs,
    recover: () => target.recoverMemberRollback(previous, rollback, proofs),
    opened: () => fixture.model.calls.filter(call => call.startsWith('writable:')).length,
  }
}
