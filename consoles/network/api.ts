import { message, type Message } from '../ui/locale'
import { networkCopy } from './locales'
import { request, RequestError, type FaultMessage } from '../ui/request'
import { decode, encode } from './identity'
import type { AccountDetails, AccountPage, AccountSummary, Discovery, Enrollment, TrafficStatus, MilestoneReview, NodeDetails, NodePage, FleetAction, PolicyChange, PolicySnapshot, RelayNode, Report, ReportPage, Restrictions, SettingsChanges, Snapshot, Target } from './model'

function nodeHttpStatus(fault: Readonly<Record<string, unknown>>): Message {
  const status = typeof fault.status === 'number' && Number.isInteger(fault.status) ? fault.status : null
  return status === null
    ? message(networkCopy, 'nodeHttpError')
    : { format: locale => networkCopy[locale].nodeHttpStatus(status) }
}

const messages: Readonly<Record<string, FaultMessage>> = {
  relay_bound: message(networkCopy, 'relayBound'),
  stun_bound: message(networkCopy, 'stunBound'),
  listing_changed: message(networkCopy, 'listingChanged'),
  alias_bound: message(networkCopy, 'aliasBound'),
  node_settings_changed: message(networkCopy, 'nodeSettingsChanged'),
  vantage_key_bound: message(networkCopy, 'vantageKeyBound'),
  verification_required: message(networkCopy, 'verificationRequired'),
  drain_pending: message(networkCopy, 'drainPending'),
  maintenance_in_progress: message(networkCopy, 'maintenance'),
  mirror_lag: message(networkCopy, 'mirrorLag'),
  invalid_node_url: message(networkCopy, 'invalidNodeUrl'),
  reserved_node_host: message(networkCopy, 'reservedNodeHost'),
  node_dns_failed: message(networkCopy, 'nodeDnsFailed'),
  node_non_public_address: message(networkCopy, 'nodeNonPublicAddress'),
  node_unreachable: message(networkCopy, 'nodeUnreachable'),
  node_redirect_refused: message(networkCopy, 'nodeRedirectRefused'),
  node_http_status: nodeHttpStatus,
  node_invalid_descriptor: message(networkCopy, 'nodeInvalidDescriptor'),
  node_descriptor_expired: message(networkCopy, 'nodeDescriptorExpired'),
  node_descriptor_mismatch: message(networkCopy, 'nodeDescriptorMismatch'),
  credentials: message(networkCopy, 'invalidCredentials'),
  credentials_changed: message(networkCopy, 'sessionChanged'),
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

  private async write<T>(route: string, data: unknown, method = 'POST', overrides?: Readonly<Record<string, FaultMessage>>) {
    try {
      return await request<T>('/v1/admin/' + route, {
        method, data, messages: overrides ? { ...messages, ...overrides } : messages,
        headers: { 'X-CSRF-Token': this.csrf },
      })
    } catch (error) {
      if (error instanceof RequestError && error.code === 'administrator_session') this.expired()
      throw error
    }
  }
  signOut = () => this.write('logout', {})
  changePassword = async (currentPassword: string, newPassword: string) => {
    const result = await this.write<{ csrf: string }>('password', {
      current_password: currentPassword,
      new_password: newPassword,
    }, 'POST', { credentials: message(networkCopy, 'invalidCurrentPassword') })
    this.csrf = result.csrf
  }
  discover = (url: string) => this.write<Discovery>('nodes/discover', { url })
  promote = (node_key: string, operation_id: string, status: TrafficStatus) => this.write('nodes/promote', { node_key, operation_id, status })
  manage = (node_key: string, operation_id: string, action: FleetAction) => this.write('nodes/manage', { node_key, operation_id, action })
  cancelDraft = (node_key: string, operation_id: string) => this.write('nodes/cancel', { node_key, operation_id })
  enroll = (node: Enrollment) => this.write<NodeDetails>('nodes', node)
  policy = (key: number[], change: PolicyChange) => this.write<RelayNode>('nodes/policy', { key, change }, 'PUT')
  updateSettings = (changes: SettingsChanges) => this.write<PolicySnapshot>('settings', changes, 'PATCH')
  dismissReview = (count: number) => this.write<MilestoneReview[]>('reviews/dismiss', { count })
  restrict = (target: Target, blocked: boolean) => this.write<Restrictions>('blocks', { ...target, blocked })
}
