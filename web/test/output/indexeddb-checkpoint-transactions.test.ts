import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import {
  INDEXEDDB_V7_STORE_SCHEMAS,
  INDEXEDDB_V8_STORE_SCHEMAS,
  INDEXEDDB_V9_STORE_SCHEMAS,
  installIndexedDbV8Schema,
  installIndexedDbV9Schema,
} from '../../src/output/browser/indexeddb-database'
import {
  assertCheckpointInstallCapacity,
  assertPristineInitialCheckpoint,
  type IndexedDbCheckpointInventory,
} from '../../src/output/browser/indexeddb/repository-transactions'
import {
  FILE_CHECKPOINT_COMMIT_CANDIDATE,
  FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
  FILE_CHECKPOINT_PHASE_ACTIVE,
  MAX_CHECKPOINT_RECORDS_PER_OPERATION,
  newFileCheckpointV2,
  type FileCheckpointSpec,
} from '../../src/output/persistence/checkpoint'

describe('IndexedDB checkpoint transaction admission', () => {
  it('owns the v7 metadata reset inside the v8 versionchange transaction', () => {
    const cleared: string[] = []
    const stores = new Map(INDEXEDDB_V8_STORE_SCHEMAS.map(schema => [
      schema.name,
      {
        indexNames: { contains: (name: string) =>
          schema.indexes.some(index => index.name === name) },
        clear: () => cleared.push(schema.name),
      },
    ]))
    const database = {
      objectStoreNames: { contains: (name: string) => stores.has(name) },
    } as unknown as IDBDatabase
    const transaction = {
      objectStore: (name: string) => stores.get(name),
    } as unknown as IDBTransaction

    installIndexedDbV8Schema(database, transaction, 7)

    expect(cleared).toEqual(INDEXEDDB_V7_STORE_SCHEMAS.map(schema => schema.name))
    expect(INDEXEDDB_V8_STORE_SCHEMAS.slice(-2).map(schema => schema.name)).toEqual([
      'compatible-name-v1-operations',
      'compatible-name-v1-mappings',
    ])
  })

  it('destructively separates incompatible v8 receive authority during v9 migration', () => {
    const deleted: string[] = []
    const cleared: string[] = []
    const created: string[] = []
    const schemas = new Map(INDEXEDDB_V8_STORE_SCHEMAS.map(schema => [schema.name, schema]))
    const stores = new Map(INDEXEDDB_V8_STORE_SCHEMAS.map(schema => [
      schema.name,
      {
        indexNames: { contains: (name: string) =>
          schema.indexes.some(index => index.name === name) },
        createIndex: () => undefined,
        clear: () => cleared.push(schema.name),
      },
    ]))
    const database = {
      objectStoreNames: { contains: (name: string) => stores.has(name) },
      createObjectStore: (name: string) => {
        created.push(name)
        const store = {
          indexNames: { contains: () => false },
          createIndex: () => undefined,
          clear: () => cleared.push(name),
        }
        stores.set(name, store)
        return store
      },
      deleteObjectStore: (name: string) => {
        deleted.push(name)
        stores.delete(name)
      },
    } as unknown as IDBDatabase
    const transaction = {
      objectStore: (name: string) => stores.get(name),
    } as unknown as IDBTransaction

    installIndexedDbV9Schema(database, transaction, 8)

    expect(deleted).toEqual([
      'receive-operation-v1-records',
      'receive-operation-v1-manifest-pages',
      'receive-operation-v1-handles',
      'receive-operation-v1-leases',
    ])
    expect(cleared).toEqual([
      'file-checkpoint-v2-candidates',
      'file-checkpoint-v2-committed',
      'file-checkpoint-v2-handles',
      'file-checkpoint-v2-control',
      'compatible-name-v1-operations',
      'compatible-name-v1-mappings',
    ])
    expect(created).toEqual(INDEXEDDB_V9_STORE_SCHEMAS
      .filter(schema => !schemas.has(schema.name))
      .map(schema => schema.name))
  })

  it('refuses to upgrade existing stores without the owning versionchange transaction', () => {
    const database = {
      objectStoreNames: { contains: () => true },
    } as unknown as IDBDatabase
    expect(() => installIndexedDbV8Schema(database, undefined, 7)).toThrow(
      'lacks its versionchange transaction',
    )
  })

  it('enforces the operation-wide physical RecordID capacity before installation', () => {
    const candidate = checkpoint()
    const physicalRecordIds = new Set<string>()
    Object.defineProperty(physicalRecordIds, 'size', {
      value: MAX_CHECKPOINT_RECORDS_PER_OPERATION,
    })

    expect(() => assertCheckpointInstallCapacity(
      inventory({ physicalRecordIds }),
      candidate,
    )).toThrow(expect.objectContaining({ name: 'QuotaExceededError' }))
  })

  it('rejects a proposed object already claimed by another physical record', () => {
    const candidate = checkpoint()
    const existing = checkpoint({
      fileId: identity(16, 0x21),
      canonicalPath: ['other.bin'],
    })

    expect(() => assertCheckpointInstallCapacity(
      inventory({ operationRecords: [existing] }),
      candidate,
    )).toThrow(expect.objectContaining({ name: 'InvalidStateError' }))
  })

  it('admits only the pristine candidate-before-object crash state', () => {
    const candidate = checkpoint()
    expect(() => assertPristineInitialCheckpoint(candidate)).not.toThrow()
    expect(() => assertPristineInitialCheckpoint(checkpoint({
      checkpointGeneration: 1n,
      verifiedRanges: [{ start: 0n, end: 1n }],
    }))).toThrow('pristine active candidate')
  })
})

function inventory(overrides: Partial<IndexedDbCheckpointInventory>): IndexedDbCheckpointInventory {
  return {
    lineageRecords: [],
    operationRecords: [],
    physicalRecordIds: new Set<string>(),
    crossLineageOwnershipConflict: false,
    ...overrides,
  }
}

function checkpoint(overrides: Partial<FileCheckpointSpec> = {}) {
  return newFileCheckpointV2({
    operationId: identity(16, 1),
    receiveIntentDigest: identity(32, 2),
    materializationBindingDigest: identity(32, 3),
    fileId: identity(16, 4),
    fileRevision: identity(16, 5),
    canonicalPath: ['file.bin'],
    exactSize: 8n,
    materializerKind: FILE_CHECKPOINT_MATERIALIZER_FSA_TREE,
    authorityRef: identity(32, 6),
    ownedObjectId: identity(32, 7),
    stateGeneration: 1n,
    checkpointGeneration: 0n,
    verifiedRanges: [],
    phase: FILE_CHECKPOINT_PHASE_ACTIVE,
    commitState: FILE_CHECKPOINT_COMMIT_CANDIDATE,
    ...overrides,
  })
}

function identity(width: number, fill: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(fill))
}
