// Hosted donation backend contract (services/donations/wrangler.toml).
// Kept independent from deploy/network/release.json so forks and custom
// networks do not misclassify their own frontend origins as allowed by
// donate.windshare.cc CORS policy.
export const DONATE_API_ORIGIN = 'https://donate.windshare.cc'
export const AFDIAN_ORDER_URL = 'https://afdian.com/order/create?user_id=735de47ce57c11f1b2c95254001e7c00'
export const KOFI_PAGE_URL = 'https://ko-fi.com/windshare'
export const OFFICIAL_DONATE_PAGE_URL = 'https://windshare.cc/donate'

export const OFFICIAL_DONATE_ORIGINS: ReadonlySet<string> = new Set([
  'https://ws.ourworld.top',
  'https://windshare.cc',
])

export type CryptoWalletId = 'btc' | 'eth' | 'sol' | 'sui' | 'bch' | 'usdt'

export interface DirectCryptoWallet {
  readonly id: CryptoWalletId
  readonly ticker: string
  readonly name: string
  readonly network: string
  readonly address: string
}

export const DEFAULT_DIRECT_WALLET_ID: CryptoWalletId = 'btc'

// Isolated self-custody wallets shared across official direct transfers and
// self-hosted origins where hosted NOWPayments intents are disallowed by CORS.
export const DIRECT_CRYPTO_WALLETS: readonly DirectCryptoWallet[] = Object.freeze([
  {
    id: 'btc',
    ticker: 'BTC',
    name: 'Bitcoin',
    network: 'Bitcoin',
    address: 'bc1p8f5ar05p08e3ut8k6r25v0tu5tewq4qw7mnx87w34m9fzpkqpc7saq3aeh',
  },
  {
    id: 'eth',
    ticker: 'ETH',
    name: 'Ethereum',
    network: 'Ethereum',
    address: '0x5eebb308017317b059b11b00aa90ad2a21feb256',
  },
  {
    id: 'sol',
    ticker: 'SOL',
    name: 'Solana',
    network: 'Solana',
    address: 'A5ZvjhS8rbajgEoZ5MPMeFKbMVBjTngJE2KDqSFd8YZU',
  },
  {
    id: 'sui',
    ticker: 'SUI',
    name: 'Sui',
    network: 'Sui',
    address: '0x21c41c0a31ea5a30a3e7644d045c7f565fb8478efded149e0da0bc1516410539',
  },
  {
    id: 'bch',
    ticker: 'BCH',
    name: 'Bitcoin Cash',
    network: 'Bitcoin Cash',
    address: 'qzxgwva627638dwk42wcc99ef89am2jnzvr3nfa9m2',
  },
  {
    id: 'usdt',
    ticker: 'USDT',
    name: 'Tether',
    network: 'TRON · TRC20',
    address: 'THJzAL1ZQuVrn5cGPaecCnp7PfTr9Eh4mm',
  },
])

export interface CryptoIntentOriginOptions {
  readonly allowLocalhost?: boolean
}

export function canCreateCryptoIntent(
  origin: string,
  options: CryptoIntentOriginOptions = {},
): boolean {
  if (OFFICIAL_DONATE_ORIGINS.has(origin)) return true
  if (!options.allowLocalhost) return false
  try {
    const { protocol, hostname } = new URL(origin)
    return (protocol === 'http:' || protocol === 'https:')
      && (hostname === 'localhost' || hostname === '127.0.0.1')
  } catch {
    return false
  }
}
