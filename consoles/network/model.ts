export type Zone = 'cn' | 'global'
export interface Enrollment { key: number[]; zone: Zone; region: string }
export interface RelayNode extends Enrollment { enabled: boolean; anonymous: boolean; version: number }
export type PolicyField = 'public_relay' | 'anonymous_sharing'

export interface LoadReport {
  node: string
  created_at: number
  reported_at: number | null
  load: { active_sessions: number; forwarded_bytes: number; admission_refusals: number; wire_bytes: number }
}
export interface Publication {
  generation: number; serial: number; expires: number; keys: string[]; digest: string; mirror: string
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
export interface Snapshot extends PolicySnapshot {
  csrf: string
  nodes: RelayNode[]
  load: LoadReport[]
  directory: Publication | null
  restrictions: Restrictions
  email_configured: boolean
  account_count: number
  reviews: number[]
}
export const GIB = 1024 ** 3
