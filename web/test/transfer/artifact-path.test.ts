import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  createCatalogRootDirectoryTreeArtifact,
  createCompleteDirectoryResultRoot,
  createDirectTreePlan,
  createFSANamedEntryReservation,
  createNativeContainerRootReservation,
  createNativeNamedEntryReservation,
  createReceiveIntent,
  createResultRootDirectoryTreeArtifact,
  createSelectionSpec,
  createSingleFileDirectoryTreeArtifact,
  createSyntheticSelectionResultRoot,
  type DirectoryTreeArtifact,
  type ReceiveIntent,
} from '../../src/transfer/intent'
import {
  artifactDirectoryPath,
  artifactFilePath,
  directoryIsResultRoot,
} from '../../src/transfer/job/artifact-path'
import { createDirectTreeCoordinateContract } from '../../src/transfer/job/coordinate/direct-tree'
import { identityText } from './v2-job-fixture'

describe('artifact path projection', () => {
  it('keeps anchored logical paths unchanged for ZIP and prepared output', () => {
    const layout = createCompleteDirectoryResultRoot(identityText(8), 'parent/photos')
    const intent = {
      artifact: { kind: 'zip-archive', layout },
    } as unknown as ReceiveIntent

    expect(artifactDirectoryPath(intent, ['parent'])).toEqual([])
    expect(artifactDirectoryPath(intent, ['parent', 'photos'])).toEqual([layout.name])
    expect(artifactDirectoryPath(intent, ['parent', 'photos', 'nested']))
      .toEqual([layout.name, 'nested'])
    expect(artifactFilePath(intent, ['parent', 'photos', 'image.jpg']))
      .toEqual([layout.name, 'image.jpg'])
    expect(directoryIsResultRoot(intent, ['parent', 'photos'])).toBe(true)
  })

  it('projects a single file without inventing directory materialization', async () => {
    const artifact = await createSingleFileDirectoryTreeArtifact({
      fileId: identityText(20),
      sourcePath: 'parent/report.bin',
      outputName: 'report.bin',
    })
    const contract = await fsaContract(artifact)

    expect(contract.rootExpectation).toEqual({ kind: 'none', anchorKind: 'single-file' })
    expect(contract.projectDirectory([])).toMatchObject({
      kind: 'reference',
      sourceAuthenticationPath: [],
      logicalArtifactPath: [],
    })
    expect(contract.projectDirectory(['parent'])).toMatchObject({
      kind: 'reference',
      sourceAuthenticationPath: ['parent'],
      logicalArtifactPath: [],
    })
    expect(contract.projectFile(['parent', 'report.bin'])).toEqual({
      sourceAuthenticationPath: ['parent', 'report.bin'],
      logicalArtifactPath: ['report.bin'],
      relativePath: [],
    })

    const selection = await selectionSpec()
    const nativeReservation = await createNativeNamedEntryReservation({
      operationId: identityText(43),
      reservationId: identityText(44),
      artifact,
      authorityRef: opaqueIdentity(45),
      logicalReservedName: 'report.bin',
      collisionIndex: 0,
    })
    const native = await createDirectTreeCoordinateContract(await createReceiveIntent({
      selection,
      artifact,
      plan: await createDirectTreePlan(artifact, nativeReservation),
    }))
    expect(native.projectFile(['parent', 'report.bin']).relativePath).toEqual(['report.bin'])
    expect(() => contract.projectDirectory(['unrelated'])).toThrow(/single-file ancestry/u)
  })

  it('rebases a directory anchor exactly once at the reserved FSA root', async () => {
    const anchorId = identityText(30)
    const artifact = await createResultRootDirectoryTreeArtifact(
      createCompleteDirectoryResultRoot(anchorId, 'parent/photos'),
    )
    const contract = await fsaContract(artifact)

    expect(contract.rootExpectation).toEqual({
      kind: 'materialized-directory',
      anchorKind: 'directory',
      directoryId: anchorId,
      relativePath: [],
    })
    expect(contract.projectDirectory(['parent'])).toMatchObject({
      kind: 'reference',
      logicalArtifactPath: [],
    })
    expect(contract.projectDirectory(['parent', 'photos'])).toEqual({
      kind: 'materialize',
      sourceAuthenticationPath: ['parent', 'photos'],
      logicalArtifactPath: ['photos'],
      relativePath: [],
    })
    expect(contract.projectDirectory(['parent', 'photos', 'nested'])).toEqual({
      kind: 'materialize',
      sourceAuthenticationPath: ['parent', 'photos', 'nested'],
      logicalArtifactPath: ['photos', 'nested'],
      relativePath: ['nested'],
    })
    expect(contract.projectFile(['parent', 'photos', 'image.jpg'])).toEqual({
      sourceAuthenticationPath: ['parent', 'photos', 'image.jpg'],
      logicalArtifactPath: ['photos', 'image.jpg'],
      relativePath: ['image.jpg'],
    })
    expect(() => contract.projectDirectory(['other'])).toThrow(/escapes|outside/u)
    expect(() => contract.projectFile(['parent', 'escape.jpg'])).toThrow(/escapes/u)
    expect(() => contract.projectFile(['parent', 'photos'])).toThrow(/relative file/u)
  })

  it('rebases a synthetic result root while preserving its logical diagnostic name', async () => {
    const artifact = await createResultRootDirectoryTreeArtifact(
      createSyntheticSelectionResultRoot(),
    )
    const contract = await fsaContract(artifact)

    expect(contract.rootExpectation).toEqual({
      kind: 'materialized-directory',
      anchorKind: 'synthetic-root',
      directoryId: identityText(2),
      relativePath: [],
    })
    expect(contract.projectDirectory([])).toEqual({
      kind: 'materialize',
      sourceAuthenticationPath: [],
      logicalArtifactPath: ['windshare'],
      relativePath: [],
    })
    expect(contract.projectFile(['payload.bin'])).toEqual({
      sourceAuthenticationPath: ['payload.bin'],
      logicalArtifactPath: ['windshare', 'payload.bin'],
      relativePath: ['payload.bin'],
    })
    expect(() => contract.projectFile([])).toThrow(/relative file/u)
  })

  it('keeps native named roots container-relative instead of applying the FSA rebase', async () => {
    const artifact = await createResultRootDirectoryTreeArtifact(
      createSyntheticSelectionResultRoot(),
    )
    const selection = await selectionSpec()
    const reservation = await createNativeNamedEntryReservation({
      operationId: identityText(45),
      reservationId: identityText(46),
      artifact,
      authorityRef: opaqueIdentity(47),
      logicalReservedName: 'windshare',
      collisionIndex: 0,
    })
    const intent = await createReceiveIntent({
      selection,
      artifact,
      plan: await createDirectTreePlan(artifact, reservation),
    })
    const contract = await createDirectTreeCoordinateContract(intent)

    expect(contract.coordinate).toBe('native-container-relative')
    expect(contract.rootExpectation).toMatchObject({ relativePath: ['windshare'] })
    expect(contract.projectDirectory([])).toMatchObject({ relativePath: ['windshare'] })
    expect(contract.projectFile(['payload.bin'])).toMatchObject({
      logicalArtifactPath: ['windshare', 'payload.bin'],
      relativePath: ['windshare', 'payload.bin'],
    })
  })

  it('models native catalog-root coordinates explicitly', async () => {
    const artifact = await createCatalogRootDirectoryTreeArtifact()
    const selection = await selectionSpec()
    const reservation = await createNativeContainerRootReservation({
      operationId: identityText(50),
      reservationId: identityText(51),
      artifact,
      authorityRef: opaqueIdentity(52),
    })
    const intent = await createReceiveIntent({
      selection,
      artifact,
      plan: await createDirectTreePlan(artifact, reservation),
    })
    const contract = await createDirectTreeCoordinateContract(intent)

    expect(contract.coordinate).toBe('native-container-relative')
    expect(contract.rootExpectation).toEqual({
      kind: 'materialized-directory',
      anchorKind: 'catalog-root',
      directoryId: identityText(2),
      relativePath: [],
    })
    expect(contract.projectDirectory([])).toMatchObject({ kind: 'materialize', relativePath: [] })
    expect(() => contract.projectFile([])).toThrow(/relative file/u)
    expect(contract.projectFile(['docs', 'report.bin'])).toMatchObject({
      logicalArtifactPath: ['docs', 'report.bin'],
      relativePath: ['docs', 'report.bin'],
    })
  })
})

async function fsaContract(
  artifact: DirectoryTreeArtifact,
): Promise<Awaited<ReturnType<typeof createDirectTreeCoordinateContract>>> {
  const selection = await selectionSpec()
  if (artifact.layout.kind === 'catalog-root') {
    throw new TypeError('FSA named-entry fixture requires a named artifact')
  }
  const reservedName = artifact.layout.kind === 'single-file'
    ? artifact.layout.outputName
    : artifact.layout.root.name
  const reservation = await createFSANamedEntryReservation({
    operationId: identityText(40),
    reservationId: identityText(41),
    artifact,
    authorityRef: opaqueIdentity(42),
    logicalReservedName: reservedName,
    physicalName: reservedName,
    collisionIndex: 0,
  })
  const intent = await createReceiveIntent({
    selection,
    artifact,
    plan: await createDirectTreePlan(artifact, reservation),
  })
  return createDirectTreeCoordinateContract(intent)
}

function selectionSpec() {
  return createSelectionSpec({
    shareInstance: identityText(1),
    syntheticRoot: identityText(2),
    rules: { mode: 'node-id', defaultSelected: true, rules: [] },
  })
}

function opaqueIdentity(seed: number): string {
  const value = new Uint8Array(32)
  value[0] = seed
  value[value.length - 1] = seed ^ 0xff
  return encodeBase64Url(value)
}
