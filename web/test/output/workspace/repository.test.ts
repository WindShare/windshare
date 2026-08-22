import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../../src/crypto/bytes'
import {
  CHECKPOINT_DATABASE_VERSION,
  INDEXEDDB_V9_STORE_SCHEMAS,
} from '../../../src/output/browser/indexeddb-database'
import {
  RECEIVE_RECORD_MATERIALIZED_MANIFEST,
  createManifestPageRecord,
  receiveOperationLeaseRecord,
} from '../../../src/output/workspace/records'
import { prepareReceiveOperationTransition } from '../../../src/output/workspace/repository'
import {
  initialReceiveLifecycleState,
  nextReceiveLifecycleState,
} from '../../../src/output/workspace/state'
import {
  canonicalReceiveLifecycleStateBytes,
  decodeReceiveLifecycleState,
  storedReceiveLifecycleState,
} from '../../../src/output/workspace/state-codec'

describe('IndexedDB v9 operation repository contract', () => {
  it('isolates V2 receive authority and Direct ZIP journal stores', () => {
    expect(CHECKPOINT_DATABASE_VERSION).toBe(9)
    const stores = new Map(INDEXEDDB_V9_STORE_SCHEMAS.map(value => [value.name, value]))
    expect([...stores.keys()]).not.toContain('receive-operation-v1-records')
    expect(stores.get('receive-operation-v2-records')).toEqual(schema(
      'receive-operation-v2-records',
      [
        ['by-operation', 'operationId'],
        ['by-operation-kind', ['operationId', 'kind']],
        ['by-reopen-key', 'reopenKey'],
        ['by-state', 'state'],
        ['by-expiry', 'expiresAt'],
        ['by-kind', 'kind'],
      ],
    ))
    expect([...stores.keys()].filter(name => name.startsWith('direct-zip-'))).toEqual([
      'direct-zip-state-v1',
      'direct-zip-candidates-v1',
      'direct-zip-layout-pages-v1',
      'direct-zip-central-pages-v1',
      'direct-zip-epoch-pages-v1',
    ])
  })

  it('round-trips canonical lifecycle authority instead of trusting indexes', () => {
    const lifecycle = nextReceiveLifecycleState(initialReceiveLifecycleState({
      operationId: identity(16, 1),
      receiveIntentDigest: identity(32, 2),
    }), {
      kind: 'needs-attention',
      reason: 'cleanup-unknown',
      lastVerifiedRecordDigest: identity(32, 3),
    })
    const bytes = canonicalReceiveLifecycleStateBytes(lifecycle)
    expect(decodeReceiveLifecycleState(bytes)).toEqual(lifecycle)

    const truncated = bytes.subarray(0, bytes.length - 1)
    expect(() => decodeReceiveLifecycleState(truncated)).toThrow()
  })

  it('rejects lifecycle index projections that disagree with canonical authority', async () => {
    const lifecycle = initialReceiveLifecycleState({
      operationId: identity(16, 1),
      receiveIntentDigest: identity(32, 2),
    })
    const record = await storedReceiveLifecycleState(lifecycle)
    await expect(prepareReceiveOperationTransition({
      operationId: lifecycle.operationId,
      records: [{ ...record, state: 20 }],
    })).rejects.toThrow('projections disagree')
  })

  it('rebuilds manifest pages before accepting their indexed projections', async () => {
    const operationId = identity(16, 1)
    const page = await createManifestPageRecord({
      operationId,
      ownerKind: RECEIVE_RECORD_MATERIALIZED_MANIFEST,
      ownerDigest: identity(32, 2),
      pageIndex: 0,
      totalPageCount: 1,
      canonicalEntries: [Uint8Array.of(1)],
    })
    await expect(prepareReceiveOperationTransition({
      operationId,
      manifestPages: [{ ...page, entryCount: 2 }],
    })).rejects.toThrow('projections are invalid')
  })

  it('rejects deletion keys outside the transition operation namespace', async () => {
    await expect(prepareReceiveOperationTransition({
      operationId: identity(16, 1),
      deleteRecordIds: [
        `windshare/receive-operation/v1/${identity(16, 2)}/9`,
      ],
    })).rejects.toThrow('invalid or repeated record deletion')
  })

  it('prepares lifecycle and lease mutations as one generation-checked transition', async () => {
    const operationId = identity(16, 1)
    const leaseId = identity(16, 4)
    const initial = initialReceiveLifecycleState({
      operationId,
      receiveIntentDigest: identity(32, 2),
    })
    const prepared = await prepareReceiveOperationTransition({
      operationId,
      lifecycle: initial,
      lease: {
        kind: 'put',
        record: receiveOperationLeaseRecord({
          operationId,
          leaseId,
          acquiredAt: 5_000,
        }),
      },
    })
    expect(prepared.records).toHaveLength(1)
    expect(prepared.lease).toEqual(expect.objectContaining({ kind: 'put' }))

    await expect(prepareReceiveOperationTransition({
      operationId,
      expectedLifecycleGeneration: 7n,
      lifecycle: nextReceiveLifecycleState(initial, {
        kind: 'receiving',
        activeLeaseId: leaseId,
      }),
    })).rejects.toThrow('exactly one generation')
  })
})

function schema(
  name: string,
  indexes: readonly (readonly [string, string | readonly string[]])[],
  keyPath = 'id',
) {
  return {
    name,
    keyPath,
    indexes: indexes.map(([indexName, keyPath]) => ({ name: indexName, keyPath })),
  }
}

function identity(width: number, value: number): string {
  return encodeBase64Url(new Uint8Array(width).fill(value))
}
