import { expect, it } from 'vitest'
import { canonicalSTUN } from '../../src/network/stun-endpoint'
import { strictJSON, timestamp } from '../../src/network/strict-json'

it('rejects STUN credentials, numeric aliases and non-UDP transports', () => {
  for (const raw of ['stun:127.1:3478', 'stun:2130706433:3478', 'turn:host:3478', 'stun:user@host:3478', 'stun:host:0',
    'stun:host:65536', 'stun:host_name:3478', 'stun:host:3478?transport=tcp', 'stun:%68ost:3478', 'stun:host\\other:3478']) expect(() => canonicalSTUN(raw)).toThrow()
  expect(canonicalSTUN('stun:STUN.Example:03478')).toBe('stun:stun.example:3478')
  expect(canonicalSTUN('stun:[2001:db8:0:0::1]:3478')).toBe('stun:[2001:db8::1]:3478')
  expect(canonicalSTUN('stun:[::ffff:192.0.2.1]:3478')).toBe('stun:192.0.2.1:3478')
})
it('bounds JSON nesting and distinguishes escaped duplicate names from string content', () => {
  expect(() => strictJSON('{"name":1,"\\u006eame":2}')).toThrow()
  expect(() => strictJSON('['.repeat(17) + '0' + ']'.repeat(17))).toThrow()
  expect(strictJSON('{"name":"x\\":{","nested":{"name":2}}')).toEqual({ name: 'x":{', nested: { name: 2 } })
  for (const raw of ['{', '{"a":1,}', '{"a":"unterminated}', '{}{}']) expect(() => strictJSON(raw)).toThrow()
  expect(() => timestamp('2026-02-30T00:00:00Z')).toThrow()
  expect(() => timestamp('2026-01-01T24:00:00Z')).toThrow()
})
