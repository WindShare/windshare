import { DatabaseSync, type SQLInputValue } from 'node:sqlite'
import type { DonationWallRpc } from '../src/env'
import type {
  AdminConfigPatch,
  AdminDonationsFilter,
  AdminVisibilityCommand,
  CommitDonationInput,
  CreateAdminSessionInput,
  CreateCryptoIntentInput,
  DonationProvider,
} from '../src/model'
import { WallStore, matchesIfNoneMatch, type SqlStoragePort } from '../src/wall-store'

export type AsyncWallStub = {
  [K in Exclude<keyof DonationWallRpc, keyof Rpc.DurableObjectBranded>]: DonationWallRpc[K] extends (...args: infer P) => infer R
    ? (...args: P) => Promise<R>
    : never
}

export function createInMemoryWallStoreWithDb(): { readonly store: WallStore; readonly db: DatabaseSync } {
  const db = new DatabaseSync(':memory:')
  const sqlPort: SqlStoragePort = {
    exec(query: string, ...bindings: unknown[]): Iterable<Record<string, unknown>> {
      const stmt = db.prepare(query)
      const params = bindings as SQLInputValue[]
      const trimmed = query.trimStart().toUpperCase()
      if (trimmed.startsWith('SELECT')) {
        return stmt.all(...params) as Record<string, unknown>[]
      }
      stmt.run(...params)
      return []
    },
  }
  let inTx = false
  const runTx = <T,>(closure: () => T): T => {
    // Enforce flat transaction boundaries so tests catch any accidental nested runTx calls.
    if (inTx) {
      throw new Error('nested WallStore transaction detected')
    }
    inTx = true
    db.exec('BEGIN')
    try {
      const result = closure()
      db.exec('COMMIT')
      return result
    } catch (error) {
      db.exec('ROLLBACK')
      throw error
    } finally {
      inTx = false
    }
  }
  return { store: new WallStore(sqlPort, runTx), db }
}

export function createInMemoryWallStore(): WallStore {
  return createInMemoryWallStoreWithDb().store
}

export function createWallNamespaceFromStore(
  store: WallStore,
  options?: {
    readonly onCall?: () => void
    readonly overrides?: Partial<AsyncWallStub>
  },
): DurableObjectNamespace<DonationWallRpc> {
  const track = (): void => {
    options?.onCall?.()
  }
  const baseStub: AsyncWallStub = {
    getPublicWall: async (ifNoneMatch: string | null) => {
      track()
      const snapshot = store.getPublicWallSnapshot()
      if (matchesIfNoneMatch(ifNoneMatch, snapshot.etag)) {
        return { notModified: true, etag: snapshot.etag }
      }
      return { notModified: false, etag: snapshot.etag, json: snapshot.json }
    },
    createCryptoIntent: async (input: CreateCryptoIntentInput) => {
      track()
      return store.createCryptoIntent(input)
    },
    getCryptoIntent: async (intentId: string, nowSec: number) => {
      track()
      return store.getCryptoIntent(intentId, nowSec)
    },
    hasDonation: async (provider: DonationProvider, externalId: string) => {
      track()
      return store.hasDonation(provider, externalId)
    },
    findExistingExternalIds: async (provider: DonationProvider, externalIds: readonly string[]) => {
      track()
      return store.findExistingExternalIds(provider, externalIds)
    },
    commitDonation: async (input: CommitDonationInput, nowSec: number) => {
      track()
      return store.commitDonation(input, nowSec)
    },
    commitDonationsBatch: async (inputs: readonly CommitDonationInput[], nowSec: number) => {
      track()
      return store.commitDonationsBatch(inputs, nowSec)
    },
    consumeAdminLoginRateLimit: async (clientIp: string, nowSec: number) => {
      track()
      return store.consumeRateLimit('admin_login', clientIp, nowSec)
    },
    createAdminSession: async (input: CreateAdminSessionInput) => {
      track()
      return store.createAdminSession(input)
    },
    verifyAdminSession: async (sessionHash: string, nowSec: number) => {
      track()
      return store.verifyAdminSession(sessionHash, nowSec)
    },
    revokeAdminSession: async (sessionHash: string) => {
      track()
      store.revokeAdminSession(sessionHash)
    },
    getAdminConfig: async () => {
      track()
      return store.getConfig()
    },
    patchAdminConfig: async (patch: AdminConfigPatch) => {
      track()
      return store.patchConfig(patch)
    },
    listAdminDonations: async (
      afterCursor: string | null,
      rawLimit: number | null,
      filter?: AdminDonationsFilter,
    ) => {
      track()
      return store.listAdminDonations(afterCursor, rawLimit, filter)
    },
    moderateDonationVisibility: async (donationId: string, command: AdminVisibilityCommand, nowSec: number) => {
      track()
      return store.moderateDonationVisibility(donationId, command, nowSec)
    },
  }
  const stub: AsyncWallStub = { ...baseStub, ...options?.overrides }
  return {
    idFromName: (name: string) => ({ toString: () => name }),
    get: () => stub,
  } as unknown as DurableObjectNamespace<DonationWallRpc>
}
