export type PasswordRole = 'account' | 'admin'

export const USERNAME_MAX_BYTES = 64
export const PASSWORD_MIN_BYTES = 12
export const PASSWORD_MAX_BYTES = 256
export const PASSWORD_VERIFIER_BITS = 256
export const PASSWORD_PBKDF2_ITERATIONS = 600_000

const PASSWORD_DOMAINS: Readonly<Record<PasswordRole, string>> = {
  account: 'windshare-account-password-v1:',
  admin: 'windshare-admin-password-v1:',
}

const USERNAME_PATTERN = /^[A-Za-z0-9._-]{1,64}$/
const encoder = new TextEncoder()

function encodeBase64Url(bytes: Uint8Array): string {
  return btoa(String.fromCharCode(...bytes))
    .replaceAll('+', '-')
    .replaceAll('/', '_')
    .replaceAll('=', '')
}

export function normalizeUsername(raw: string): string | null {
  const trimmed = raw.trim()
  // Validate the ASCII identifier alphabet before lowercasing so Unicode case
  // mappings such as Kelvin sign (U+212A) cannot fold into ASCII letters.
  return USERNAME_PATTERN.test(trimmed) ? trimmed.toLowerCase() : null
}

export function isValidNewPassword(password: string): boolean {
  const bytes = encoder.encode(password).length
  return bytes >= PASSWORD_MIN_BYTES && bytes <= PASSWORD_MAX_BYTES
}

export function isValidLoginPassword(password: string): boolean {
  const bytes = encoder.encode(password).length
  return bytes > 0 && bytes <= PASSWORD_MAX_BYTES
}

// Derive a 32-byte PBKDF2-HMAC-SHA256 verifier locally with a role-separated
// canonical username salt so plaintext passwords never leave the client.
export async function derivePasswordVerifier(
  role: PasswordRole,
  username: string,
  password: string,
): Promise<string> {
  const canonical = normalizeUsername(username)
  if (!canonical || !isValidLoginPassword(password)) {
    throw new RangeError('Invalid username or password for verifier derivation')
  }
  const key = await crypto.subtle.importKey(
    'raw',
    encoder.encode(password),
    'PBKDF2',
    false,
    ['deriveBits'],
  )
  const bits = await crypto.subtle.deriveBits(
    {
      name: 'PBKDF2',
      hash: 'SHA-256',
      salt: encoder.encode(PASSWORD_DOMAINS[role] + canonical),
      iterations: PASSWORD_PBKDF2_ITERATIONS,
    },
    key,
    PASSWORD_VERIFIER_BITS,
  )
  return encodeBase64Url(new Uint8Array(bits))
}
