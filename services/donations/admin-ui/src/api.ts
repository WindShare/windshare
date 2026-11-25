import {
  DONATION_PROVIDERS,
  type AdminConfigPatch,
  type AdminDonationsFilter,
  type AdminDonationsPage,
  type AdminVisibilityCommand,
  type AdminVisibilityCounts,
  type DonationConfig,
  type DonationProvider,
  type DonationRecord,
  type RestorableVisibility,
  type WallVisibility,
} from '../../src/model'
import type { CopyCatalog } from './locales'

export interface ApiSuccess<T> {
  readonly ok: true
  readonly status: number
  readonly data: T
}

export interface ApiFailure {
  readonly ok: false
  readonly status: number
  readonly error: string
  readonly retryAfterSeconds: number | null
}

export type ApiResult<T> = ApiSuccess<T> | ApiFailure

export interface AdminFetchInit {
  readonly method: 'GET' | 'POST' | 'PATCH'
  readonly headers: Readonly<Record<string, string>>
  readonly credentials: 'same-origin'
  readonly body?: string
  readonly signal?: AbortSignal
}

export interface AdminFetchResponse {
  readonly ok: boolean
  readonly status: number
  readonly headers: { get(name: string): string | null }
  json(): Promise<unknown>
}

export type AdminFetchFn = (path: string, init: AdminFetchInit) => Promise<AdminFetchResponse>

export interface SessionInfo {
  readonly authenticated: true
  readonly csrfToken: string
  readonly expiresAt: number
  readonly config: DonationConfig
  readonly afdianSyncConfigured: boolean
}

export interface LoginInfo {
  readonly authenticated: true
  readonly csrfToken: string
  readonly expiresAt: number
  readonly afdianSyncConfigured: boolean
}

export interface ModerateResponse {
  readonly record: DonationRecord
  readonly counts: AdminVisibilityCounts
}

export interface ManualDonationPayload {
  readonly provider: DonationProvider
  readonly amount: string
  readonly currency: string
  readonly displayName: string | null
  readonly message: string | null
  readonly externalId: string | null
  readonly donatedAt: number | null
}

export interface SyncAfdianResponse {
  readonly fetched: number
  readonly paid: number
  readonly committed: number
  readonly deduplicated: number
  readonly deferred: number
}

interface RequestOptions {
  readonly method?: 'GET' | 'POST' | 'PATCH'
  readonly body?: unknown
  readonly headers?: Readonly<Record<string, string>>
  readonly signal?: AbortSignal
  readonly handleUnauthorized?: boolean
}

const VISIBILITY_COUNT_KEYS = [
  'total',
  'published',
  'pending_review',
  'below_threshold',
  'hidden',
  'private_donor',
] as const

const SYNC_AFDIAN_KEYS = [
  'fetched',
  'paid',
  'committed',
  'deduplicated',
  'deferred',
] as const

const STATIC_ERROR_KEYS = {
  invalid_credentials: 'invalidCredentials',
  admin_not_configured: 'adminNotConfigured',
  afdian_not_configured: 'afdianNotConfigured',
  afdian_query_failed: 'afdianQueryFailed',
  afdian_rate_limited: 'afdianRateLimited',
  invalid_amount: 'invalidAmount',
  invalid_manual_donation: 'invalidManualDonation',
  invalid_idempotency_key: 'invalidManualDonation',
  duplicate_external_id: 'duplicateExternalId',
  idempotency_key_conflict: 'idempotencyKeyConflict',
  service_unavailable: 'serviceUnavailable',
  invalid_response: 'invalidResponse',
} as const satisfies Readonly<
  Record<string, Exclude<keyof CopyCatalog['errors'], 'rateLimited' | 'fallback'>>
>

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function isFiniteNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value)
}

function isNonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0
}

function isNullableString(value: unknown): value is string | null {
  return value === null || typeof value === 'string'
}

function isDonationProvider(value: unknown): value is DonationProvider {
  return typeof value === 'string' && (DONATION_PROVIDERS as readonly string[]).includes(value)
}

function isDonationConfig(value: unknown): value is DonationConfig {
  return isPlainObject(value)
    && isFiniteNumber(value.minPublicCnyMinor)
    && isFiniteNumber(value.minPublicUsdMinor)
    && typeof value.moderationEnabled === 'boolean'
    && typeof value.cryptoCheckoutPaused === 'boolean'
}

function isVisibilityCounts(value: unknown): value is AdminVisibilityCounts {
  return isPlainObject(value) && VISIBILITY_COUNT_KEYS.every(key => isFiniteNumber(value[key]))
}

function isRestorableVisibility(value: unknown): value is RestorableVisibility {
  if (!isPlainObject(value) || typeof value.kind !== 'string') return false
  if (value.kind === 'published' || value.kind === 'below_threshold') return true
  return value.kind === 'pending_review' && typeof value.reason === 'string'
}

function isWallVisibility(value: unknown): value is WallVisibility {
  if (!isPlainObject(value) || typeof value.kind !== 'string') return false
  if (value.kind === 'private_donor' || isRestorableVisibility(value)) return true
  return value.kind === 'hidden'
    && isFiniteNumber(value.moderatedAt)
    && typeof value.reason === 'string'
    && isRestorableVisibility(value.previous)
}

function isDonationRecord(value: unknown): value is DonationRecord {
  if (!isPlainObject(value)) return false
  return isNonEmptyString(value.id)
    && isDonationProvider(value.provider)
    && typeof value.externalId === 'string'
    && isNullableString(value.displayName)
    && isFiniteNumber(value.amountMinor)
    && typeof value.currency === 'string'
    && isFiniteNumber(value.minorDigits)
    && isNullableString(value.message)
    && isWallVisibility(value.visibility)
    && isFiniteNumber(value.donatedAt)
}

function isLoginInfo(value: unknown): value is LoginInfo {
  return isPlainObject(value)
    && value.authenticated === true
    && isNonEmptyString(value.csrfToken)
    && isFiniteNumber(value.expiresAt)
    && typeof value.afdianSyncConfigured === 'boolean'
}

function isSessionInfo(value: unknown): value is SessionInfo {
  return isPlainObject(value) && isLoginInfo(value) && isDonationConfig(value.config)
}

function isLogoutResponse(value: unknown): value is { readonly ok: true } {
  return isPlainObject(value) && value.ok === true
}

function isAdminDonationsPage(value: unknown): value is AdminDonationsPage {
  return isPlainObject(value)
    && Array.isArray(value.items)
    && value.items.every(isDonationRecord)
    && isNullableString(value.nextCursor)
    && isDonationConfig(value.config)
    && isVisibilityCounts(value.counts)
}

function isModerateResponse(value: unknown): value is ModerateResponse {
  return isPlainObject(value)
    && isDonationRecord(value.record)
    && isVisibilityCounts(value.counts)
}

function isSyncAfdianResponse(value: unknown): value is SyncAfdianResponse {
  return isPlainObject(value) && SYNC_AFDIAN_KEYS.every(key => isFiniteNumber(value[key]))
}

function isConfigResponse(value: unknown): value is { readonly config: DonationConfig } {
  return isPlainObject(value) && isDonationConfig(value.config)
}

function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError'
}

function parseRetryAfter(header: string | null): number | null {
  if (header === null) return null
  const value = Number(header.trim())
  return Number.isFinite(value) && value > 0 ? Math.ceil(value) : null
}

async function readErrorPayload(res: AdminFetchResponse): Promise<Record<string, unknown> | null> {
  try {
    const parsed = await res.json()
    return isPlainObject(parsed) ? parsed : null
  } catch (error) {
    if (isAbortError(error)) throw error
    return null
  }
}

async function readSuccessJson(
  res: AdminFetchResponse,
): Promise<{ readonly ok: true; readonly value: unknown } | { readonly ok: false }> {
  try {
    return { ok: true, value: await res.json() }
  } catch (error) {
    if (error instanceof SyntaxError) {
      return { ok: false }
    }
    throw error
  }
}

export function formatApiError(failure: ApiFailure, t: CopyCatalog): string {
  if (failure.status === 0) return t.errors.network
  if (failure.error === 'rate_limited') return t.errors.rateLimited(failure.retryAfterSeconds)
  if (Object.hasOwn(STATIC_ERROR_KEYS, failure.error)) {
    const key = STATIC_ERROR_KEYS[failure.error as keyof typeof STATIC_ERROR_KEYS]
    return t.errors[key]
  }
  return t.errors.fallback(failure.error || String(failure.status))
}

export class AdminApiClient {
  private csrfToken = ''
  private readonly onSessionExpired: () => void
  private readonly fetchFn: AdminFetchFn | undefined

  constructor(onSessionExpired: () => void, fetchFn?: AdminFetchFn) {
    this.onSessionExpired = onSessionExpired
    this.fetchFn = fetchFn
  }

  setCsrf(token: string): void {
    this.csrfToken = token
  }

  clearCsrf(): void {
    this.csrfToken = ''
  }

  private buildRequestInit(options: RequestOptions): AdminFetchInit {
    const method = options.method ?? 'GET'
    const headers: Record<string, string> = { ...options.headers }
    if (options.body !== undefined) {
      headers['Content-Type'] = 'application/json'
    }
    if (method !== 'GET' && this.csrfToken.length > 0) {
      headers['X-CSRF-Token'] = this.csrfToken
    }
    return {
      method,
      headers,
      credentials: 'same-origin',
      ...(options.body === undefined ? {} : { body: JSON.stringify(options.body) }),
      ...(options.signal === undefined ? {} : { signal: options.signal }),
    }
  }

  private async toHttpFailure(
    res: AdminFetchResponse,
    handleUnauthorized: boolean,
  ): Promise<ApiFailure | null> {
    if (res.status === 401 && handleUnauthorized) {
      this.clearCsrf()
      this.onSessionExpired()
      return null
    }
    const errorPayload = await readErrorPayload(res)
    return {
      ok: false,
      status: res.status,
      error: typeof errorPayload?.error === 'string' ? errorPayload.error : String(res.status),
      retryAfterSeconds: parseRetryAfter(res.headers.get('Retry-After')),
    }
  }

  private async request<T>(
    path: string,
    validate: (value: unknown) => value is T,
    options: RequestOptions = {},
  ): Promise<ApiResult<T> | null> {
    const init = this.buildRequestInit(options)

    try {
      const res = await (this.fetchFn ? this.fetchFn(path, init) : fetch(path, init))
      if (!res.ok) {
        return await this.toHttpFailure(res, options.handleUnauthorized === true)
      }
      // Do not fall back to {} on 2xx: truncated streams, mid-body aborts, or malformed payloads
      // leave mutation outcomes uncertain and would poison AppState (e.g. counts = undefined).
      const parsed = await readSuccessJson(res)
      if (!parsed.ok || !validate(parsed.value)) {
        return {
          ok: false,
          status: res.status,
          error: 'invalid_response',
          retryAfterSeconds: null,
        }
      }
      return { ok: true, status: res.status, data: parsed.value }
    } catch (error) {
      if (isAbortError(error)) {
        return null
      }
      return { ok: false, status: 0, error: 'network_error', retryAfterSeconds: null }
    }
  }

  async checkSession(): Promise<ApiResult<SessionInfo>> {
    const res = await this.request('/v1/admin/session', isSessionInfo)
    return res ?? { ok: false, status: 0, error: 'network_error', retryAfterSeconds: null }
  }

  async login(password: string): Promise<ApiResult<LoginInfo>> {
    const res = await this.request('/v1/admin/login', isLoginInfo, {
      method: 'POST',
      body: { password },
    })
    return res ?? { ok: false, status: 0, error: 'network_error', retryAfterSeconds: null }
  }

  async logout(): Promise<ApiResult<{ readonly ok: true }>> {
    const res = await this.request('/v1/admin/logout', isLogoutResponse, { method: 'POST' })
    return res ?? { ok: false, status: 0, error: 'network_error', retryAfterSeconds: null }
  }

  listDonations(
    params: { readonly after?: string | null; readonly filter?: AdminDonationsFilter },
    signal?: AbortSignal,
  ): Promise<ApiResult<AdminDonationsPage> | null> {
    const query = new URLSearchParams()
    if (params.after) query.set('after', params.after)
    if (params.filter?.visibility) query.set('visibility', params.filter.visibility)
    if (params.filter?.provider) query.set('provider', params.filter.provider)
    if (params.filter?.query) query.set('q', params.filter.query)
    const suffix = query.size > 0 ? `?${query.toString()}` : ''
    return this.request(`/v1/admin/donations${suffix}`, isAdminDonationsPage, {
      handleUnauthorized: true,
      ...(signal === undefined ? {} : { signal }),
    })
  }

  moderate(id: string, command: AdminVisibilityCommand): Promise<ApiResult<ModerateResponse> | null> {
    return this.request(
      `/v1/admin/donations/${encodeURIComponent(id)}/visibility`,
      isModerateResponse,
      { method: 'POST', body: command, handleUnauthorized: true },
    )
  }

  createManualDonation(
    payload: ManualDonationPayload,
    idempotencyKey: string,
  ): Promise<ApiResult<ModerateResponse> | null> {
    return this.request('/v1/admin/donations', isModerateResponse, {
      method: 'POST',
      body: payload,
      headers: { 'Idempotency-Key': idempotencyKey },
      handleUnauthorized: true,
    })
  }

  syncAfdian(): Promise<ApiResult<SyncAfdianResponse> | null> {
    return this.request('/v1/admin/donations/sync-afdian', isSyncAfdianResponse, {
      method: 'POST',
      handleUnauthorized: true,
    })
  }

  patchConfig(patch: AdminConfigPatch): Promise<ApiResult<{ readonly config: DonationConfig }> | null> {
    return this.request('/v1/admin/donations/config', isConfigResponse, {
      method: 'PATCH',
      body: patch,
      handleUnauthorized: true,
    })
  }
}
