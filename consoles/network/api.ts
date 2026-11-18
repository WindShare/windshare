import { request, RequestError } from '../ui/request'
import type { Enrollment, PolicyField, RelayNode, Restrictions, Settings, Snapshot, Target } from './model'

const messages = {
  credentials: 'The username or password was not accepted.',
  initialization_consumed: 'This setup link is no longer available. Use your administrator credentials.',
  rate_limited: 'Too many requests. Please try again later.',
  invalid: 'The service could not accept these values. Check the form and try again.',
  csrf: 'Your session has changed. Refresh the console and try again.',
}

export class AuthorityClient {
  private csrf = ''
  constructor(private readonly expired: () => void) {}

  snapshot = async (signal: AbortSignal) => {
    try {
      const snapshot = await request<Snapshot>('/v1/admin/console', { signal, messages })
      if (!signal.aborted) this.csrf = snapshot.csrf
      return snapshot
    } catch (error) {
      if (error instanceof RequestError && error.code === 'administrator_session') return null
      throw error
    }
  }
  signIn(username: string, password: string, token: string | null) {
    return request('/v1/admin/' + (token ? 'initialize' : 'login'), {
      data: token ? { username, password, token } : { username, password }, messages,
    })
  }
  private async write<T>(route: string, data: unknown, method = 'POST') {
    try {
      return await request<T>('/v1/admin/' + route, {
        method, data, messages, headers: { 'X-CSRF-Token': this.csrf },
      })
    } catch (error) {
      if (error instanceof RequestError && error.code === 'administrator_session') this.expired()
      throw error
    }
  }
  signOut = () => this.write('logout', {})
  enroll = (node: Enrollment) => this.write<RelayNode>('nodes', node)
  policy = (key: number[], field: PolicyField, value: boolean) => this.write<RelayNode>('nodes/policy', { key, change: { field, value } }, 'PUT')
  settings = (value: Settings) => this.write('settings', value, 'PUT')
  restrict = (target: Target, blocked: boolean) => this.write<Restrictions>('blocks', { ...target, blocked })
}
