export type Zone = 'cn' | 'global'
export type Role = 'relay' | 'stun'
export interface Discovery { node_key: string; relay: string; roles: Role[]; stun: string[]; zone: Zone; region: string; capacity_mbps: number | null }
export interface Enrollment { url: string; node_key: string; alias: string; provider: string; zone: Zone; region: string; capacity_mbps: number; roles: Role[] }
export type Admission = 'accepting' | 'draining' | 'disabled'
export type FleetAction =
  | { kind: 'activate' | 'drain' }
  | { kind: 'disable' | 'remove'; early_cutoff: boolean }
export type NodeSpec = Omit<Enrollment, 'url'> & { relay: string }
export type NodeState = 'canary' | 'active' | 'draining' | 'disabled'
export interface RelayNode {
  key: number[]
  spec: NodeSpec
  state: NodeState
  admission: Admission
  anonymous: boolean
  version: number
}
export type PolicyChange = { field: 'anonymous_sharing'; value: boolean }

export function servesRelay(node: Pick<RelayNode, 'spec'>): boolean {
  return node.spec.roles.includes('relay')
}

export interface LoadReport {
  created_at: number
  reported_at: number
  load: { resident_shares: number; applied_policy_version: number; relay_budget?: { bytes: number; reset_day_utc: number }; active_sessions: number; forwarded_bytes: number; admission_refusals: number; wire_bytes: number }
}
export interface NodeDetails extends RelayNode { report: LoadReport | null; cutoff_ready: boolean; report_stale: boolean }
export interface NodePage { entries: NodeDetails[]; next_cursor: string | null }

export interface AccountSummary {
  id: number[]
  created_at: number
  username: string | null
  nickname: string | null
  email: string | null
  has_password: boolean
  today_bytes: number
}
export interface AccountPage { today_day: number; entries: AccountSummary[]; next_cursor: string | null }
export interface DailyUsage { day: number; bytes: number }
export interface AccountDetails extends AccountSummary {
  today_day: number
  daily_limit_bytes: number
  remaining_bytes: number
  restricted: boolean
  restriction_version: number
  entitlement_version: number
  recent_usage: DailyUsage[]
  has_older_usage: boolean
  shares: number[][]
  has_more_shares: boolean
}

export interface NodeSetup { url: string; issuer: string; certificate: string }
export interface Publication {
  generation: number; serial: number; expires: number; keys: string[]; digest: string; kv_sync_pending: boolean; expiring_certificates: { ticket: number | null; directory: number | null }
  setup?: NodeSetup
}
export type RegistrationMode = 'automatic' | 'proof_of_work' | 'email'
export interface Settings {
  registration: 'by_account_count' | RegistrationMode
  pow_bits: number
  daily_bytes: number
  source_rate: Rate
  global_rate: Rate
}
export interface Rate { burst: number; refill_seconds: number }
export type SettingsChanges = Partial<Pick<Settings, 'registration' | 'pow_bits' | 'daily_bytes'>> & {
  source_rate?: Partial<Rate>
  global_rate?: Partial<Rate>
}
export interface PolicySnapshot {
  settings: Settings
  registration_mode: RegistrationMode
}
export interface Report { id: number[]; share: number[]; reason: string; created_at: number }
export interface ReportPage { reports: Report[]; next_cursor: string | null }
export type Target = { kind: 'share' | 'account'; id: number[] }
export interface Restrictions { version: number; shares: number[][]; accounts: number[][] }
export interface MilestoneReview { count: number; reached_at: number }
export interface Snapshot extends PolicySnapshot {
  username: string | null
  csrf: string
  node_page: NodePage
  directory: Publication
  restrictions: Restrictions
  email_configured: boolean
  account_count: number
  reviews: MilestoneReview[]
}
export const GIB = 1024 ** 3
