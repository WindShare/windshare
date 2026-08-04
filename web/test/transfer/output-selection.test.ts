import { describe, expect, it } from 'vitest'

import type { V2CatalogEntry, V2CatalogModifiedTime } from '../../src/catalog/v2-records'
import { V2SelectionPolicy } from '../../src/catalog/v2-selection'
import {
  createV2OutputSelection,
  V2OutputSelectionPlan,
  type V2OutputSelectionDirectory,
  type V2OutputSelectionFile,
} from '../../src/transfer/output-selection'
import { b64ToBytes, loadVectorFile, type VectorCase } from '../vectors'

interface CanonicalSelectionVector extends VectorCase {
  readonly shareInstanceB64: string
  readonly syntheticRootB64: string
  readonly rootGenerationB64: string
  readonly directoryIdB64: string
  readonly directoryGenerationB64: string
  readonly fileIdB64: string
  readonly defaultSelected: boolean
  readonly modifiedSeconds: string
  readonly modifiedNanoseconds: number
  readonly modifiedPrecision: 1 | 2 | 3
  readonly expectedSize: string
  readonly canonicalRequestB64: string
  readonly canonicalSelectionB64: string
  readonly selectionIdentityB64: string
  readonly resumeIntentB64: string
}

const semantics = loadVectorFile(
  new URL('../../../core/testvectors/v2-semantics.json', import.meta.url),
)
const vector = semantics.cases.find(
  (candidate) => candidate.name === 'canonical-selection-v1',
) as CanonicalSelectionVector | undefined

describe('v2 canonical output selection', () => {
  it('matches the Go CanonicalSelectionV1, SelectionIdentity, and ResumeIntent bytes', async () => {
    if (vector === undefined) throw new Error('canonical selection vector is missing')
    const fixture = vectorFixture(vector)

    const selection = await createV2OutputSelection(
      fixture.descriptor,
      fixture.policy.snapshot(),
      fixture.rootGeneration,
      [fixture.directory],
      [fixture.file],
    )

    const canonicalRequest = bytes(vector.canonicalRequestB64)
    expect(selection.canonicalSelection.slice(0, canonicalRequest.byteLength))
      .toEqual(canonicalRequest)
    expect(selection.canonicalSelection).toEqual(bytes(vector.canonicalSelectionB64))
    expect(selection.selectionIdentity).toEqual(bytes(vector.selectionIdentityB64))
    expect(selection.resumeIntent).toEqual(bytes(vector.resumeIntentB64))
  })

  it('moves a changed terminal selection into a different recovery namespace', async () => {
    if (vector === undefined) throw new Error('canonical selection vector is missing')
    const fixture = vectorFixture(vector)
    const original = await createV2OutputSelection(
      fixture.descriptor,
      fixture.policy.snapshot(),
      fixture.rootGeneration,
      [fixture.directory],
      [fixture.file],
    )
    const changed = await createV2OutputSelection(
      fixture.descriptor,
      fixture.policy.snapshot(),
      fixture.rootGeneration,
      [fixture.directory],
      [{ ...fixture.file, expectedSize: fixture.file.expectedSize + 1n }],
    )

    expect(changed.selectionIdentityText).not.toBe(original.selectionIdentityText)
    expect(changed.resumeIntentText).not.toBe(original.resumeIntentText)
  })

  it('claims catalog identities across the entire discovery tree', () => {
    if (vector === undefined) throw new Error('canonical selection vector is missing')
    const fixture = vectorFixture(vector)
    const plan = new V2OutputSelectionPlan()

    plan.claimNode(fixture.directory.directoryId)

    expect(() => plan.claimNode(fixture.directory.directoryId.slice()))
      .toThrow('Selection discovery repeats an opaque node identity')
  })

  it('rejects selected records that reuse root or cross-kind identities', async () => {
    if (vector === undefined) throw new Error('canonical selection vector is missing')
    const fixture = vectorFixture(vector)

    await expect(createV2OutputSelection(
      fixture.descriptor,
      fixture.policy.snapshot(),
      fixture.rootGeneration,
      [fixture.directory],
      [{ ...fixture.file, fileId: fixture.directory.directoryId }],
    )).rejects.toThrow('Selection plan repeats an opaque node identity')

    await expect(createV2OutputSelection(
      fixture.descriptor,
      fixture.policy.snapshot(),
      fixture.rootGeneration,
      [{ ...fixture.directory, directoryId: fixture.descriptor.syntheticRoot }],
      [],
    )).rejects.toThrow('Selection plan repeats an opaque node identity')
  })
})

function vectorFixture(value: CanonicalSelectionVector) {
  const shareInstance = bytes(value.shareInstanceB64)
  const syntheticRoot = bytes(value.syntheticRootB64)
  const directoryId = bytes(value.directoryIdB64)
  const fileId = bytes(value.fileIdB64)
  const rootGeneration = bytes(value.rootGenerationB64)
  const directoryGeneration = bytes(value.directoryGenerationB64)
  const modifiedTime: V2CatalogModifiedTime = Object.freeze({
    seconds: BigInt(value.modifiedSeconds),
    nanoseconds: value.modifiedNanoseconds,
    precision: value.modifiedPrecision,
    milliseconds: BigInt(value.modifiedSeconds) * 1_000n +
      BigInt(value.modifiedNanoseconds / 1_000_000),
  })
  const directoryEntry: Extract<V2CatalogEntry, { kind: 'directory' }> = Object.freeze({
    kind: 'directory', id: directoryId, idText: 'directory', name: 'photos', modifiedTime,
  })
  const fileEntry: Extract<V2CatalogEntry, { kind: 'file' }> = Object.freeze({
    kind: 'file', id: fileId, idText: 'file', name: 'readme.txt',
    expectedSize: BigInt(value.expectedSize), modifiedTime,
  })
  const policy = new V2SelectionPolicy(value.defaultSelected)
  policy.toggle(fileEntry, ['root', 'directory'])
  policy.toggle(directoryEntry, ['root'])
  const directory: V2OutputSelectionDirectory = Object.freeze({
    path: Object.freeze(['photos']),
    directoryId,
    generation: directoryGeneration,
    modifiedTime,
  })
  const file: V2OutputSelectionFile = Object.freeze({
    path: Object.freeze(['photos', 'readme.txt']),
    fileId,
    parentDirectoryId: directoryId,
    parentGeneration: directoryGeneration,
    expectedSize: BigInt(value.expectedSize),
    modifiedTime,
  })
  return {
    descriptor: { shareInstance, syntheticRoot } as never,
    policy,
    rootGeneration,
    directory,
    file,
  }
}

function bytes(encoded: string): Uint8Array<ArrayBuffer> {
  return Uint8Array.from(b64ToBytes(encoded))
}
