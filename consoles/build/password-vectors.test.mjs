import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'
import {
  PASSWORD_MAX_BYTES,
  PASSWORD_MIN_BYTES,
  PASSWORD_PBKDF2_ITERATIONS,
  USERNAME_MAX_BYTES,
  derivePasswordVerifier,
  isValidLoginPassword,
  isValidNewPassword,
  normalizeUsername,
} from '../network/auth/password.ts'

test('network console PBKDF2 password derivation matches shared cross-language vectors', async () => {
  const raw = await readFile(new URL('../../testdata/network/password-verifiers.json', import.meta.url), 'utf8')
  const fixture = JSON.parse(raw)
  assert.equal(fixture.iterations, PASSWORD_PBKDF2_ITERATIONS)
  assert.equal(fixture.verifier_bytes, 32)

  const results = new Map()
  for (const vector of fixture.vectors) {
    assert.equal(normalizeUsername(vector.username), vector.canonical_username, vector.name)
    const verifier = await derivePasswordVerifier(vector.role, vector.username, vector.password)
    assert.equal(verifier, vector.verifier, vector.name)
    results.set(vector.name, verifier)
  }
  assert.notEqual(
    results.get('account_mixed_case_whitespace'),
    results.get('admin_domain_separation'),
    'account and admin verifiers must be domain-separated',
  )
})

test('network console password and username validation enforces byte bounds', async () => {
  for (const invalid of ['', '   ', 'bad space', 'a@b', '中文', '\u212Aevin', '\u0130van', 'a'.repeat(USERNAME_MAX_BYTES + 1)]) {
    assert.equal(normalizeUsername(invalid), null, invalid)
  }
  assert.equal(normalizeUsername('  Alice_1.0-x  '), 'alice_1.0-x')
  assert.equal(normalizeUsername('\u00A0\u3000Alice_1.0-x\u3000\u00A0'), 'alice_1.0-x')

  assert.equal(isValidNewPassword('a'.repeat(PASSWORD_MIN_BYTES - 1)), false)
  assert.equal(isValidNewPassword('a'.repeat(PASSWORD_MIN_BYTES)), true)
  assert.equal(isValidNewPassword('a'.repeat(PASSWORD_MAX_BYTES)), true)
  assert.equal(isValidNewPassword('a'.repeat(PASSWORD_MAX_BYTES + 1)), false)
  // UTF-8 byte length is enforced, not JS UTF-16 code-unit length.
  assert.equal(isValidNewPassword('春'.repeat(4)), true)
  assert.equal(isValidNewPassword('春'.repeat(86)), false)

  assert.equal(isValidLoginPassword(''), false)
  assert.equal(isValidLoginPassword('x'), true)
  assert.equal(isValidLoginPassword('a'.repeat(PASSWORD_MAX_BYTES)), true)
  assert.equal(isValidLoginPassword('a'.repeat(PASSWORD_MAX_BYTES + 1)), false)

  await assert.rejects(() => derivePasswordVerifier('admin', 'bad user', 'Correct-Horse-Battery-2026!'), RangeError)
  await assert.rejects(() => derivePasswordVerifier('account', 'alice', ''), RangeError)
})
