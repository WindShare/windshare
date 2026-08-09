import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  PORTABLE_HANDOFF_MAXIMUM_BYTES,
  PORTABLE_HANDOFF_MAXIMUM_PARTS,
  PORTABLE_HANDOFF_PART_BYTES,
  issuePortableArtifactAdmission,
  openPortableHandoff,
  type BrowserHandoffPublisher,
  type PortableArtifactAdmission,
} from '../../src/output/portable/browser-download'
import {
  DEFAULT_PORTABLE_ARTIFACT_LIMIT,
  DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES,
  DEFAULT_PORTABLE_MAXIMUM_PARTS,
  createOriginalFileArtifact,
  createPortableBinding,
  createPortableHandoffPlan,
  createReceiveIntent,
  createSelectionSpec,
  type ReceiveIntent,
} from '../../src/transfer/intent'

describe('bounded PortableHandoff structure', () => {
  it('derives the fixed assembly ceiling and part count from the canonical binding', async () => {
    const intent = await portableIntent()
    const session = await openPortableHandoff({
      intent,
      admission: admission(intent, DEFAULT_PORTABLE_ARTIFACT_LIMIT),
      attemptId: identity(8),
      publisher: downloadStartedPublisher(),
      assembly: { Blob, WritableStream },
    })
    const result = session.result.catch((error: unknown) => error)

    expect(PORTABLE_HANDOFF_MAXIMUM_BYTES).toBe(Number(DEFAULT_PORTABLE_ARTIFACT_LIMIT))
    expect(PORTABLE_HANDOFF_PART_BYTES).toBe(Number(DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES))
    expect(PORTABLE_HANDOFF_MAXIMUM_PARTS).toBe(Number(DEFAULT_PORTABLE_MAXIMUM_PARTS))
    expect(PORTABLE_HANDOFF_MAXIMUM_BYTES / PORTABLE_HANDOFF_PART_BYTES)
      .toBe(PORTABLE_HANDOFF_MAXIMUM_PARTS)
    expect(session.maximumArtifactBytes).toBe(DEFAULT_PORTABLE_ARTIFACT_LIMIT)

    await session.writable.abort()
    await expect(result).resolves.toMatchObject({ restartReason: 'portable-aborted' })
  })

  it('retains at most the cap-derived number of fixed-size parts', async () => {
    const intent = await portableIntent()
    const exactBytes = DEFAULT_PORTABLE_ASSEMBLY_PART_BYTES + 1n
    let maximumRetainedParts = 0
    const session = await openPortableHandoff({
      intent,
      admission: admission(intent, exactBytes),
      attemptId: identity(8),
      publisher: downloadStartedPublisher(),
      assembly: {
        Blob,
        WritableStream,
        observeAssembly: (snapshot) => {
          maximumRetainedParts = Math.max(maximumRetainedParts, snapshot.retainedParts)
        },
      },
    })
    const writer = session.writable.getWriter()
    await writer.write(new Uint8Array(PORTABLE_HANDOFF_PART_BYTES))
    await writer.write(Uint8Array.of(1))
    await writer.close()

    await expect(session.result).resolves.toMatchObject({ kind: 'download-started' })
    expect(maximumRetainedParts).toBe(2)
    expect(maximumRetainedParts).toBeLessThanOrEqual(PORTABLE_HANDOFF_MAXIMUM_PARTS)
  })
})

function downloadStartedPublisher(): BrowserHandoffPublisher {
  return {
    handoff: (request) => request.context.attemptKind === 'workspace'
      ? Object.freeze({
          kind: 'download-started',
          suggestedName: request.suggestedName,
          retryableUntil: request.context.retryableUntil,
        })
      : Object.freeze({
          kind: 'download-started',
          suggestedName: request.suggestedName,
        }),
  }
}

async function portableIntent(): Promise<ReceiveIntent> {
  const selection = await createSelectionSpec({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
  const artifact = await createOriginalFileArtifact({
    fileId: identity(3),
    sourcePath: 'root/bounded.bin',
    suggestedName: 'bounded.bin',
  })
  const portable = await createPortableBinding({
    operationId: identity(4),
    portablePlanId: identity(5),
    artifact,
  })
  const plan = await createPortableHandoffPlan(artifact, portable)
  return createReceiveIntent({ selection, artifact, plan })
}

function admission(intent: ReceiveIntent, exactArtifactBytes: bigint): PortableArtifactAdmission {
  return issuePortableArtifactAdmission({
    receiveIntentDigest: intent.digest,
    artifactDigest: intent.artifact.digest,
    sealedArtifact: {
      artifactKind: 'original-file',
      preparationManifestDigest: identity(9, 32),
    },
    exactArtifactBytes,
  })
}

function identity(seed: number, width = 16): string {
  const bytes = new Uint8Array(width)
  bytes[0] = seed
  bytes[bytes.length - 1] = seed ^ 0xff
  return encodeBase64Url(bytes)
}
