import { message } from '../ui/locale'
import { relayCopy } from './locales'
import { request, RequestError } from '../ui/request'

export interface Capabilities {
  contribution?: boolean
  managed_turn?: boolean
  revocation_stops_shares?: boolean
  password_change?: boolean
}
export interface ContributionPolicy {
  enabled: boolean
  daily_bytes: number
  bytes_per_second: number
  connections: number
  host_uplink: number | null
}
export interface ContributionSettings {
  policy: ContributionPolicy
  ceiling: { daily_bytes?: number; bytes_per_second?: number; connections?: number }
}
export interface Enrollment {
  enrollment_id: string
  user_code: string
  device_name: string
  expires_at: number
}
export interface Credential {
  credential_id: string
  name: string
  last_used_at: number | null
  revoked_at: number | null
}
export interface CredentialPage { credentials: Credential[]; next_cursor: string | null }
export interface ServiceCredential { url: string; access_key: string }

const messages = {
  invalid_password: message(relayCopy, 'invalidPassword'),
  initialization_consumed: message(relayCopy, 'setupLinkUsed'),
  slow_down: message(relayCopy, 'slowDown'),
  access_denied: message(relayCopy, 'accessDenied'),
  already_completed: message(relayCopy, 'alreadyCompleted'),
  credential_capacity: message(relayCopy, 'credentialLimit'),
  invalid_name: message(relayCopy, 'invalidName'),
}

export class RelayClient {
  constructor(private readonly expired: () => void) {}

  private async call<T>(path: string, data?: unknown, signal?: AbortSignal) {
    try {
      return await request<T>('/api/' + path, { data, signal, messages })
    } catch (error) {
      if (!signal?.aborted && error instanceof RequestError && error.code === 'login_required') this.expired()
      throw error
    }
  }

  session = async (signal: AbortSignal) => {
    try {
      const result = await request<{ capabilities: Capabilities }>('/api/session', { signal, messages })
      return result.capabilities
    } catch (error) {
      if (error instanceof RequestError && error.code === 'uninitialized') return 'uninitialized' as const
      if (error instanceof RequestError && error.code === 'login_required') return null
      throw error
    }
  }
  signIn(password: string, token: string | null) {
    return this.call(token ? 'initialize' : 'login', token ? { password, token } : { password })
  }
  contribution = (signal: AbortSignal) => this.call<ContributionSettings>('contribution', undefined, signal)
  saveContribution = (policy: ContributionPolicy) => this.call<{ disabled_notice: boolean }>('contribution', policy)
  turnStatus = (signal: AbortSignal) => this.call<{ enabled: boolean }>('turn/status', undefined, signal)
  toggleTURN = (enabled: boolean) => this.call<{ enabled: boolean }>('turn/toggle', { enabled })
  signOut = () => this.call('logout', {})
  changePassword = (currentPassword: string, newPassword: string) =>
    this.call('password', { current_password: currentPassword, new_password: newPassword })
  enrollments = async (signal: AbortSignal) => (await this.call<{ enrollments: Enrollment[] }>('enrollments', undefined, signal)).enrollments
  approve = (device: Enrollment) => this.call('enrollments/approve', { enrollment_id: device.enrollment_id, user_code: device.user_code })
  activeCredentials = async (signal: AbortSignal): Promise<CredentialPage> => ({
    ...await this.call<{ credentials: Credential[] }>('credentials', undefined, signal),
    next_cursor: null,
  })
  revokedCredentials = (signal: AbortSignal, after?: string) => this.call<CredentialPage>(
    after === undefined ? 'credentials/revoked' : 'credentials/revoked?after=' + encodeURIComponent(after), undefined, signal,
  )
  createKey = async (name: string) => (await this.call<{ configuration: ServiceCredential }>('credentials', { name })).configuration
  revoke = (id: string, stop: boolean) => this.call('credentials/revoke', { credential_id: id, terminate_shares: stop })
}
