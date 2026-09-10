import { describe, expect, it } from 'vitest'
import { decodeBase64Url } from '../../src/crypto/bytes'
import { DirectZipEpochWriterV1 } from '../../src/output/direct-zip/writer'
import type { DirectZipWriterCheckpointV1 } from '../../src/output/direct-zip/writer'
import type { DirectZipFileSystemPort } from '../../src/output/direct-zip/target'
import { BrowserDirectZipTarget, type BrowserDirectZipBinding } from '../../src/ui/browser-receive/direct-zip/target'
import { StagedFsaModel, type StagedFileHandle } from '../output/direct-zip/target/staged-fsa-model'
import { createWriterHarness, fileAdmission, fileSource } from '../output/direct-zip/writer/fault-model'

const TARGET_NAME = 'root.windshare-owned.zip'
const PAYLOAD_BYTES = 65_536
const PAYLOAD = new Uint8Array(PAYLOAD_BYTES).fill(0xa7)

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
      preClosingEpochRoot: saved.checkpoint.epochRoot,
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

async function targetFixture() {
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
