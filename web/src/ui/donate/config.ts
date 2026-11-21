import { backup_entry, link_host } from '../../../../deploy/network/release.json'

export const DONATE_API_ORIGIN = 'https://donate.windshare.cc'
export const AFDIAN_ORDER_URL = 'https://afdian.com/order/create?user_id=735de47ce57c11f1b2c95254001e7c00'
export const KOFI_PAGE_URL = 'https://ko-fi.com/windshare'
export const OFFICIAL_DONATE_PAGE_URL = `${new URL(backup_entry).origin}/donate`

export const OFFICIAL_DONATE_ORIGINS: ReadonlySet<string> = new Set(
  [link_host, backup_entry].map(entry => new URL(entry).origin),
)

export interface FallbackWallet {
  readonly ticker: string
  readonly network: string
  readonly address: string
}

// Isolated self-hosted wallets shown only on self-deployed origins where hosted
// Crypto checkout intents are not allowed by CORS origin policy.
export const SELF_HOST_FALLBACK_WALLETS: readonly FallbackWallet[] = Object.freeze([
  {
    ticker: 'USDC / SOL',
    network: 'Solana',
    address: 'A5ZvjhS8rbajgEoZ5MPMeFKbMVBjTngJE2KDqSFd8YZU',
  },
  {
    ticker: 'USDT',
    network: 'TRON · TRC20',
    address: 'THJzAL1ZQuVrn5cGPaecCnp7PfTr9Eh4mm',
  },
  {
    ticker: 'BTC',
    network: 'Bitcoin',
    address: 'bc1p8f5ar05p08e3ut8k6r25v0tu5tewq4qw7mnx87w34m9fzpkqpc7saq3aeh',
  },
  {
    ticker: 'ETH',
    network: 'Ethereum',
    address: '0x5eebb308017317b059b11b00aa90ad2a21feb256',
  },
])

export function canCreateCryptoIntent(origin: string): boolean {
  if (OFFICIAL_DONATE_ORIGINS.has(origin)) return true
  try {
    const { hostname } = new URL(origin)
    return hostname === 'localhost' || hostname === '127.0.0.1'
  } catch {
    return false
  }
}
