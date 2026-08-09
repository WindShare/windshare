import { StreamingZipArchiveWriter } from '../streams/streaming-zip'
import {
  IndexedDbZipCentralDirectorySpool,
  type ZipCentralDirectorySpool,
} from '../streams/zip-spool'
import {
  validateSealedZipLayoutPlan,
  type SealedZipLayoutPlanV1,
} from '../zip-layout/layout'
import type { ZipEntryPlanV1 } from '../zip-layout/policy'
import type { MaterializedManifestV1 } from '../workspace/manifest'
import {
  createZipArtifactVerificationReceipt,
  type ZipArtifactVerificationReceiptV1,
} from '../workspace/receipts'

export interface OriginPrivatePackageSource {
  readOwnedFile(ownedObjectId: string): Promise<Blob>
}

export type OriginPrivateZipPackageResult =
  | Readonly<{
      kind: 'sealed'
      verification: ZipArtifactVerificationReceiptV1
    }>
  | Readonly<{
      kind: 'cleanup-pending'
      retryCleanup(): Promise<OriginPrivateZipPackageResult>
    }>

interface PendingZipPackageVerification {
  readonly operationId: string
  readonly receiveIntentDigest: string
  readonly sealedMaterializationDigest: string
  readonly layoutDigest: string
  readonly packageOwnedObjectId: string
  readonly exactBytes: bigint
}

/**
 * Packaging consumes only sealed layout/member authority. It cannot discover new
 * members or turn cancellation into a known-incomplete archive.
 */
export class OriginPrivateZipPackageBuilder {
  readonly #createSpool: () => ZipCentralDirectorySpool
  #state: 'idle' | 'building' | 'cleanup-pending' | 'sealed' | 'failed' = 'idle'
  #writer: StreamingZipArchiveWriter | undefined
  #pendingVerification: PendingZipPackageVerification | undefined
  #cleanupPromise: Promise<OriginPrivateZipPackageResult> | undefined

  constructor(
    createSpool: () => ZipCentralDirectorySpool = () => new IndexedDbZipCentralDirectorySpool(),
  ) {
    this.#createSpool = createSpool
  }

  async build(input: {
    readonly operationId: string
    readonly receiveIntentDigest: string
    readonly sealedMaterializationDigest: string
    readonly manifest: MaterializedManifestV1
    readonly layout: SealedZipLayoutPlanV1
    readonly packageOwnedObjectId: string
    readonly output: WritableStream<Uint8Array>
    readonly source: OriginPrivatePackageSource
    readonly readPackageExactBytes: () => Promise<bigint>
    readonly signal: AbortSignal
  }): Promise<OriginPrivateZipPackageResult> {
    if (this.#state !== 'idle') throw new Error('origin-private ZIP package builder is not idle')
    input.signal.throwIfAborted()
    this.#state = 'building'
    const layout = await validateSealedZipLayoutPlan(input.layout)
    validatePackageAuthority(input.manifest, layout, input.receiveIntentDigest)
    const writer = new StreamingZipArchiveWriter(
      input.output,
      this.#createSpool(),
      Object.freeze({ kind: 'sealed', plan: layout }),
    )
    this.#writer = writer
    try {
      const files = materializedFilesByPath(input.manifest)
      for (const entry of layout.entries) {
        input.signal.throwIfAborted()
        if (entry.kind === 'directory') {
          await writer.addDirectory(entry)
          continue
        }
        const materialized = files.get(pathKey(entry.path))
        if (materialized === undefined || materialized.exactSize !== entry.exactSize) {
          throw new TypeError('sealed ZIP member escaped its materialized file authority')
        }
        await writeMember(
          writer,
          entry,
          await input.source.readOwnedFile(materialized.ownedObjectId),
          input.signal,
        )
      }
      await writer.close(layout, input.signal)
      const actualBytes = await input.readPackageExactBytes()
      if (actualBytes !== layout.exactArchiveBytes) {
        throw new TypeError('origin-private package length changed after writer close')
      }
      this.#pendingVerification = Object.freeze({
        operationId: input.operationId,
        receiveIntentDigest: input.receiveIntentDigest,
        sealedMaterializationDigest: input.sealedMaterializationDigest,
        layoutDigest: layout.digest,
        packageOwnedObjectId: input.packageOwnedObjectId,
        exactBytes: actualBytes,
      })
      if (writer.cleanupPending) {
        this.#state = 'cleanup-pending'
        return this.#pendingCleanupResult()
      }
      return this.#sealVerification()
    } catch (error) {
      this.#state = 'failed'
      try {
        await writer.abort(error)
      } catch (abortError) {
        throw new AggregateError(
          [error, abortError],
          'origin-private package build and cleanup failed',
          { cause: abortError },
        )
      }
      throw error
    }
  }

  retryCleanup(): Promise<OriginPrivateZipPackageResult> {
    if (this.#state === 'sealed') {
      throw new Error('origin-private ZIP package is already sealed')
    }
    if (this.#state !== 'cleanup-pending' || this.#writer === undefined) {
      throw new Error('origin-private ZIP package has no retryable cleanup')
    }
    if (this.#cleanupPromise !== undefined) return this.#cleanupPromise
    const operation = this.#writer.retryCleanup().then(() => {
      if (this.#writer?.cleanupPending === true) return this.#pendingCleanupResult()
      return this.#sealVerification()
    }).finally(() => { this.#cleanupPromise = undefined })
    this.#cleanupPromise = operation
    return operation
  }

  #pendingCleanupResult(): OriginPrivateZipPackageResult {
    return Object.freeze({
      kind: 'cleanup-pending',
      retryCleanup: () => this.retryCleanup(),
    })
  }

  async #sealVerification(): Promise<OriginPrivateZipPackageResult> {
    const pending = this.#pendingVerification
    if (pending === undefined) throw new Error('ZIP package lost its close verification')
    const verification = await createZipArtifactVerificationReceipt({
      ...pending,
      writerCloseVerified: true,
    })
    this.#state = 'sealed'
    this.#writer = undefined
    this.#pendingVerification = undefined
    return Object.freeze({ kind: 'sealed', verification })
  }
}

async function writeMember(
  writer: StreamingZipArchiveWriter,
  entry: ZipEntryPlanV1,
  blob: Blob,
  signal: AbortSignal,
): Promise<void> {
  if (BigInt(blob.size) !== entry.exactSize) {
    throw new TypeError('materialized file length changed before packaging')
  }
  const member = await writer.beginFile(entry)
  const reader = blob.stream().getReader()
  try {
    while (true) {
      signal.throwIfAborted()
      const chunk = await reader.read()
      if (chunk.done) break
      await member.write(chunk.value)
    }
    signal.throwIfAborted()
    await member.close()
  } catch (error) {
    await member.abort(error)
    throw error
  } finally {
    reader.releaseLock()
  }
}

function materializedFilesByPath(
  manifest: MaterializedManifestV1,
): ReadonlyMap<string, Extract<MaterializedManifestV1['entries'][number], { kind: 'file' }>> {
  const files = new Map<
    string,
    Extract<MaterializedManifestV1['entries'][number], { kind: 'file' }>
  >()
  for (const entry of manifest.entries) {
    if (entry.kind !== 'file') continue
    const key = pathKey(entry.artifactPath)
    if (files.has(key)) throw new TypeError('materialized manifest repeats a ZIP member path')
    files.set(key, entry)
  }
  return files
}

function validatePackageAuthority(
  manifest: MaterializedManifestV1,
  layout: SealedZipLayoutPlanV1,
  receiveIntentDigest: string,
): void {
  if (manifest.receiveIntentDigest !== receiveIntentDigest ||
      layout.receiveIntentDigest !== receiveIntentDigest ||
      manifest.preparationBinding.kind !== 'present' ||
      layout.evidence.kind !== 'prepared' ||
      manifest.preparationBinding.preparationDigest !==
        layout.evidence.preparationManifestDigest ||
      manifest.entryCount !== layout.entryCount) {
    throw new TypeError('ZIP package authorities disagree')
  }
}

function pathKey(path: readonly string[]): string {
  return path.join('\0')
}
