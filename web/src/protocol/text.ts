// JavaScript strings can contain lone UTF-16 surrogates that TextEncoder silently
// replaces. Rejecting them before encoding keeps browser inputs identical to Go's
// UTF-8 scalar-value contract instead of signing different bytes than the caller
// supplied.
export function isWellFormedUnicode(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const current = value.charCodeAt(index)
    if (current >= 0xd800 && current <= 0xdbff) {
      const next = value.charCodeAt(index + 1)
      if (!Number.isInteger(next) || next < 0xdc00 || next > 0xdfff) return false
      index += 1
      continue
    }
    if (current >= 0xdc00 && current <= 0xdfff) return false
  }
  return true
}
