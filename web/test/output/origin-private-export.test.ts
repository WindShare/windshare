import {
  Uint8ArrayReader,
  Uint8ArrayWriter,
  ZipReader,
} from '@zip.js/zip.js'
import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import { OriginPrivateZipPackageBuilder } from '../../src/output/origin-private/zip-exporter'
import type { MaterializedManifestV1 } from '../../src/output/workspace/manifest'
import { planZipLayout } from '../../src/output/zip-layout/layout'
import { MemoryZipCentralDirectorySpool } from './zip-spool-fake'

const RECEIVE_INTENT_DIGEST = identity(32, 1)
const ARTIFACT_DIGEST = identity(32, 2)
const PREPARATION_DIGEST = identity(32, 3)
const SEALED_MATERIALIZATION_DIGEST = identity(32, 4)
const PACKAGE_OBJECT_ID = identity(32, 5)
const RAW_OBJECT_ID = identity(32, 6)
const ACTIVE_SIGNAL = new AbortController().signal

describe('origin-private sealed ZIP packaging', () => {
  it('writes the full sealed member set and verifies the exact closed package length', async () => {
    const layout = await fixtureLayout()
    const output = recordedOutput()
    const builder = new OriginPrivateZipPackageBuilder(
      () => new MemoryZipCentralDirectorySpool(),
    )
    const result = await builder.build({
      operationId: identity(16, 7),
      receiveIntentDigest: RECEIVE_INTENT_DIGEST,
      sealedMaterializationDigest: SEALED_MATERIALIZATION_DIGEST,
      manifest: fixtureManifest(),
      layout,
      packageOwnedObjectId: PACKAGE_OBJECT_ID,
      output: output.stream,
      source: { readOwnedFile: async () => new Blob([Uint8Array.of(1, 2, 3)]) },
      readPackageExactBytes: async () => BigInt(output.bytes().byteLength),
      signal: ACTIVE_SIGNAL,
    })

    expect(result).toEqual(expect.objectContaining({
      kind: 'sealed',
      verification: expect.objectContaining({
        writerCloseVerified: true,
        exactBytes: layout.exactArchiveBytes,
      }),
    }))
    const reader = new ZipReader(new Uint8ArrayReader(output.bytes()))
    const entries = await reader.getEntries()
    expect(entries.map((entry) => entry.filename)).toEqual(['root/', 'root/file.bin'])
    const file = entries[1]
    if (file === undefined || file.directory) throw new Error('packaged member is missing')
    expect(await file.getData(new Uint8ArrayWriter())).toEqual(Uint8Array.of(1, 2, 3))
    await reader.close()
  })

  it('never seals a known-incomplete member and aborts the package writer', async () => {
    const layout = await fixtureLayout()
    const output = recordedOutput()
    const builder = new OriginPrivateZipPackageBuilder(
      () => new MemoryZipCentralDirectorySpool(),
    )

    await expect(builder.build({
      operationId: identity(16, 7),
      receiveIntentDigest: RECEIVE_INTENT_DIGEST,
      sealedMaterializationDigest: SEALED_MATERIALIZATION_DIGEST,
      manifest: fixtureManifest(),
      layout,
      packageOwnedObjectId: PACKAGE_OBJECT_ID,
      output: output.stream,
      source: { readOwnedFile: async () => new Blob([Uint8Array.of(1)]) },
      readPackageExactBytes: async () => BigInt(output.bytes().byteLength),
      signal: ACTIVE_SIGNAL,
    })).rejects.toThrow('materialized file length changed')
    expect(output.aborted).toBe(true)
  })

  it('retries spool cleanup after writer close without rebuilding package bytes', async () => {
    const layout = await fixtureLayout()
    const output = recordedOutput()
    const spool = new FailOnceClearSpool()
    const builder = new OriginPrivateZipPackageBuilder(() => spool)
    const input = {
      operationId: identity(16, 7),
      receiveIntentDigest: RECEIVE_INTENT_DIGEST,
      sealedMaterializationDigest: SEALED_MATERIALIZATION_DIGEST,
      manifest: fixtureManifest(),
      layout,
      packageOwnedObjectId: PACKAGE_OBJECT_ID,
      output: output.stream,
      source: { readOwnedFile: async () => new Blob([Uint8Array.of(1, 2, 3)]) },
      readPackageExactBytes: async () => BigInt(output.bytes().byteLength),
      signal: ACTIVE_SIGNAL,
    }

    const pending = await builder.build(input)
    expect(pending.kind).toBe('cleanup-pending')
    const packageBytes = output.bytes()
    const sealed = await builder.retryCleanup()

    expect(sealed.kind).toBe('sealed')
    expect(output.bytes()).toEqual(packageBytes)
    expect(spool.clearAttempts).toBe(2)
  })
})

class FailOnceClearSpool extends MemoryZipCentralDirectorySpool {
  clearAttempts = 0

  override async clear(): Promise<void> {
    this.clearAttempts += 1
    if (this.clearAttempts === 1) throw new Error('transient cleanup failure')
    await super.clear()
  }
}

async function fixtureLayout() {
  return planZipLayout({
    receiveIntentDigest: RECEIVE_INTENT_DIGEST,
    artifactDigest: ARTIFACT_DIGEST,
    preparationManifestDigest: PREPARATION_DIGEST,
    entries: [
      { kind: 'directory', path: ['root'] },
      { kind: 'file', path: ['root', 'file.bin'], exactSize: 3n },
    ],
  })
}

function fixtureManifest(): MaterializedManifestV1 {
  return {
    schemaVersion: 1,
    operationId: identity(16, 7),
    receiveIntentDigest: RECEIVE_INTENT_DIGEST,
    materializationBindingDigest: identity(32, 8),
    preparationBinding: { kind: 'present', preparationDigest: PREPARATION_DIGEST },
    generations: [],
    entries: [
      {
        kind: 'directory',
        artifactPath: ['root'],
        directoryId: identity(16, 9),
        generation: identity(16, 10),
        ownedObjectId: identity(32, 11),
      },
      {
        kind: 'file',
        artifactPath: ['root', 'file.bin'],
        fileId: identity(16, 12),
        fileRevision: identity(16, 13),
        exactSize: 3n,
        ownedObjectId: RAW_OBJECT_ID,
        checkpoint: {
          recordId: 'checkpoint-record',
          recordDigest: identity(32, 14),
          checkpointGeneration: 1n,
        },
      },
    ],
    entryCount: 2n,
    fileCount: 1n,
    directoryCount: 1n,
    rawBytes: 3n,
    canonicalMetadataBytes: 1n,
    canonicalBytes: Uint8Array.of(1),
    digest: identity(32, 15),
  }
}

function recordedOutput(): {
  readonly stream: WritableStream<Uint8Array>
  readonly aborted: boolean
  bytes(): Uint8Array
} {
  const chunks: Uint8Array[] = []
  let aborted = false
  return {
    stream: new WritableStream<Uint8Array>({
      write: (chunk) => { chunks.push(chunk.slice()) },
      abort: () => { aborted = true },
    }),
    get aborted() { return aborted },
    bytes: () => {
      const bytes = new Uint8Array(chunks.reduce((total, chunk) => total + chunk.length, 0))
      let offset = 0
      for (const chunk of chunks) {
        bytes.set(chunk, offset)
        offset += chunk.length
      }
      return bytes
    },
  }
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
