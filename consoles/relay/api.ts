import { request, RequestError } from '../ui/request'

export interface Capabilities {
  revocation_stops_shares?: boolean
  managed_turn?: boolean
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
  revoked: boolean
}
export interface ServiceConfiguration { url: string; access_key: string }
export interface TurnStatus {
  ready: boolean
  enabled: boolean
  analytics_available: boolean
  leases: Partial<Record<'active' | 'revocation_pending' | 'revoked' | 'expired', number>>
}
export interface TurnUsage { first_date: string; last_date: string; ingress_bytes: number; egress_bytes: number }

const messages: Record<string, string> = {
  invalid_password: 'The password was not accepted.',
  initialization_consumed: 'This setup link is no longer available. Sign in with the owner password.',
  slow_down: 'Please wait before trying again.',
  access_denied: 'This request is no longer available. Check progress on the device.',
  already_completed: 'This request has already been completed. Check progress on the device.',
  credential_capacity: 'This node has reached its credential limit.',
  invalid_name: 'Enter a short, descriptive name.',
  analytics_unavailable: 'Usage estimates are currently unavailable.',
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
      if (error instanceof RequestError && error.code === 'login_required') return null
      throw error
    }
  }
  signIn(password: string, token: string | null) {
    return this.call(token ? 'initialize' : 'login', token ? { password, token } : { password })
  }
  signOut = () => this.call('logout', {})
  enrollments = async (signal: AbortSignal) => (await this.call<{ enrollments: Enrollment[] }>('enrollments', undefined, signal)).enrollments
  approve = (device: Enrollment) => this.call('enrollments/approve', { enrollment_id: device.enrollment_id, user_code: device.user_code })
  credentials = async (signal: AbortSignal) => (await this.call<{ credentials: Credential[] }>('credentials', undefined, signal)).credentials
  createKey = async (name: string) => (await this.call<{ configuration: ServiceConfiguration }>('credentials', { name })).configuration
  revoke = (id: string, stop: boolean) => this.call('credentials/revoke', { credential_id: id, terminate_shares: stop })
  turnStatus = (signal: AbortSignal) => this.call<TurnStatus>('turn/status', undefined, signal)
  setTurn = (enabled: boolean) => this.call<TurnStatus>('turn/settings', { enabled })
  turnUsage = () => this.call<TurnUsage>('turn/usage')
}
