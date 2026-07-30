import { contractError } from './json.ts'

const JSON_WHITESPACE = new Set([' ', '\t', '\n', '\r'])

/**
 * Evidence schemas contain integers only. Rejecting alternate numeric spellings
 * and duplicate decoded member names keeps Go and JavaScript on one semantic
 * input language instead of relying on their different JSON coercion rules.
 */
export function parseCanonicalJsonText(encoded: string, label: string): unknown {
  const decoder = new CanonicalJsonDecoder(encoded, label)
  return decoder.decode()
}

class CanonicalJsonDecoder {
  readonly #encoded: string
  readonly #label: string
  #offset = 0

  constructor(encoded: string, label: string) {
    this.#encoded = encoded
    this.#label = label
  }

  decode(): unknown {
    this.#skipWhitespace()
    const value = this.#value()
    this.#skipWhitespace()
    if (this.#offset !== this.#encoded.length) this.#fail('contains trailing data')
    return value
  }

  #value(): unknown {
    const character = this.#encoded[this.#offset]
    if (character === '{') return this.#object()
    if (character === '[') return this.#array()
    if (character === '"') return this.#string()
    if (character === 't') return this.#literal('true', true)
    if (character === 'f') return this.#literal('false', false)
    if (character === 'n') return this.#literal('null', null)
    return this.#integer()
  }

  #object(): Record<string, unknown> {
    this.#offset += 1
    this.#skipWhitespace()
    const result: Record<string, unknown> = {}
    const names = new Set<string>()
    if (this.#consume('}')) return result
    while (true) {
      if (this.#encoded[this.#offset] !== '"') this.#fail('object member name must be a string')
      const name = this.#string()
      if (names.has(name)) this.#fail(`contains duplicate object member ${JSON.stringify(name)}`)
      names.add(name)
      this.#skipWhitespace()
      if (!this.#consume(':')) this.#fail('object member lacks a colon')
      this.#skipWhitespace()
      Object.defineProperty(result, name, {
        value: this.#value(),
        enumerable: true,
        configurable: true,
        writable: true,
      })
      this.#skipWhitespace()
      if (this.#consume('}')) return result
      if (!this.#consume(',')) this.#fail('object members must be comma-separated')
      this.#skipWhitespace()
    }
  }

  #array(): unknown[] {
    this.#offset += 1
    this.#skipWhitespace()
    const result: unknown[] = []
    if (this.#consume(']')) return result
    while (true) {
      result.push(this.#value())
      this.#skipWhitespace()
      if (this.#consume(']')) return result
      if (!this.#consume(',')) this.#fail('array entries must be comma-separated')
      this.#skipWhitespace()
    }
  }

  #string(): string {
    const start = this.#offset
    this.#offset += 1
    let escaped = false
    while (this.#offset < this.#encoded.length) {
      const character = this.#encoded[this.#offset]
      this.#offset += 1
      if (escaped) {
        escaped = false
        continue
      }
      if (character === '\\') {
        escaped = true
        continue
      }
      if (character === '"') {
        const raw = this.#encoded.slice(start, this.#offset)
        let decoded: string
        try {
          decoded = JSON.parse(raw) as string
        } catch (cause) {
          this.#fail(`contains an invalid JSON string: ${String(cause)}`)
        }
        if (!isWellFormedUnicode(decoded) || decoded.includes('\ufffd')) {
          this.#fail('contains invalid or replacement Unicode')
        }
        return decoded
      }
    }
    this.#fail('contains an unterminated string')
  }

  #integer(): number {
    const remainder = this.#encoded.slice(this.#offset)
    const match = /^-?(0|[1-9]\d*)/u.exec(remainder)
    if (match === null) this.#fail('contains a non-canonical integer token')
    const token = match[0]
    if (token === '-0') this.#fail('contains a non-canonical integer token')
    const following = remainder[token.length]
    if (following !== undefined && /[.eE+\d]/u.test(following)) {
      this.#fail('contains a non-canonical integer token')
    }
    const value = Number(token)
    if (!Number.isSafeInteger(value)) this.#fail('contains an unsafe integer')
    this.#offset += token.length
    return value
  }

  #literal<T>(encoded: string, value: T): T {
    if (!this.#encoded.startsWith(encoded, this.#offset)) this.#fail('contains an invalid literal')
    this.#offset += encoded.length
    return value
  }

  #skipWhitespace(): void {
    while (JSON_WHITESPACE.has(this.#encoded[this.#offset] ?? '')) {
      this.#offset += 1
    }
  }

  #consume(expected: string): boolean {
    if (this.#encoded[this.#offset] !== expected) return false
    this.#offset += 1
    return true
  }

  #fail(reason: string): never {
    contractError(`${this.#label} ${reason} at UTF-16 offset ${this.#offset}`)
  }
}

function isWellFormedUnicode(value: string): boolean {
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index)
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = value.charCodeAt(index + 1)
      if (!Number.isInteger(next) || next < 0xdc00 || next > 0xdfff) return false
      index += 1
    } else if (unit >= 0xdc00 && unit <= 0xdfff) {
      return false
    }
  }
  return true
}
