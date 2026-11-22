import type {
  AdminConfigPatch,
  AdminDonationsFilter,
  AdminDonationsPage,
  AdminVisibilityCommand,
  AdminVisibilityCounts,
  DonationConfig,
  DonationProvider,
  DonationRecord,
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

function parseRetryAfter(header: string | null): number | null {
  if (header === null) return null
  const value = Number(header.trim())
  return Number.isFinite(value) && value > 0 ? Math.ceil(value) : null
}

export function formatApiError(failure: ApiFailure, t: CopyCatalog): string {
  if (failure.status === 0) return t.errors.network
  switch (failure.error) {
    case 'invalid_credentials':
      return t.errors.invalidCredentials
    case 'rate_limited':
      return t.errors.rateLimited(failure.retryAfterSeconds)
    case 'admin_not_configured':
      return t.errors.adminNotConfigured
    case 'afdian_not_configured':
      return t.errors.afdianNotConfigured
    case 'afdian_query_failed':
      return t.errors.afdianQueryFailed
    case 'afdian_rate_limited':
      return t.errors.afdianRateLimited
    case 'invalid_amount':
      return t.errors.invalidAmount
    case 'invalid_manual_donation':
      return t.errors.invalidManualDonation
    case 'duplicate_external_id':
      return t.errors.duplicateExternalId
    case 'service_unavailable':
      return t.errors.serviceUnavailable
    default:
      return t.errors.fallback(failure.error || String(failure.status))
  }
}

export class AdminApiClient {
  private csrfToken = ''
  private readonly onSessionExpired: () => void

  constructor(onSessionExpired: () => void) {
    this.onSessionExpired = onSessionExpired
  }

  setCsrf(token: string): void {
    this.csrfToken = token
  }

  clearCsrf(): void {
    this.csrfToken = ''
  }

  private async request<T>(
    path: string,
    options: {
      readonly method?: 'GET' | 'POST' | 'PATCH'
      readonly body?: unknown
      readonly signal?: AbortSignal
      readonly handleUnauthorized?: boolean
    } = {},
  ): Promise<ApiResult<T> | null> {
    const method = options.method ?? 'GET'
    const headers: Record<string, string> = {}
    if (options.body !== undefined) {
      headers['Content-Type'] = 'application/json'
    }
    if (method !== 'GET' && this.csrfToken.length > 0) {
      headers['X-CSRF-Token'] = this.csrfToken
    }

    try {
      const res = await fetch(path, {
        method,
        headers,
        credentials: 'same-origin',
        ...(options.body === undefined ? {} : { body: JSON.stringify(options.body) }),
        ...(options.signal === undefined ? {} : { signal: options.signal }),
      })
      const payload = (await res.json().catch(() => ({}))) as Record<string, unknown>
      if (!res.ok) {
        if (res.status === 401 && options.handleUnauthorized) {
          this.clearCsrf()
          this.onSessionExpired()
          return null
        }
        return {
          ok: false,
          status: res.status,
          error: typeof payload.error === 'string' ? payload.error : String(res.status),
          retryAfterSeconds: parseRetryAfter(res.headers.get('Retry-After')),
        }
      }
      return { ok: true, status: res.status, data: payload as unknown as T }
    } catch (error) {
      if (error instanceof DOMException && error.name === 'AbortError') {
        return null
      }
      return { ok: false, status: 0, error: 'network_error', retryAfterSeconds: null }
    }
  }

  async checkSession(): Promise<ApiResult<SessionInfo>> {
    const res = await this.request<SessionInfo>('/v1/admin/session')
    return res ?? { ok: false, status: 0, error: 'network_error', retryAfterSeconds: null }
  }

  async login(password: string): Promise<ApiResult<LoginInfo>> {
    const res = await this.request<LoginInfo>('/v1/admin/login', {
      method: 'POST',
      body: { password },
    })
    return res ?? { ok: false, status: 0, error: 'network_error', retryAfterSeconds: null }
  }

  async logout(): Promise<ApiResult<{ readonly ok: true }>> {
    const res = await this.request<{ readonly ok: true }>('/v1/admin/logout', { method: 'POST' })
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
    return this.request<AdminDonationsPage>(`/v1/admin/donations${suffix}`, {
      handleUnauthorized: true,
      ...(signal === undefined ? {} : { signal }),
    })
  }

  moderate(id: string, command: AdminVisibilityCommand): Promise<ApiResult<ModerateResponse> | null> {
    return this.request<ModerateResponse>(
      `/v1/admin/donations/${encodeURIComponent(id)}/visibility`,
      { method: 'POST', body: command, handleUnauthorized: true },
    )
  }

  createManualDonation(payload: ManualDonationPayload): Promise<ApiResult<ModerateResponse> | null> {
    return this.request<ModerateResponse>('/v1/admin/donations', {
      method: 'POST',
      body: payload,
      handleUnauthorized: true,
    })
  }

  syncAfdian(): Promise<ApiResult<SyncAfdianResponse> | null> {
    return this.request<SyncAfdianResponse>('/v1/admin/donations/sync-afdian', {
      method: 'POST',
      handleUnauthorized: true,
    })
  }

  patchConfig(patch: AdminConfigPatch): Promise<ApiResult<{ readonly config: DonationConfig }> | null> {
    return this.request<{ readonly config: DonationConfig }>('/v1/admin/donations/config', {
      method: 'PATCH',
      body: patch,
      handleUnauthorized: true,
    })
  }
}
