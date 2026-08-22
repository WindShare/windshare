import { describe, expect, it } from 'vitest'
import {
  DirectZipMemberWriterV1,
  type DirectZipCompletionSealV1,
  type DirectZipWriterCheckpointV1,
} from '../../../../src/output/direct-zip/writer'
import {
  candidateObservation,
  createWriterHarness,
  fileAdmission,
  fileSource,
} from './fault-model'

const PAYLOAD = Uint8Array.of(1, 2, 3, 4, 5, 6)

describe('DirectZip member epochs', () => {
  it('treats a generic checkpoint as an observation when automatic evidence is absent', async () => {
    const harness = createWriterHarness()
    const writer = harness.writer()
    const member = await writer.beginFile(fileAdmission(harness.checkpoint))
    await member.write(PAYLOAD.subarray(0, 2))

    const observed = await writer.automaticCheckpoint()

    expect(observed.kind).toBe('unchanged')
    expect(observed.policyDecision).toMatchObject({
      kind: 'decline',
      reason: 'evidence-unavailable',
    })
    expect(observed.checkpoint.safeResumeBytes).toBe(0n)
    expect(harness.target.closeAttemptCount).toBe(0)
  })

  it.each([1, 3, 5])(
    'resumes an inside-member checkpoint from payload offset %i and its CRC state',
    async (resumeOffset) => {
    const harness = createWriterHarness()
    const firstWriter = harness.writer()
    const firstMember = await firstWriter.beginFile(fileAdmission(harness.checkpoint))
    await firstMember.write(PAYLOAD.subarray(0, resumeOffset))
    const paused = await firstWriter.pause()
    expect(paused.kind).toBe('advanced')
    expect(paused.checkpoint.phase).toBe('inside-member')
    expect(paused.checkpoint.member?.payloadOffset).toBe(BigInt(resumeOffset))
    const crcAtPause = paused.checkpoint.member?.crc32Accumulator

    const resumedWriter = harness.writer(paused.checkpoint)
    expect(resumedWriter.decideMemberResume(fileSource())).toEqual({
      kind: 'resume',
      payloadOffset: BigInt(resumeOffset),
      crc32Accumulator: crcAtPause,
    })
    const resumed = resumedWriter.resumeFile(fileSource())
    expect(resumed).toBeInstanceOf(DirectZipMemberWriterV1)
    await (resumed as DirectZipMemberWriterV1).write(PAYLOAD.subarray(resumeOffset))
    await (resumed as DirectZipMemberWriterV1).close()
    const completedMember = await resumedWriter.pause()

    expect(completedMember.kind).toBe('advanced')
    expect(completedMember.checkpoint.phase).toBe('between-members')
    expect(completedMember.checkpoint.safeResumeBytes).toBe(BigInt(PAYLOAD.byteLength))
    expect(completedMember.checkpoint.pages.centralRecordCount).toBe(2n)
    },
  )

  it('rolls back only the active member coordinate when source revision changes', async () => {
    const harness = createWriterHarness()
    const writer = harness.writer()
    const member = await writer.beginFile(fileAdmission(harness.checkpoint))
    await member.write(PAYLOAD.subarray(0, 3))
    const paused = await writer.pause()
    const restored = harness.writer(paused.checkpoint)

    expect(restored.resumeFile(fileSource('revision-2'))).toEqual({
      kind: 'rollback-member',
      archiveOffset: harness.checkpoint.archiveOffset,
      safeResumeBytes: 0n,
      nextEntryOrdinal: 1n,
      reason: 'revision-changed',
    })
    expect(harness.target.openEpochCount).toBe(1)
  })

  it('rejects restored member CRC and rollback authority that is not self-consistent', async () => {
    const harness = createWriterHarness()
    const writer = harness.writer()
    const member = await writer.beginFile(fileAdmission(harness.checkpoint))
    await member.write(PAYLOAD.subarray(0, 3))
    const paused = await writer.pause()
    const active = paused.checkpoint.member!

    expect(() => harness.writer({
      ...paused.checkpoint,
      member: { ...active, crc32Accumulator: 0x1_0000_0000 },
    })).toThrow('inside-member checkpoint coordinates are inconsistent')
    expect(() => harness.writer({
      ...paused.checkpoint,
      member: {
        ...active,
        rollback: { ...active.rollback, epochRoot: new Uint8Array(32).fill(0xee) },
      },
    })).toThrow('inside-member checkpoint coordinates are inconsistent')
  })

  it('forces pause checkpointing and reports the next reopen space upper bound', async () => {
    const harness = createWriterHarness()
    const writer = harness.writer()
    const member = await writer.beginFile(fileAdmission(harness.checkpoint))
    await member.write(PAYLOAD.subarray(0, 1))

    const paused = await writer.pause()

    expect(paused.kind).toBe('advanced')
    expect(paused.additionalTemporaryBytesUpperBound).toBe(paused.checkpoint.committedLength)
    expect(writer.resumeTemporarySpaceUpperBound).toBe(paused.checkpoint.committedLength)
  })

  it('allows automatic close only when an injected budget admits the next prefix copy', async () => {
    const harness = createWriterHarness()
    const writer = harness.writer(harness.checkpoint, {
      maximumPrefixCopyBytes: 10_000n,
      maximumCumulativePrefixCopyBytes: 20_000n,
      maximumModeledPeakTemporaryBytes: 10_000n,
    })
    const member = await writer.beginFile(fileAdmission(harness.checkpoint))
    await member.write(PAYLOAD.subarray(0, 1))

    const checkpoint = await writer.automaticCheckpoint()

    expect(checkpoint.kind).toBe('advanced')
    expect(checkpoint.policyDecision?.kind).toBe('admit')
    expect(harness.target.closeAttemptCount).toBe(1)
  })

  it('keeps writing after automatic advancement stops and lets an explicit pause cut the epoch', async () => {
    const harness = createWriterHarness()
    const writer = harness.writer(harness.checkpoint, {
      maximumPrefixCopyBytes: 0n,
      maximumCumulativePrefixCopyBytes: 0n,
      maximumModeledPeakTemporaryBytes: 0n,
    })
    const member = await writer.beginFile(fileAdmission(harness.checkpoint))
    await member.write(PAYLOAD.subarray(0, 1))

    const automatic = await writer.automaticCheckpoint()
    await member.write(PAYLOAD.subarray(1, 2))
    const paused = await writer.pause()

    expect(automatic.kind).toBe('unchanged')
    expect(automatic.policyDecision).toMatchObject({
      kind: 'decline',
      reason: 'prefix-copy-budget',
    })
    expect(paused.kind).toBe('advanced')
    expect(paused.additionalTemporaryBytesUpperBound).toBe(paused.checkpoint.committedLength)
    expect(harness.target.closeAttemptCount).toBe(1)
  })
})

describe('DirectZip epoch close cuts and candidate recovery', () => {
  it('retires and replays when close throws before publication', async () => {
    const harness = createWriterHarness()
    harness.target.closeFaults.push('before-publish')
    const writer = harness.writer()
    const member = await writer.beginFile(fileAdmission(harness.checkpoint))
    await member.write(PAYLOAD.subarray(0, 2))

    const paused = await writer.pause()

    expect(paused.kind).toBe('replay-required')
    expect(paused.checkpoint).toEqual(harness.checkpoint)
    expect(harness.cuts.retired).toHaveLength(1)
    expect(harness.cuts.promoted).toHaveLength(0)
    expect(BigInt(harness.target.visible.byteLength)).toBe(harness.checkpoint.committedLength)
  })

  it('reads back only the candidate range when close publishes and then throws', async () => {
    const harness = createWriterHarness()
    harness.target.closeFaults.push('after-publish')
    const writer = harness.writer()
    const member = await writer.beginFile(fileAdmission(harness.checkpoint))
    await member.write(PAYLOAD.subarray(0, 2))

    const paused = await writer.pause()

    expect(paused.kind).toBe('advanced')
    expect(harness.target.rangeReads).toEqual([{
      start: harness.checkpoint.committedLength,
      end: paused.checkpoint.committedLength,
    }])
    expect(harness.target.closeAttemptCount).toBe(1)
  })

  it('recovers a close-published candidate after the journal promotion cut fails', async () => {
    const harness = createWriterHarness()
    harness.cuts.failPromotionCount = 1
    const writer = harness.writer()
    const member = await writer.beginFile(fileAdmission(harness.checkpoint))
    await member.write(PAYLOAD.subarray(0, 2))
    await expect(writer.pause()).rejects.toThrow('injected journal promotion failure')
    const candidate = harness.cuts.staged.at(-1)!

    const recovered = await harness.writer(harness.checkpoint).recoverCandidate(candidate)

    expect(recovered.kind).toBe('promoted')
    expect(recovered.checkpoint.phase).toBe('inside-member')
    expect(harness.target.rangeReads).toHaveLength(1)
    expect(harness.target.closeAttemptCount).toBe(1)
  })

  it('refuses ambiguous ownership without promotion or truncation', async () => {
    const harness = createWriterHarness()
    harness.cuts.failPromotionCount = 1
    const writer = harness.writer()
    const member = await writer.beginFile(fileAdmission(harness.checkpoint))
    await member.write(PAYLOAD.subarray(0, 2))
    await expect(writer.pause()).rejects.toThrow('injected journal promotion failure')
    const candidate = harness.cuts.staged.at(-1)!
    harness.target.observationOverrides.push(candidateObservation({ ownership: 'ambiguous' }))

    await expect(harness.writer(harness.checkpoint).recoverCandidate(candidate)).rejects.toMatchObject({
      gate: 'target-verification-required',
    })
    expect(harness.target.truncateCount).toBe(0)
    expect(harness.cuts.promoted).toHaveLength(0)
  })

  it('does not promote same-length candidate bytes when slow digest readback differs', async () => {
    const harness = createWriterHarness()
    harness.cuts.failPromotionCount = 1
    const writer = harness.writer()
    const member = await writer.beginFile(fileAdmission(harness.checkpoint))
    await member.write(PAYLOAD.subarray(0, 2))
    await expect(writer.pause()).rejects.toThrow('injected journal promotion failure')
    const candidate = harness.cuts.staged.at(-1)!
    harness.target.failPromotionRead = true

    await expect(harness.writer(harness.checkpoint).recoverCandidate(candidate)).rejects.toMatchObject({
      gate: 'target-verification-required',
    })
    expect(harness.cuts.promoted).toHaveLength(0)
  })

  it('verifies committed epochs before truncating an unknown tail', async () => {
    const harness = createWriterHarness()
    harness.cuts.failPromotionCount = 1
    const writer = harness.writer()
    const member = await writer.beginFile(fileAdmission(harness.checkpoint))
    await member.write(PAYLOAD.subarray(0, 2))
    await expect(writer.pause()).rejects.toThrow('injected journal promotion failure')
    const candidate = harness.cuts.staged.at(-1)!
    harness.target.appendExternal(Uint8Array.of(0xaa, 0xbb))

    const resolved = await harness.writer(harness.checkpoint).recoverCandidate(candidate)

    expect(resolved.kind).toBe('replay')
    expect(harness.target.rangeReads).toEqual([{
      start: 0n,
      end: harness.checkpoint.committedLength,
    }])
    expect(harness.target.truncateCount).toBe(1)
    expect(BigInt(harness.target.visible.byteLength)).toBe(harness.checkpoint.committedLength)
  })
})

describe('DirectZip deterministic closing', () => {
  it('streams central records, proves exact completion, and never creates a second artifact', async () => {
    const { harness, writer, seal } = await preparedArchive()

    const completion = await writer.closeArchive(seal)

    expect(completion.exactArchiveBytes).toBe(BigInt(harness.target.visible.byteLength))
    expect(harness.target.rangeReads).toHaveLength(0)
    expect(harness.target.boundedCompletionReadCount).toBe(1)
    expect(harness.target.openEpochCount).toBe(2)
    expect(harness.target.closeAttemptCount).toBe(2)
    expect(harness.cuts.promoted.at(-1)?.completion).toBeDefined()
    expect(harness.target.artifactCount).toBe(1)
  })

  it('recovers an already-promoted completion without reopening or rewriting the target', async () => {
    const { harness, writer, seal } = await preparedArchive()
    const first = await writer.closeArchive(seal)
    const visible = Uint8Array.from(harness.target.visible)
    const openEpochCount = harness.target.openEpochCount
    const closeAttemptCount = harness.target.closeAttemptCount
    const boundedReadCount = harness.target.boundedCompletionReadCount

    const recovered = await harness.writer(first.checkpoint).closeArchive(seal)

    expect(recovered.checkpoint.completion).toEqual(first.checkpoint.completion)
    expect(recovered.exactArchiveBytes).toBe(first.exactArchiveBytes)
    expect(harness.target.visible).toEqual(visible)
    expect(harness.target.openEpochCount).toBe(openEpochCount)
    expect(harness.target.closeAttemptCount).toBe(closeAttemptCount)
    expect(harness.target.boundedCompletionReadCount).toBe(boundedReadCount + 1)
  })

  it('emits stable cut context while observer failures remain authority-neutral', async () => {
    const harness = createWriterHarness()
    const events: string[] = []
    const writer = harness.writer(harness.checkpoint, undefined, (event) => {
      events.push(`${event.operationId}:${event.kind}:${event.checkpointGeneration.toString()}`)
      if (event.kind === 'candidate-staged') throw new Error('diagnostic observer failed')
    })
    const member = await writer.beginFile(fileAdmission(harness.checkpoint))
    await member.write(PAYLOAD)
    await member.close()

    const paused = await writer.pause()

    expect(paused.kind).toBe('advanced')
    expect(events).toContain('operation-1:candidate-staged:1')
    expect(events).toContain('operation-1:checkpoint-promoted:2')
  })

  it('replays closing from the durable closing checkpoint after a central write fails', async () => {
    const { harness, writer, seal, memberCheckpoint } = await preparedArchive()
    harness.target.failNextWriteAtOrAfter = memberCheckpoint.committedLength
    await expect(writer.closeArchive(seal)).rejects.toThrow('injected positioned write failure')
    const closingCheckpoint = writer.committedCheckpoint
    expect(closingCheckpoint.phase).toBe('closing')
    expect(harness.target.abortCount).toBe(1)
    expect(BigInt(harness.target.visible.byteLength)).toBe(memberCheckpoint.committedLength)

    const completion = await harness.writer(closingCheckpoint).closeArchive(seal)

    expect(completion.checkpoint.phase).toBe('closing')
    expect(harness.cuts.closing).toHaveLength(1)
    expect(BigInt(harness.target.visible.byteLength)).toBe(completion.exactArchiveBytes)
  })

  it('resolves a closing close-after-publication cut with range proof plus bounded proof', async () => {
    const { harness, writer, seal } = await preparedArchive()
    harness.target.closeFaults.push('after-publish')

    const completion = await writer.closeArchive(seal)

    expect(completion.checkpoint.phase).toBe('closing')
    expect(harness.target.rangeReads).toHaveLength(1)
    expect(harness.target.boundedCompletionReadCount).toBe(1)
  })

  it('retains the closing candidate when exact tail validation fails, then promotes on recovery', async () => {
    const { harness, writer, seal, memberCheckpoint } = await preparedArchive()
    harness.target.corruptClosingTail = true
    await expect(writer.closeArchive(seal)).rejects.toThrow()
    const candidate = harness.cuts.staged.at(-1)!
    expect(candidate.kind).toBe('closing')
    expect(harness.cuts.promoted).toHaveLength(1)

    harness.target.corruptClosingTail = false
    const closingCheckpoint = harness.cuts.closing.at(-1)!
    const recovered = await harness.writer(closingCheckpoint).recoverCandidate(candidate, seal)

    expect(recovered.kind).toBe('promoted')
    expect(harness.cuts.promoted).toHaveLength(2)
    expect(harness.cuts.promoted.at(-1)?.completion).toBeDefined()
    expect(memberCheckpoint.phase).toBe('between-members')
  })
})

async function preparedArchive(): Promise<{
  readonly harness: ReturnType<typeof createWriterHarness>
  readonly writer: ReturnType<ReturnType<typeof createWriterHarness>['writer']>
  readonly memberCheckpoint: DirectZipWriterCheckpointV1
  readonly seal: DirectZipCompletionSealV1
}> {
  const harness = createWriterHarness()
  const writer = harness.writer()
  const member = await writer.beginFile(fileAdmission(harness.checkpoint))
  await member.write(PAYLOAD)
  await member.close()
  const paused = await writer.pause()
  if (paused.kind !== 'advanced') throw new Error('test member checkpoint did not advance')
  const pages = await harness.pages.snapshot()
  const seal: DirectZipCompletionSealV1 = Object.freeze({
    entryCount: pages.layoutRecordCount,
    centralDirectoryBytes: pages.centralBytes,
    layoutRoot: pages.layoutRoot,
    centralRoot: pages.centralRoot,
    preClosingEpochRoot: paused.checkpoint.epochRoot,
  })
  return { harness, writer, memberCheckpoint: paused.checkpoint, seal }
}
