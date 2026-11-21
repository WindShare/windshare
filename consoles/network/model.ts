export type Zone = 'cn' | 'global'
export type Role = 'relay' | 'stun'
export type TrafficStatus = 'canary' | 'active'
export interface Discovery { node_key: string; relay: string; roles: Role[]; stun: string[]; zone: Zone; region: string; capacity_mbps: number | null }
export interface Enrollment { url: string; node_key: string; alias: string; provider: string; zone: Zone; region: string; capacity_mbps: number; roles: Role[] }
export interface Listing {
  spec: Omit<Enrollment, 'url'> & { relay: string }
  state: { kind: 'listed'; status: TrafficStatus | 'draining' } | { kind: 'staging'; check:
    { kind: 'queued' | 'running' } | { kind: 'passed'; at: number } | { kind: 'failed'; reason: string } }
}
export interface RelayNode { key: number[]; zone: Zone; region: string; enabled: boolean; anonymous: boolean; version: number; listing: Listing | null }
export type PolicyField = 'public_relay' | 'anonymous_sharing'

export interface LoadReport {
  created_at: number
  reported_at: number | null
  load: { relay_budget?: { bytes: number; reset_day_utc: number }; active_sessions: number; forwarded_bytes: number; admission_refusals: number; wire_bytes: number }
}
export interface NodeDetails extends RelayNode { report: LoadReport | null }
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

export interface Publication {
  generation: number; serial: number; expires: number; keys: string[]; digest: string; mirrors_current: boolean; expiring_certificates: { ticket: number | null; directory: number | null }; silent_vantages: string[]
  setup: { url: string; issuer: string; certificate: string }
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
  csrf: string
  node_page: NodePage
  directory: Publication | null
  restrictions: Restrictions
  email_configured: boolean
  account_count: number
  reviews: MilestoneReview[]
}
export const GIB = 1024 ** 3
