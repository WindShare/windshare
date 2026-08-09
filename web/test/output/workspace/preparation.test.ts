import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  createCompleteDirectoryResultRoot,
  createReceiveIntent,
  createSelectionSpec,
  createSyntheticSelectionResultRoot,
  createWorkspaceBinding,
  createWorkspaceThenPublishPlan,
  createZipArchiveArtifact,
} from '../../../src/transfer/intent'
import {
  decodePreparationManifestV1,
  sealWorkspaceZipPreparation,
  validateWorkspaceZipPreparation,
} from '../../../src/output/workspace/preparation'

describe('PreparationManifestV1', () => {
  it('seals authenticated generations, selected paths, exact sizes, pages, and ZIP layout together', async () => {
    const intent = await zipIntent()
    const input = preparationInput(intent)
    const sealed = await sealWorkspaceZipPreparation(input)

    expect(sealed.manifest).toEqual(expect.objectContaining({
      operationId: intent.operationId,
      receiveIntentDigest: intent.digest,
      artifactSpecDigest: intent.artifact.digest,
      entryCount: 2n,
      directoryCount: 1n,
      fileCount: 1n,
      selectedRawBytes: 3n,
    }))
    expect(sealed.zipLayout).toEqual(expect.objectContaining({
      receiveIntentDigest: intent.digest,
      artifactDigest: intent.artifact.digest,
      evidence: {
        kind: 'prepared',
        preparationManifestDigest: sealed.manifest.digest,
      },
    }))
    expect(sealed.manifest.canonicalMetadataBytes).toBe(
      BigInt(sealed.manifest.canonicalBytes.byteLength) +
      sealed.pages.reduce((total, page) => total + BigInt(page.canonicalBytes.byteLength), 0n) +
      BigInt(sealed.zipLayoutCanonicalBytes.byteLength),
    )
    const repeated = await sealWorkspaceZipPreparation(input)
    const resultRoots = sealed.manifest.entries.filter((entry) =>
      entry.kind === 'directory' && entry.role === 'result-root')
    expect(resultRoots).toHaveLength(1)
    expect(resultRoots[0]?.sourcePath).toEqual([])
    expect(sealed.manifest.entries.find((entry) => entry.kind === 'file')?.sourcePath)
      .toEqual(['file.bin'])
    expect(repeated.manifest.digest).toBe(sealed.manifest.digest)
    await expect(decodePreparationManifestV1(sealed.manifest.canonicalBytes, intent))
      .resolves.toEqual(sealed.manifest)
    await expect(validateWorkspaceZipPreparation(sealed, intent)).resolves.toEqual(sealed)

    await expect(decodePreparationManifestV1(
      Uint8Array.from([...sealed.manifest.canonicalBytes, 0]),
      intent,
    )).rejects.toThrow('trailing canonical bytes')
  })

  it('floors precision-3 timestamps at the ZIP millisecond boundary', async () => {
    const intent = await zipIntent()
    const input = preparationInput(intent)
    const withNanoseconds = input.entries.map(entry => entry.kind === 'file'
      ? {
          ...entry,
          modifiedTime: {
            seconds: 1_700_000_000n,
            nanoseconds: 123_456_789,
            precision: 3 as const,
          },
        }
      : entry)
    const withMilliseconds = input.entries.map(entry => entry.kind === 'file'
      ? {
          ...entry,
          modifiedTime: {
            seconds: 1_700_000_000n,
            nanoseconds: 123_000_000,
            precision: 3 as const,
          },
        }
      : entry)

    const precise = await sealWorkspaceZipPreparation({ ...input, entries: withNanoseconds })
    const truncated = await sealWorkspaceZipPreparation({ ...input, entries: withMilliseconds })
    const preciseFile = precise.zipLayout.entries.find(entry => entry.kind === 'file')
    const truncatedFile = truncated.zipLayout.entries.find(entry => entry.kind === 'file')

    expect(preciseFile).toEqual(expect.objectContaining({
      dosTime: truncatedFile?.dosTime,
      dosDate: truncatedFile?.dosDate,
    }))
    await expect(validateWorkspaceZipPreparation(precise, intent)).resolves.toEqual(precise)
  })

  it('reserves an empty source path for the authenticated synthetic result root', async () => {
    const intent = await zipIntent()
    const input = preparationInput(intent)
    const memberDirectoryId = identity(16, 11)
    const memberGeneration = identity(16, 12)
    if (intent.artifact.kind !== 'zip-archive') throw new TypeError('test intent lost ZIP artifact')
    const rootName = intent.artifact.layout.name
    const directoryIntent = await directoryZipIntent()
    const directoryInput = preparationInput(directoryIntent)
    const invalidEntries = [
      input.entries.map((entry) => entry.kind === 'file'
        ? { ...entry, sourcePath: [] }
        : entry),
      [
        ...input.entries,
        {
          kind: 'directory' as const,
          sourcePath: [],
          artifactPath: [rootName, 'member'],
          directoryId: memberDirectoryId,
          generation: memberGeneration,
          role: 'necessary-ancestor' as const,
        },
      ],
      directoryInput.entries.map((entry) => entry.kind === 'directory' &&
          entry.role === 'result-root'
        ? { ...entry, sourcePath: [] }
        : entry),
      input.entries.map((entry) => entry.kind === 'directory' && entry.role === 'result-root'
        ? { ...entry, sourcePath: ['fabricated-root'] }
        : entry),
    ]
    const generations = [
      input.generations,
      [...input.generations, { directoryId: memberDirectoryId, generation: memberGeneration }],
      directoryInput.generations,
      input.generations,
    ]

    for (const [index, entries] of invalidEntries.entries()) {
      const receiveIntent = index === 2 ? directoryIntent : intent
      await expect(sealWorkspaceZipPreparation({
        receiveIntent,
        preparationId: identity(16, 13 + index),
        generations: generations[index]!,
        entries,
      })).rejects.toMatchObject({
        reason: 'generation-mismatch',
      })
    }
  })

  it('fails before content when canonical metadata exceeds its explicit limit', async () => {
    const intent = await zipIntent()
    const rejected = sealWorkspaceZipPreparation({
      ...preparationInput(intent),
      metadataLimitBytes: 1n,
    })

    await expect(rejected).rejects.toMatchObject({
      reason: 'metadata-limit',
    })
  })

  it('rejects a changed selected size when validating the sealed bundle', async () => {
    const intent = await zipIntent()
    const sealed = await sealWorkspaceZipPreparation(preparationInput(intent))
    const entries = sealed.manifest.entries.map((entry) => entry.kind === 'file'
      ? { ...entry, exactSize: entry.exactSize + 1n }
      : entry)

    await expect(validateWorkspaceZipPreparation({
      ...sealed,
      manifest: { ...sealed.manifest, entries },
    }, intent)).rejects.toThrow(/canonical authority/u)
  })
})

function preparationInput(intent: Awaited<ReturnType<typeof zipIntent>>) {
  if (intent.artifact.kind !== 'zip-archive') throw new TypeError('test intent lost ZIP artifact')
  const layout = intent.artifact.layout
  const directoryId = layout.anchor.kind === 'synthetic-root'
    ? intent.selection.syntheticRoot
    : layout.anchor.directoryId
  const generation = identity(16, 8)
  const rootSourcePath = layout.anchor.kind === 'synthetic-root'
    ? []
    : layout.anchor.sourcePath.split('/')
  const rootName = layout.name
  return {
    receiveIntent: intent,
    preparationId: identity(16, 9),
    generations: [{ directoryId, generation }],
    entries: [
      {
        kind: 'directory' as const,
        sourcePath: rootSourcePath,
        artifactPath: [rootName],
        directoryId,
        generation,
        role: 'result-root' as const,
      },
      {
        kind: 'file' as const,
        sourcePath: [...rootSourcePath, 'file.bin'],
        artifactPath: [rootName, 'file.bin'],
        fileId: identity(16, 10),
        containingDirectoryId: directoryId,
        generation,
        exactSize: 3n,
      },
    ],
  }
}

async function directoryZipIntent() {
  const artifact = await createZipArchiveArtifact(
    createCompleteDirectoryResultRoot(identity(16, 7), 'folder'),
  )
  const workspace = await createWorkspaceBinding({
    operationId: identity(16, 4),
    workspaceId: identity(16, 5),
    artifact,
    repositoryRef: identity(32, 6),
  })
  const intent = await createReceiveIntent({
    selection: await createSelectionSpec({
      shareInstance: identity(16, 1),
      syntheticRoot: identity(16, 2),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    artifact,
    plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
  if (intent.artifact.kind !== 'zip-archive') throw new TypeError('test intent lost ZIP artifact')
  return intent
}

async function zipIntent() {
  const artifact = await createZipArchiveArtifact(createSyntheticSelectionResultRoot())
  const workspace = await createWorkspaceBinding({
    operationId: identity(16, 4),
    workspaceId: identity(16, 5),
    artifact,
    repositoryRef: identity(32, 6),
  })
  const intent = await createReceiveIntent({
    selection: await createSelectionSpec({
      shareInstance: identity(16, 1),
      syntheticRoot: identity(16, 2),
      rules: { mode: 'node-id', defaultSelected: true, rules: [] },
    }),
    artifact,
    plan: await createWorkspaceThenPublishPlan(artifact, workspace),
  })
  if (intent.artifact.kind !== 'zip-archive') throw new TypeError('test intent lost ZIP artifact')
  return intent
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
