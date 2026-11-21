export interface DonationsEnv {
  readonly DONATION_WALL: DurableObjectNamespace
  readonly AFDIAN_USER_ID: string
  readonly KOFI_HANDLE: string
  readonly ALLOWED_ORIGINS: string
  readonly DONATE_API_ORIGIN: string
  readonly AFDIAN_TOKEN?: string
  readonly AFDIAN_WEBHOOK_SECRET?: string
  readonly KOFI_VERIFICATION_TOKEN?: string
  readonly NOWPAYMENTS_API_KEY?: string
  readonly NOWPAYMENTS_IPN_SECRET?: string
  readonly DONATE_ADMIN_PASSWORD?: string
}

export function resolveCryptoCheckoutApiKey(env: DonationsEnv): string | null {
  // Require both invoice creation and IPN verification secrets before accepting new
  // checkouts so paid orders cannot stall in unverified 503 webhooks. Webhook settlement
  // stays decoupled from NOWPAYMENTS_API_KEY so in-flight payments can finish settling
  // even when checkout creation is disabled by removing the API key.
  if (!env.NOWPAYMENTS_API_KEY || !env.NOWPAYMENTS_IPN_SECRET) {
    return null
  }
  return env.NOWPAYMENTS_API_KEY
}

export function parseAllowedOrigins(raw: string): ReadonlySet<string> {
  const origins = new Set<string>()
  for (const part of raw.split(',')) {
    const trimmed = part.trim()
    if (trimmed.length === 0) continue
    try {
      const parsed = new URL(trimmed)
      if (parsed.protocol === 'https:' || parsed.protocol === 'http:') {
        origins.add(parsed.origin)
      }
    } catch {
      // Ignore malformed origin entries instead of weakening allowlist matching.
    }
  }
  return origins
}
