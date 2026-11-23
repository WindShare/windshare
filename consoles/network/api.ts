import { message, MessageError, type Message } from '../ui/locale'
import { networkCopy } from './locales'
import { request, RequestError, type FaultMessage } from '../ui/request'
import { decode, encode } from './identity'
import { derivePasswordVerifier, isValidLoginPassword, isValidNewPassword, normalizeUsername } from './auth/password'
import type { AccountDetails, AccountPage, AccountSummary, Discovery, Enrollment, MilestoneReview, NodeDetails, NodePage, FleetAction, PolicyChange, PolicySnapshot, Report, ReportPage, Restrictions, SettingsChanges, Snapshot, Target } from './model'

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
  drain_pending: message(networkCopy, 'drainPending'),
  invalid_node_url: message(networkCopy, 'invalidNodeUrl'),
  reserved_node_host: message(networkCopy, 'reservedNodeHost'),
  node_dns_failed: message(networkCopy, 'nodeDnsFailed'),
  node_non_public_address: message(networkCopy, 'nodeNonPublicAddress'),
  node_unreachable: message(networkCopy, 'nodeUnreachable'),
  node_redirect_refused: message(networkCopy, 'nodeRedirectRefused'),
  node_http_status: nodeHttpStatus,
  node_invalid_descriptor: message(networkCopy, 'nodeInvalidDescriptor'),
  authority_mismatch: message(networkCopy, 'authorityMismatch'),
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
  private username: string | null = null
  constructor(private readonly expired: () => void) {}

  snapshot = async (signal: AbortSignal) => {
    try {
      const snapshot = await request<Snapshot>('/v1/admin/console', { signal, messages })
      if (!signal.aborted) {
        this.csrf = snapshot.csrf
        this.username = snapshot.username
      }
      return snapshot
    } catch (error) {
      if (error instanceof RequestError && error.code === 'administrator_session') return null
      throw error
    }
  }
  async signIn(username: string, password: string, token: string | null) {
    const canonical = normalizeUsername(username)
    if (!canonical) throw new MessageError(message(networkCopy, 'invalidValues'))
    if (token) {
      if (!isValidNewPassword(password)) throw new MessageError(message(networkCopy, 'invalidValues'))
    } else if (!isValidLoginPassword(password)) {
      throw new MessageError(message(networkCopy, 'invalidCredentials'))
    }
    const password_verifier = await derivePasswordVerifier('admin', canonical, password)
    return request('/v1/admin/' + (token ? 'initialize' : 'login'), {
      data: token ? { username: canonical, password_verifier, token } : { username: canonical, password_verifier },
      messages,
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
    const username = this.username
    if (!username) throw new MessageError(message(networkCopy, 'sessionChanged'))
    if (!isValidNewPassword(newPassword)) throw new MessageError(message(networkCopy, 'invalidValues'))
    if (!isValidLoginPassword(currentPassword)) throw new MessageError(message(networkCopy, 'invalidCurrentPassword'))
    const [current_password_verifier, new_password_verifier] = await Promise.all([
      derivePasswordVerifier('admin', username, currentPassword),
      derivePasswordVerifier('admin', username, newPassword),
    ])
    const result = await this.write<{ csrf: string }>('password', {
      current_password_verifier,
      new_password_verifier,
    }, 'POST', { credentials: message(networkCopy, 'invalidCurrentPassword') })
    this.csrf = result.csrf
  }
  discover = (url: string) => this.write<Discovery>('nodes/discover', { url })
  manage = (node: NodeDetails, action: FleetAction) => this.write('nodes/manage', {
    node_key: node.spec.node_key, expected_state: node.state, expected_policy_version: node.version, action,
  })
  enroll = (node: Enrollment) => this.write<NodeDetails>('nodes', node)
  policy = (key: number[], change: PolicyChange) => this.write<NodeDetails>('nodes/policy', { key, change }, 'PUT')
  updateSettings = (changes: SettingsChanges) => this.write<PolicySnapshot>('settings', changes, 'PATCH')
  dismissReview = (count: number) => this.write<MilestoneReview[]>('reviews/dismiss', { count })
  restrict = (target: Target, blocked: boolean) => this.write<Restrictions>('blocks', { ...target, blocked })
}
