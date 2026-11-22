import { DurableObject } from 'cloudflare:workers'
import type { DonationWallRpc, DonationsEnv } from './env'
import type {
  AdminConfigPatch,
  AdminDonationsCursor,
  AdminDonationsFilter,
  AdminDonationsPage,
  AdminSessionRecord,
  AdminVisibilityCommand,
  CommitDonationInput,
  CommitDonationResult,
  CreateAdminSessionInput,
  CreateCryptoIntentInput,
  CreateManualDonationInput,
  CreateManualDonationResult,
  CryptoCheckoutIntent,
  DonationConfig,
  DonationProvider,
  PublicWallReadResult,
} from './model'
import {
  WallStore,
  matchesIfNoneMatch,
  type CreateIntentResult,
  type ModerateVisibilityResult,
  type RateLimitDecision,
} from './wall-store'

export class DonationWall extends DurableObject<DonationsEnv> implements DonationWallRpc {
  private readonly store: WallStore

  constructor(ctx: DurableObjectState, env: DonationsEnv) {
    super(ctx, env)
    this.store = new WallStore(
      this.ctx.storage.sql,
      fn => this.ctx.storage.transactionSync(fn),
    )
  }

  getPublicWall(ifNoneMatch: string | null): PublicWallReadResult {
    const snapshot = this.store.getPublicWallSnapshot()
    if (matchesIfNoneMatch(ifNoneMatch, snapshot.etag)) {
      return { notModified: true, etag: snapshot.etag }
    }
    return { notModified: false, etag: snapshot.etag, json: snapshot.json }
  }

  createCryptoIntent(input: CreateCryptoIntentInput): CreateIntentResult {
    return this.store.createCryptoIntent(input)
  }

  getCryptoIntent(intentId: string, nowSec: number): CryptoCheckoutIntent | null {
    return this.store.getCryptoIntent(intentId, nowSec)
  }

  hasDonation(provider: DonationProvider, externalId: string): boolean {
    return this.store.hasDonation(provider, externalId)
  }

  findExistingExternalIds(provider: DonationProvider, externalIds: readonly string[]): string[] {
    return this.store.findExistingExternalIds(provider, externalIds)
  }

  commitDonation(input: CommitDonationInput, nowSec: number): CommitDonationResult {
    return this.store.commitDonation(input, nowSec)
  }

  commitDonationsBatch(inputs: readonly CommitDonationInput[], nowSec: number): CommitDonationResult[] {
    return this.store.commitDonationsBatch(inputs, nowSec)
  }

  createManualDonation(input: CreateManualDonationInput, nowSec: number): CreateManualDonationResult {
    return this.store.createManualDonation(input, nowSec)
  }

  consumeAdminLoginRateLimit(clientIp: string, nowSec: number): RateLimitDecision {
    return this.store.consumeRateLimit('admin_login', clientIp, nowSec)
  }

  createAdminSession(input: CreateAdminSessionInput): AdminSessionRecord {
    return this.store.createAdminSession(input)
  }

  verifyAdminSession(sessionHash: string, nowSec: number): AdminSessionRecord | null {
    return this.store.verifyAdminSession(sessionHash, nowSec)
  }

  revokeAdminSession(sessionHash: string): void {
    this.store.revokeAdminSession(sessionHash)
  }

  getAdminConfig(): DonationConfig {
    return this.store.getConfig()
  }

  patchAdminConfig(patch: AdminConfigPatch): DonationConfig {
    return this.store.patchConfig(patch)
  }

  listAdminDonations(
    afterCursor: AdminDonationsCursor | null,
    rawLimit: number | null,
    filter?: AdminDonationsFilter,
  ): AdminDonationsPage {
    return this.store.listAdminDonations(afterCursor, rawLimit, filter)
  }

  moderateDonationVisibility(
    donationId: string,
    command: AdminVisibilityCommand,
    nowSec: number,
  ): ModerateVisibilityResult {
    return this.store.moderateDonationVisibility(donationId, command, nowSec)
  }
}
