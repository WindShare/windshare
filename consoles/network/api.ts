import { message } from '../ui/locale'
import { networkCopy } from './locales'
import { request, RequestError } from '../ui/request'
import { decode, encode } from './identity'
import type { AccountDetails, AccountPage, AccountSummary, Enrollment, NodeDetails, NodePage, PolicyField, PolicySnapshot, RelayNode, Report, ReportPage, Restrictions, SettingsChanges, Snapshot, Target } from './model'

const messages = {
  credentials: message(networkCopy, 'invalidCredentials'),
  initialization_consumed: message(networkCopy, 'setupLinkUsed'),
  rate_limited: message(networkCopy, 'rateLimited'),
  account_not_found: message(networkCopy, 'accountNotFound'),
  invalid: message(networkCopy, 'invalidValues'),
  csrf: message(networkCopy, 'sessionChanged'),
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
  private async read<T>(route: string, signal: AbortSignal) {
    try {
      return await request<T>('/v1/admin/' + route, { signal, messages })
    } catch (error) {
      if (!signal.aborted && error instanceof RequestError && error.code === 'administrator_session') this.expired()
      throw error
    }
  }
  nodes = (after: string, signal: AbortSignal) =>
    this.read<NodePage>('nodes?after=' + encodeURIComponent(after), signal)
  node = async (id: string, signal: AbortSignal) => {
    if (!decode(id, 'node')) return null
    try {
      return await this.read<NodeDetails>('nodes/' + encodeURIComponent(id.trim()), signal)
    } catch (error) {
      if (error instanceof RequestError && error.code === 'node_not_found') return null
      throw error
    }
  }

  accounts = (signal: AbortSignal, after?: string) =>
    this.read<AccountPage>(after === undefined ? 'accounts' : 'accounts?after=' + encodeURIComponent(after), signal)
  lookupAccount = async (query: string, signal: AbortSignal) => {
    try {
      return await this.read<AccountSummary>('accounts/lookup?q=' + encodeURIComponent(query), signal)
    } catch (error) {
      if (error instanceof RequestError && error.code === 'account_not_found') return null
      throw error
    }
  }
  account = async (id: number[], signal: AbortSignal) => {
    try {
      return await this.read<AccountDetails>('accounts/' + encode(id), signal)
    } catch (error) {
      if (error instanceof RequestError && error.code === 'account_not_found') return null
      throw error
    }
  }

  reports = (signal: AbortSignal, after?: string) =>
    this.read<ReportPage>(after === undefined ? 'reports' : 'reports?after=' + encodeURIComponent(after), signal)
  report = async (id: string, signal: AbortSignal) => {
    try {
      return await this.read<Report>('reports/' + encodeURIComponent(id), signal)
    } catch (error) {
      if (error instanceof RequestError && error.code === 'report_not_found') return null
      throw error
    }
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
  enroll = (node: Enrollment) => this.write<NodeDetails>('nodes', node)
  policy = (key: number[], field: PolicyField, value: boolean) => this.write<RelayNode>('nodes/policy', { key, change: { field, value } }, 'PUT')
  updateSettings = (changes: SettingsChanges) => this.write<PolicySnapshot>('settings', changes, 'PATCH')
  restrict = (target: Target, blocked: boolean) => this.write<Restrictions>('blocks', { ...target, blocked })
}
