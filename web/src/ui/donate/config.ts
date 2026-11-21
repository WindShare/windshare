import { backup_entry, link_host } from '../../../../deploy/network/release.json'

export const DONATE_API_ORIGIN = 'https://donate.windshare.cc'
export const AFDIAN_ORDER_URL = 'https://afdian.com/order/create?user_id=735de47ce57c11f1b2c95254001e7c00'
export const KOFI_PAGE_URL = 'https://ko-fi.com/windshare'

export const OFFICIAL_DONATE_ORIGINS: ReadonlySet<string> = new Set(
  [link_host, backup_entry].map(entry => new URL(entry).origin),
)

export function canCreateCryptoIntent(origin: string): boolean {
  if (OFFICIAL_DONATE_ORIGINS.has(origin)) return true
  try {
    const { hostname } = new URL(origin)
    return hostname === 'localhost' || hostname === '127.0.0.1'
  } catch {
    return false
  }
}
