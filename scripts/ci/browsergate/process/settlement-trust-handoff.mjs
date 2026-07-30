import { randomBytes } from 'node:crypto'
import {
  existsSync,
  lstatSync,
  mkdirSync,
  writeFileSync,
} from 'node:fs'
import { isAbsolute, join, resolve } from 'node:path'

import { processSettlementPublicKeyFingerprint } from '../../../../web/scripts/browser-evidence/artifact/settlement-receipt.ts'
import { parseCanonicalJsonText } from '../../../../web/scripts/browser-evidence/contract/strict-json.ts'
import { readStableRegularFileSnapshot } from '../../../../web/scripts/browser-evidence/filesystem/snapshot.ts'

export const D5_SETTLEMENT_TRUST_HANDOFF_SCHEMA_VERSION = 1

const HANDOFF_DIRECTORY = 'orchestration'
const HANDOFF_PREFIX = 'd5-settlement-trust-'
const MAXIMUM_HANDOFF_BYTES = 64 * 1024
const CHECKOUT_SHA_PATTERN = /^[a-f0-9]{40}$/u
const INVOCATION_ID_PATTERN = /^[a-f0-9]{64}$/u
const SHA256_PATTERN = /^[a-f0-9]{64}$/u

/**
 * The outer owner chooses the invocation identity and create-only destination.
 * The D5 child can publish its public pin, but cannot silently redirect or
 * reuse a trust record from another runtime or evidence context.
 */
export function createD5SettlementTrustHandoff({
  contextRoot,
  runId,
  checkoutSha,
  runtimeManifestSha256,
  invocationId = randomBytes(32).toString('hex'),
}) {
  const root = canonicalAbsolutePath(contextRoot, 'D5 settlement context root')
  requirePortableToken(runId, 'D5 settlement run ID')
  requirePattern(checkoutSha, CHECKOUT_SHA_PATTERN, 'D5 settlement checkout SHA')
  requirePattern(
    runtimeManifestSha256,
    SHA256_PATTERN,
    'D5 settlement runtime manifest digest',
  )
  requirePattern(invocationId, INVOCATION_ID_PATTERN, 'D5 settlement invocation ID')
  const directory = join(root, HANDOFF_DIRECTORY)
  mkdirSync(directory, { recursive: true, mode: 0o700 })
  const metadata = lstatSync(directory)
  if (!metadata.isDirectory() || metadata.isSymbolicLink()) {
    throw new Error('D5 settlement handoff parent must be a non-symbolic directory')
  }
  const outputPath = join(directory, HANDOFF_PREFIX + invocationId + '.json')
  if (existsSync(outputPath)) throw new Error('D5 settlement handoff path already exists')
  return Object.freeze({
    schemaVersion: D5_SETTLEMENT_TRUST_HANDOFF_SCHEMA_VERSION,
    outputPath,
    invocationId,
    runtimeManifestSha256,
    runId,
    checkoutSha,
    suite: 'main',
  })
}

export function writeD5SettlementTrustHandoff(authority, trust) {
  const expected = parseAuthority(authority)
  const parsedTrust = parseTrust(trust)
  if (
    parsedTrust.invocationId !== expected.invocationId ||
    parsedTrust.runtimeManifestSha256 !== expected.runtimeManifestSha256
  ) throw new Error('D5 settlement trust differs from its outer handoff authority')
  const record = Object.freeze({
    schemaVersion: D5_SETTLEMENT_TRUST_HANDOFF_SCHEMA_VERSION,
    invocationId: expected.invocationId,
    runtimeManifestSha256: expected.runtimeManifestSha256,
    runId: expected.runId,
    checkoutSha: expected.checkoutSha,
    suite: expected.suite,
    publicKeySpkiBase64: parsedTrust.publicKeySpkiBase64,
    publicKeySha256: parsedTrust.publicKeySha256,
  })
  writeFileSync(expected.outputPath, JSON.stringify(record) + '\n', {
    encoding: 'utf8',
    flag: 'wx',
    mode: 0o600,
  })
}

export async function readD5SettlementTrustHandoff(authority) {
  const expected = parseAuthority(authority)
  const snapshot = await readStableRegularFileSnapshot(
    expected.outputPath,
    MAXIMUM_HANDOFF_BYTES,
    'D5 settlement trust handoff',
  )
  const value = parseCanonicalJsonText(
    new TextDecoder('utf-8', { fatal: true }).decode(snapshot.bytes),
    'D5 settlement trust handoff',
  )
  requireExactKeys(value, [
    'schemaVersion',
    'invocationId',
    'runtimeManifestSha256',
    'runId',
    'checkoutSha',
    'suite',
    'publicKeySpkiBase64',
    'publicKeySha256',
  ], 'D5 settlement trust handoff')
  if (
    value.schemaVersion !== expected.schemaVersion ||
    value.invocationId !== expected.invocationId ||
    value.runtimeManifestSha256 !== expected.runtimeManifestSha256 ||
    value.runId !== expected.runId ||
    value.checkoutSha !== expected.checkoutSha ||
    value.suite !== expected.suite
  ) throw new Error('D5 settlement trust handoff differs from its outer authority')
  return parseTrust(value)
}

function parseAuthority(value) {
  requireExactKeys(value, [
    'schemaVersion',
    'outputPath',
    'invocationId',
    'runtimeManifestSha256',
    'runId',
    'checkoutSha',
    'suite',
  ], 'D5 settlement handoff authority')
  if (value.schemaVersion !== D5_SETTLEMENT_TRUST_HANDOFF_SCHEMA_VERSION) {
    throw new Error('D5 settlement handoff authority schema is unsupported')
  }
  const outputPath = canonicalAbsolutePath(value.outputPath, 'D5 settlement handoff output')
  requirePattern(value.invocationId, INVOCATION_ID_PATTERN, 'D5 settlement invocation ID')
  requirePattern(
    value.runtimeManifestSha256,
    SHA256_PATTERN,
    'D5 settlement runtime manifest digest',
  )
  requirePortableToken(value.runId, 'D5 settlement run ID')
  requirePattern(value.checkoutSha, CHECKOUT_SHA_PATTERN, 'D5 settlement checkout SHA')
  if (value.suite !== 'main') throw new Error('D5 settlement handoff is valid only for main')
  const expectedSuffix = join(
    HANDOFF_DIRECTORY,
    HANDOFF_PREFIX + value.invocationId + '.json',
  )
  if (!outputPath.endsWith(expectedSuffix)) {
    throw new Error('D5 settlement handoff output does not bind its invocation ID')
  }
  return Object.freeze({ ...value, outputPath })
}

function parseTrust(value) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('D5 settlement trust must be an object')
  }
  const invocationId = requirePattern(
    value.invocationId,
    INVOCATION_ID_PATTERN,
    'D5 settlement trust invocation ID',
  )
  const runtimeManifestSha256 = requirePattern(
    value.runtimeManifestSha256,
    SHA256_PATTERN,
    'D5 settlement trust runtime manifest digest',
  )
  if (typeof value.publicKeySpkiBase64 !== 'string' || value.publicKeySpkiBase64 === '') {
    throw new Error('D5 settlement trust public key is missing')
  }
  const fingerprint = processSettlementPublicKeyFingerprint(value.publicKeySpkiBase64)
  if (value.publicKeySha256 !== fingerprint) {
    throw new Error('D5 settlement trust public-key fingerprint is invalid')
  }
  return Object.freeze({
    invocationId,
    runtimeManifestSha256,
    publicKeySpkiBase64: value.publicKeySpkiBase64,
    publicKeySha256: fingerprint,
  })
}

function canonicalAbsolutePath(value, label) {
  if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value) {
    throw new Error(label + ' must be a canonical absolute path')
  }
  return value
}

function requirePortableToken(value, label) {
  if (typeof value !== 'string' || !/^[A-Za-z0-9._-]{1,128}$/u.test(value)) {
    throw new Error(label + ' is invalid')
  }
  return value
}

function requirePattern(value, pattern, label) {
  if (typeof value !== 'string' || !pattern.test(value)) throw new Error(label + ' is invalid')
  return value
}

function requireExactKeys(value, expected, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(label + ' must be an object')
  }
  const actual = Object.keys(value).sort()
  const sortedExpected = [...expected].sort()
  if (
    actual.length !== sortedExpected.length ||
    actual.some((key, index) => key !== sortedExpected[index])
  ) throw new Error(label + ' fields are not exact')
}
