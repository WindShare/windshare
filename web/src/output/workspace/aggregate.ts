import { BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS } from '../../transfer/intent'
import { encodeBase64Url } from '../../crypto/bytes'
import {
  assertCanonicalRecordDomain,
  canonicalDigest,
  canonicalFrame,
  canonicalIdentity,
  canonicalRecord,
  canonicalText,
  canonicalU8,
  canonicalU64,
  concatCanonicalBytes,
  equalCanonicalBytes,
  snapshotCanonicalBytes,
  snapshotIdentity,
  type CanonicalBytes,
} from './canonical'
import { type PreparationBinding } from './manifest'

interface AggregateRecordBase {
  readonly schemaVersion: 1
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly canonicalBytes: CanonicalBytes
  readonly digest: string
}

const PACKAGED_ARTIFACT_DOMAIN = 'windshare/packaged-artifact/v1'
const TEXT_ENCODER = new TextEncoder()

export interface SealedMaterializationV1 extends AggregateRecordBase {
  readonly workspaceBindingDigest: string
  readonly preparationBinding: PreparationBinding
  readonly materializedManifestDigest: string
  readonly generationTableDigest: string
  readonly artifactVersion: number
  readonly layoutVersion: number
  readonly rawWorkspaceReceiptDigest: string
}

export interface PackagedArtifactV1 extends AggregateRecordBase {
  readonly sealedMaterializationDigest: string
  readonly artifactSpecDigest: string
  readonly packageOwnedObjectId: string
  readonly exactBytes: bigint
  readonly artifactReceiptDigest: string
  readonly layoutDigest: string
}

export type PublicationAttemptRoute =
  | Readonly<{ kind: 'managed'; reservationDigest: string }>
  | Readonly<{
      kind: 'handoff'
      suggestedName: string
      packagedFileSupported: boolean
      objectUrlLeaseMilliseconds: typeof BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS
    }>

export interface PublicationAttemptV1 extends AggregateRecordBase {
  readonly publicationAttemptId: string
  readonly packagedArtifactDigest: string
  readonly route: PublicationAttemptRoute
}

export async function sealWorkspaceMaterialization(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly workspaceBindingDigest: string
  readonly preparationBinding: PreparationBinding
  readonly materializedManifestDigest: string
  readonly generationTableDigest: string
  readonly artifactVersion: number
  readonly layoutVersion: number
  readonly rawWorkspaceReceiptDigest: string
}): Promise<SealedMaterializationV1> {
  const identity = snapshotAggregateIdentity(input)
  const workspaceBindingDigest = digest(input.workspaceBindingDigest, 'workspace binding digest')
  const materializedManifestDigest = digest(
    input.materializedManifestDigest,
    'materialized manifest digest',
  )
  const generationTableDigest = digest(input.generationTableDigest, 'generation table digest')
  const rawWorkspaceReceiptDigest = digest(
    input.rawWorkspaceReceiptDigest,
    'raw workspace receipt digest',
  )
  const preparationBinding = snapshotPreparationBinding(input.preparationBinding)
  const canonicalBytes = canonicalRecord('windshare/sealed-materialization/v1', 1, [
    canonicalFrame(canonicalIdentity(identity.operationId, 16, 'operation ID')),
    canonicalFrame(canonicalIdentity(identity.receiveIntentDigest, 32, 'receive intent digest')),
    canonicalFrame(canonicalIdentity(workspaceBindingDigest, 32, 'workspace binding digest')),
    canonicalFrame(canonicalPreparationBinding(preparationBinding)),
    canonicalFrame(canonicalIdentity(materializedManifestDigest, 32, 'manifest digest')),
    canonicalFrame(canonicalIdentity(generationTableDigest, 32, 'generation table digest')),
    canonicalFrame(canonicalU8(input.artifactVersion)),
    canonicalFrame(canonicalU8(input.layoutVersion)),
    canonicalFrame(canonicalIdentity(rawWorkspaceReceiptDigest, 32, 'workspace receipt digest')),
  ])
  return Object.freeze({
    schemaVersion: 1,
    ...identity,
    workspaceBindingDigest,
    preparationBinding,
    materializedManifestDigest,
    generationTableDigest,
    artifactVersion: input.artifactVersion,
    layoutVersion: input.layoutVersion,
    rawWorkspaceReceiptDigest,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

/** Rebuilds a sealed materialization from its durable aggregate record. */
export async function decodeSealedMaterializationV1(
  input: Uint8Array,
): Promise<SealedMaterializationV1> {
  const canonicalBytes = snapshotCanonicalBytes(input)
  assertCanonicalRecordDomain(canonicalBytes, 'windshare/sealed-materialization/v1', 1)
  const reader = new AggregateReader(canonicalBytes, 'windshare/sealed-materialization/v1')
  const rebuilt = await sealWorkspaceMaterialization({
    operationId: reader.identity(16, 'operation ID'),
    receiveIntentDigest: reader.identity(32, 'receive intent digest'),
    workspaceBindingDigest: reader.identity(32, 'workspace binding digest'),
    preparationBinding: decodeAggregatePreparationBinding(reader.frame('preparation binding')),
    materializedManifestDigest: reader.identity(32, 'manifest digest'),
    generationTableDigest: reader.identity(32, 'generation table digest'),
    artifactVersion: reader.u8('artifact version'),
    layoutVersion: reader.u8('layout version'),
    rawWorkspaceReceiptDigest: reader.identity(32, 'workspace receipt digest'),
  })
  reader.end()
  if (!equalCanonicalBytes(rebuilt.canonicalBytes, canonicalBytes)) {
    throw new TypeError('sealed materialization canonical authority changed')
  }
  return rebuilt
}

export async function sealPackagedArtifact(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly sealedMaterializationDigest: string
  readonly artifactSpecDigest: string
  readonly packageOwnedObjectId: string
  readonly exactBytes: bigint
  readonly artifactReceiptDigest: string
  readonly layoutDigest: string
}): Promise<PackagedArtifactV1> {
  const identity = snapshotAggregateIdentity(input)
  const sealedMaterializationDigest = digest(input.sealedMaterializationDigest, 'seal digest')
  const artifactSpecDigest = digest(input.artifactSpecDigest, 'artifact spec digest')
  const packageOwnedObjectId = digest(input.packageOwnedObjectId, 'package owned object ID')
  const artifactReceiptDigest = digest(input.artifactReceiptDigest, 'artifact receipt digest')
  const layoutDigest = digest(input.layoutDigest, 'layout digest')
  const exactBytes = u64(input.exactBytes, 'package byte length')
  const canonicalBytes = canonicalRecord(PACKAGED_ARTIFACT_DOMAIN, 1, [
    canonicalFrame(canonicalIdentity(identity.operationId, 16, 'operation ID')),
    canonicalFrame(canonicalIdentity(identity.receiveIntentDigest, 32, 'receive intent digest')),
    canonicalFrame(canonicalIdentity(sealedMaterializationDigest, 32, 'seal digest')),
    canonicalFrame(canonicalIdentity(artifactSpecDigest, 32, 'artifact digest')),
    canonicalFrame(canonicalIdentity(packageOwnedObjectId, 32, 'owned object ID')),
    canonicalFrame(canonicalU64(exactBytes)),
    canonicalFrame(canonicalIdentity(artifactReceiptDigest, 32, 'artifact receipt digest')),
    canonicalFrame(canonicalIdentity(layoutDigest, 32, 'layout digest')),
  ])
  return Object.freeze({
    schemaVersion: 1,
    ...identity,
    sealedMaterializationDigest,
    artifactSpecDigest,
    packageOwnedObjectId,
    exactBytes,
    artifactReceiptDigest,
    layoutDigest,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

/** Rehydrates the package authority from its canonical repository record after a reload. */
export async function decodePackagedArtifactV1(input: Uint8Array): Promise<PackagedArtifactV1> {
  const canonicalBytes = snapshotCanonicalBytes(input)
  assertCanonicalRecordDomain(canonicalBytes, PACKAGED_ARTIFACT_DOMAIN, 1)
  const reader = new AggregateReader(canonicalBytes, PACKAGED_ARTIFACT_DOMAIN)
  const artifact = await sealPackagedArtifact({
    operationId: reader.identity(16, 'operation ID'),
    receiveIntentDigest: reader.identity(32, 'receive intent digest'),
    sealedMaterializationDigest: reader.identity(32, 'seal digest'),
    artifactSpecDigest: reader.identity(32, 'artifact digest'),
    packageOwnedObjectId: reader.identity(32, 'owned object ID'),
    exactBytes: reader.u64('package byte length'),
    artifactReceiptDigest: reader.identity(32, 'artifact receipt digest'),
    layoutDigest: reader.identity(32, 'layout digest'),
  })
  reader.end()
  if (!equalCanonicalBytes(artifact.canonicalBytes, canonicalBytes)) {
    throw new TypeError('packaged artifact canonical authority changed')
  }
  return artifact
}

export async function createPublicationAttempt(input: {
  readonly publicationAttemptId: string
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly packagedArtifactDigest: string
  readonly route: PublicationAttemptRoute
}): Promise<PublicationAttemptV1> {
  const identity = snapshotAggregateIdentity(input)
  const publicationAttemptId = snapshotIdentity(
    input.publicationAttemptId,
    16,
    'publication attempt ID',
  )
  const packagedArtifactDigest = digest(input.packagedArtifactDigest, 'package digest')
  const route = snapshotPublicationRoute(input.route)
  const canonicalBytes = canonicalRecord('windshare/publication-attempt/v1', 1, [
    canonicalFrame(canonicalIdentity(publicationAttemptId, 16, 'publication attempt ID')),
    canonicalFrame(canonicalIdentity(identity.operationId, 16, 'operation ID')),
    canonicalFrame(canonicalIdentity(packagedArtifactDigest, 32, 'package digest')),
    canonicalFrame(canonicalPublicationRoute(route)),
  ])
  return Object.freeze({
    schemaVersion: 1,
    ...identity,
    publicationAttemptId,
    packagedArtifactDigest,
    route,
    canonicalBytes,
    digest: await canonicalDigest(canonicalBytes),
  })
}

function snapshotAggregateIdentity(input: {
  readonly operationId: string
  readonly receiveIntentDigest: string
}): { readonly operationId: string; readonly receiveIntentDigest: string } {
  return Object.freeze({
    operationId: snapshotIdentity(input.operationId, 16, 'operation ID'),
    receiveIntentDigest: digest(input.receiveIntentDigest, 'receive intent digest'),
  })
}

function snapshotPreparationBinding(binding: PreparationBinding): PreparationBinding {
  return binding.kind === 'absent'
    ? Object.freeze({ kind: 'absent' })
    : Object.freeze({
        kind: 'present',
        preparationDigest: digest(binding.preparationDigest, 'preparation digest'),
      })
}

function canonicalPreparationBinding(binding: PreparationBinding): CanonicalBytes {
  return binding.kind === 'absent'
    ? canonicalU8(2)
    : concatCanonicalBytes([
        canonicalU8(1),
        canonicalFrame(canonicalIdentity(binding.preparationDigest, 32, 'preparation digest')),
      ])
}

function decodeAggregatePreparationBinding(bytes: Uint8Array): PreparationBinding {
  const reader = new AggregateReader(bytes)
  const discriminant = reader.rawU8('preparation binding discriminant')
  if (discriminant === 2) {
    reader.end()
    return Object.freeze({ kind: 'absent' })
  }
  if (discriminant !== 1) throw new TypeError('preparation binding discriminant is invalid')
  const preparationDigest = reader.identity(32, 'preparation digest')
  reader.end()
  return Object.freeze({ kind: 'present', preparationDigest })
}

function snapshotPublicationRoute(route: PublicationAttemptRoute): PublicationAttemptRoute {
  if (route.kind === 'managed') {
    return Object.freeze({
      kind: 'managed',
      reservationDigest: digest(route.reservationDigest, 'reservation digest'),
    })
  }
  if (route.kind !== 'handoff' ||
      route.objectUrlLeaseMilliseconds !== BROWSER_HANDOFF_OBJECT_URL_LEASE_MILLISECONDS ||
      typeof route.packagedFileSupported !== 'boolean') {
    throw new TypeError('publication handoff route is invalid')
  }
  canonicalText(route.suggestedName)
  return Object.freeze({ ...route })
}

function canonicalPublicationRoute(route: PublicationAttemptRoute): CanonicalBytes {
  if (route.kind === 'managed') {
    return concatCanonicalBytes([
      canonicalU8(1),
      canonicalFrame(canonicalIdentity(route.reservationDigest, 32, 'reservation digest')),
    ])
  }
  return concatCanonicalBytes([
    canonicalU8(2),
    canonicalFrame(canonicalText(route.suggestedName)),
    canonicalFrame(canonicalU64(route.objectUrlLeaseMilliseconds)),
    canonicalFrame(canonicalU8(route.packagedFileSupported ? 1 : 0)),
  ])
}

function digest(value: string, label: string): string {
  return snapshotIdentity(value, 32, label)
}

function u64(value: bigint, label: string): bigint {
  if (typeof value !== 'bigint' || value < 0n || value > 0xffff_ffff_ffff_ffffn) {
    throw new TypeError(`${label} is not a u64`)
  }
  return value
}

class AggregateReader {
  readonly #bytes: Uint8Array
  #offset: number

  constructor(bytes: Uint8Array, domain?: string) {
    this.#bytes = bytes
    this.#offset = domain === undefined ? 0 : TEXT_ENCODER.encode(`${domain}\0`).byteLength + 1
  }

  identity(width: number, label: string): string {
    const bytes = this.#frame(label)
    if (bytes.byteLength !== width) throw new TypeError(`${label} width is invalid`)
    return snapshotIdentity(encodeBase64Url(bytes), width, label)
  }

  u64(label: string): bigint {
    const bytes = this.#frame(label)
    if (bytes.byteLength !== 8) throw new TypeError(`${label} width is invalid`)
    return new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getBigUint64(0, false)
  }

  u8(label: string): number {
    const bytes = this.#frame(label)
    if (bytes.byteLength !== 1) throw new TypeError(`${label} width is invalid`)
    return bytes[0]!
  }

  rawU8(label: string): number {
    return this.#take(1, label)[0]!
  }

  frame(label: string): Uint8Array {
    return this.#frame(label)
  }

  end(): void {
    if (this.#offset !== this.#bytes.byteLength) {
      throw new TypeError('aggregate record has trailing canonical bytes')
    }
  }

  #frame(label = 'aggregate frame'): Uint8Array {
    const lengthBytes = this.#take(8, `${label} length`)
    const length = new DataView(
      lengthBytes.buffer,
      lengthBytes.byteOffset,
      lengthBytes.byteLength,
    ).getBigUint64(0, false)
    if (length > BigInt(Number.MAX_SAFE_INTEGER)) {
      throw new TypeError('aggregate record frame is too large')
    }
    return this.#take(Number(length), label)
  }

  #take(length: number, label = 'aggregate field'): Uint8Array {
    if (!Number.isSafeInteger(length) || length < 0 ||
        this.#offset > this.#bytes.byteLength - length) {
      throw new TypeError(`${label} is truncated`)
    }
    const value = this.#bytes.subarray(this.#offset, this.#offset + length)
    this.#offset += length
    return value
  }
}
