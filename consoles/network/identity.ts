import type { Restrictions, Target } from './model'
import { message, MessageError } from '../ui/locale'
import { identityErrors } from './locales/identity'

const identityBytes = { node: 32, account: 16, share: 12 } as const
type IdentityKind = keyof typeof identityBytes

export function encode(bytes: ArrayLike<number>): string {
  return btoa(String.fromCharCode(...Array.from(bytes))).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '')
}

export function decode(text: string, kind: IdentityKind): number[] | null {
  const length = identityBytes[kind]
  const value = text.trim()
  if (/^[A-Za-z0-9_-]+$/.test(value) && value.length === Math.ceil(length * 4 / 3)) {
    const bytes = Array.from(atob(value.replaceAll('-', '+').replaceAll('_', '/')), char => char.charCodeAt(0))
    if (bytes.length === length && bytes.some(byte => byte !== 0) && encode(bytes) === value) return bytes
  }
  return null
}

export function accountFromFragment(fragment: string): number[] | null {
  try { return decode(decodeURIComponent(fragment), 'account') }
  catch { return null }
}

export function requireIdentity(text: string, kind: IdentityKind): number[] {
  const bytes = decode(text, kind)
  if (!bytes) throw new MessageError(message(identityErrors, kind))
  return bytes
}

export function directlyRestricted(restrictions: Restrictions, target: Target) {
  const entries = target.kind === 'account' ? restrictions.accounts : restrictions.shares
  const id = encode(target.id)
  return entries.some(value => encode(value) === id)
}
