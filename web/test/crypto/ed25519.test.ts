import { readFileSync } from 'node:fs'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ed25519 } from '@noble/curves/ed25519.js'
import { createEd25519Verifier, type Ed25519Observation } from '../../src/crypto/ed25519'
import { ED25519_NATIVE_DEADLINE_MS } from '../../src/crypto/ed25519-native'
import type { CryptoRuntime } from '../../src/crypto/webcrypto'

interface AcceptanceCase {
  name: string
  publicKeyHex: string
  messageHex: string
  signatureHex: string
  accepted: boolean
  keyAccepted: boolean
}
const vectors = (JSON.parse(readFileSync(
  new URL('../../../core/testvectors/ed25519-acceptance.json', import.meta.url), 'utf8',
)) as { cases: AcceptanceCase[] }).cases
const valid = vectors.find(row => row.name === 'valid-97')!
const bytes = (hex: string) => Uint8Array.from(Buffer.from(hex, 'hex'))
const message = bytes(valid.messageHex)
const signature = bytes(valid.signatureHex)

afterEach(() => { vi.useRealTimers() })

describe('sender Ed25519 acceptance and backend lifetime', () => {
  it.each(['native', 'noble'] as const)('enforces the shared Go vectors using %s', async backend => {
    const runtime = wrappedRuntime({
      ...(backend === 'noble' ? { importKey: async () => { throw new DOMException('', 'NotSupportedError') } } : {}),
    })
    for (const row of vectors) {
      const check = async () => createEd25519Verifier(bytes(row.publicKeyHex), { runtime })
        .verify(bytes(row.messageHex), bytes(row.signatureHex))
      if (!row.keyAccepted) await expect(check(), row.name).rejects.toThrow()
      else await expect(check(), row.name).resolves.toBe(row.accepted)
    }
  })

  it('rejects the cofactored-only signature that strict Noble alone accepts', async () => {
    const row = vectors.find(row => row.name === 'mixed-order-R')!
    expect(ed25519.verify(bytes(row.signatureHex), bytes(row.messageHex), bytes(row.publicKeyHex), { zip215: false })).toBe(true)
    const runtime = wrappedRuntime({ importKey: async () => { throw new DOMException('', 'NotSupportedError') } })
    await expect(createEd25519Verifier(bytes(row.publicKeyHex), { runtime })
      .verify(bytes(row.messageHex), bytes(row.signatureHex))).resolves.toBe(false)
  })

  it('qualifies once per runtime and imports each sender key once under concurrency', async () => {
    let imports = 0
    const importKey = new Proxy(crypto.subtle.importKey, {
      apply(target, _receiver, args) {
        imports += 1
        return Reflect.apply(target, crypto.subtle, args)
      },
    })
    const verify = vi.fn(crypto.subtle.verify.bind(crypto.subtle))
    const runtime = wrappedRuntime({ importKey, verify })
    const events: Ed25519Observation[] = []
    const sender = createEd25519Verifier(bytes(valid.publicKeyHex), { runtime, observe: event => events.push(event) })
    const second = createEd25519Verifier(bytes(valid.publicKeyHex), { runtime })
    expect(await Promise.all([sender.verify(message, signature), sender.verify(message, signature), second.verify(message, signature)]))
      .toEqual([true, true, true])
    expect(imports).toBe(3) // one capability probe and two identity lifetimes
    expect(events).toEqual([expect.objectContaining({ backend: 'native', transition: 'selected', reason: 'qualified' })])
    await sender.verify(message, signature)
    expect(imports).toBe(3)
  })

  it.each([true, false])('disqualifies a native verifier that always returns %s', async answer => {
    const events: Ed25519Observation[] = []
    const runtime = wrappedRuntime({ verify: async () => answer })
    const sender = createEd25519Verifier(bytes(valid.publicKeyHex), { runtime, observe: event => events.push(event) })
    await expect(sender.verify(message, signature)).resolves.toBe(true)
    await expect(sender.verify(new Uint8Array(), signature)).resolves.toBe(false)
    expect(events[0]).toMatchObject({ backend: 'noble', reason: 'qualification-failed' })
  })

  it('treats false as authentication failure after successful qualification', async () => {
    let reject = false
    const verify = vi.fn<SubtleCrypto['verify']>((...args) => reject
      ? Promise.resolve(false) : crypto.subtle.verify(...args))
    const events: Ed25519Observation[] = []
    const sender = createEd25519Verifier(bytes(valid.publicKeyHex), {
      runtime: wrappedRuntime({ verify }), observe: event => events.push(event),
    })
    await expect(sender.verify(message, signature)).resolves.toBe(true)
    const before = verify.mock.calls.length
    reject = true
    await expect(sender.verify(message, signature)).resolves.toBe(false)
    expect(verify).toHaveBeenCalledTimes(before + 1)
    expect(events.at(-1)).toMatchObject({ backend: 'native', transition: 'rejected', reason: 'invalid-signature' })
    expect(events.some(event => event.transition === 'fallback')).toBe(false)
  })

  it('disables a failing backend once and completely verifies every subsequent signature', async () => {
    let fail = false
    const verify = vi.fn<SubtleCrypto['verify']>((...args) => {
      if (fail) return Promise.reject(new DOMException('engine failed', 'OperationError'))
      return crypto.subtle.verify(...args)
    })
    const events: Ed25519Observation[] = []
    const sender = createEd25519Verifier(bytes(valid.publicKeyHex), {
      runtime: wrappedRuntime({ verify }), observe: event => events.push(event),
    })
    await sender.verify(message, signature)
    fail = true
    await expect(sender.verify(message, signature)).resolves.toBe(true)
    const calls = verify.mock.calls.length
    await expect(sender.verify(new Uint8Array(), signature)).resolves.toBe(false)
    await expect(sender.verify(message, signature)).resolves.toBe(true)
    expect(verify).toHaveBeenCalledTimes(calls)
    expect(events.filter(event => event.transition === 'fallback'))
      .toEqual([expect.objectContaining({ reason: 'operation-failed', backend: 'noble' })])
  })

  it('propagates unexpected backend errors instead of disguising configuration failures', async () => {
    let fail = false
    const failure = new DOMException('wrong key usage', 'InvalidAccessError')
    const sender = createEd25519Verifier(bytes(valid.publicKeyHex), {
      runtime: wrappedRuntime({ verify: (...args) => fail ? Promise.reject(failure) : crypto.subtle.verify(...args) }),
    })
    await sender.verify(message, signature)
    fail = true
    await expect(sender.verify(message, signature)).rejects.toBe(failure)
  })

  it('bounds a stalled operation and ignores its late native result', async () => {
    vi.useFakeTimers()
    let stall = false
    let finish: ((value: boolean) => void) | undefined
    const verify = vi.fn<SubtleCrypto['verify']>((...args) => stall
      ? new Promise(resolve => { finish = resolve }) : crypto.subtle.verify(...args))
    const sender = createEd25519Verifier(bytes(valid.publicKeyHex), { runtime: wrappedRuntime({ verify }) })
    await sender.verify(message, signature)
    stall = true
    const pending = sender.verify(message, signature)
    await vi.waitFor(() => expect(finish).toBeDefined())
    await vi.advanceTimersByTimeAsync(ED25519_NATIVE_DEADLINE_MS)
    await expect(pending).resolves.toBe(true)
    finish!(false)
    await expect(sender.verify(message, signature)).resolves.toBe(true)
  })

  it('rejects exceptional signature encodings before dispatching to the qualified backend', async () => {
    const verify = vi.fn(crypto.subtle.verify.bind(crypto.subtle))
    const sender = createEd25519Verifier(bytes(valid.publicKeyHex), { runtime: wrappedRuntime({ verify }) })
    await sender.verify(message, signature)
    const calls = verify.mock.calls.length
    for (const name of ['short-signature', 'long-signature', 'scalar-order', 'scalar-malleation',
      'invalid-R-point', 'noncanonical-R', 'negative-zero-R', 'identity-R', 'small-order-R']) {
      const row = vectors.find(candidate => candidate.name === name)!
      await expect(sender.verify(bytes(row.messageHex), bytes(row.signatureHex)), name).resolves.toBe(false)
    }
    expect(verify).toHaveBeenCalledTimes(calls)
  })

  it('falls back after a stalled capability probe without retrying it for another identity', async () => {
    vi.useFakeTimers()
    const importKey = vi.fn(async () => new Promise<CryptoKey>(() => undefined))
    const events: Ed25519Observation[] = []
    const runtime = wrappedRuntime({ importKey })
    const sender = createEd25519Verifier(bytes(valid.publicKeyHex), { runtime, observe: event => events.push(event) })
    const pending = sender.verify(message, signature)
    await vi.waitFor(() => expect(importKey).toHaveBeenCalledOnce())
    await vi.advanceTimersByTimeAsync(ED25519_NATIVE_DEADLINE_MS)
    await expect(pending).resolves.toBe(true)
    await expect(createEd25519Verifier(bytes(valid.publicKeyHex), { runtime }).verify(message, signature)).resolves.toBe(true)
    expect(importKey).toHaveBeenCalledOnce()
    expect(events[0]).toMatchObject({ backend: 'noble', reason: 'timeout' })
  })

  it('owns key, signature and message snapshots before asynchronous initialization', async () => {
    const publicKey = bytes(valid.publicKeyHex)
    const ownedMessage = message.slice()
    const ownedSignature = signature.slice()
    const sender = createEd25519Verifier(publicKey)
    const pending = sender.verify(ownedMessage, ownedSignature)
    publicKey.fill(0)
    sender.publicKey.fill(0)
    ownedMessage.fill(0)
    ownedSignature.fill(0)
    await expect(pending).resolves.toBe(true)
    expect(sender.publicKey).toEqual(bytes(valid.publicKeyHex))
  })
})

function wrappedRuntime(overrides: Partial<Pick<SubtleCrypto, 'verify' | 'importKey'>>): CryptoRuntime {
  return { subtle: new Proxy(crypto.subtle, {
    get(target, property) {
      if (property === 'verify' && overrides.verify !== undefined) return overrides.verify
      if (property === 'importKey' && overrides.importKey !== undefined) return overrides.importKey
      const value = Reflect.get(target, property) as unknown
      return typeof value === 'function' ? value.bind(target) : value
    },
  }) }
}
