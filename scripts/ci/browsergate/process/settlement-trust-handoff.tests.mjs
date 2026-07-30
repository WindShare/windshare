import assert from 'node:assert/strict'
import { generateKeyPairSync } from 'node:crypto'
import {
  mkdirSync,
  mkdtempSync,
  rmSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'

import { processSettlementPublicKeyFingerprint } from '../../../../web/scripts/browser-evidence/artifact/settlement-receipt.ts'
import {
  createD5SettlementTrustHandoff,
  readD5SettlementTrustHandoff,
  writeD5SettlementTrustHandoff,
} from './settlement-trust-handoff.mjs'

const temporaryRoot = resolve(mkdtempSync(join(tmpdir(), 'windshare-d5-settlement-handoff-')))
try {
  const contextRoot = join(temporaryRoot, 'main')
  mkdirSync(contextRoot)
  const runtimeManifestSha256 = 'a'.repeat(64)
  const base = Object.freeze({
    contextRoot,
    runId: 'handoff-contract',
    checkoutSha: 'b'.repeat(40),
    runtimeManifestSha256,
  })
  const authority = createD5SettlementTrustHandoff({
    ...base,
    invocationId: 'c'.repeat(64),
  })
  const pair = generateKeyPairSync('ed25519')
  const publicKeySpkiBase64 = pair.publicKey
    .export({ format: 'der', type: 'spki' })
    .toString('base64')
  const trust = Object.freeze({
    invocationId: authority.invocationId,
    runtimeManifestSha256,
    publicKeySpkiBase64,
    publicKeySha256: processSettlementPublicKeyFingerprint(publicKeySpkiBase64),
  })

  writeD5SettlementTrustHandoff(authority, trust)
  assert.deepEqual(await readD5SettlementTrustHandoff(authority), trust)
  assert.throws(
    () => writeD5SettlementTrustHandoff(authority, trust),
    /EEXIST/u,
    'D5 public pin handoff must be create-only',
  )

  const mismatched = createD5SettlementTrustHandoff({
    ...base,
    invocationId: 'd'.repeat(64),
  })
  assert.throws(
    () => writeD5SettlementTrustHandoff(mismatched, trust),
    /outer handoff authority/u,
  )

  const tampered = createD5SettlementTrustHandoff({
    ...base,
    invocationId: 'e'.repeat(64),
  })
  writeFileSync(tampered.outputPath, JSON.stringify({
    schemaVersion: tampered.schemaVersion,
    invocationId: tampered.invocationId,
    runtimeManifestSha256,
    runId: 'different-run',
    checkoutSha: tampered.checkoutSha,
    suite: tampered.suite,
    publicKeySpkiBase64,
    publicKeySha256: trust.publicKeySha256,
  }) + '\n', { encoding: 'utf8', flag: 'wx' })
  await assert.rejects(
    readD5SettlementTrustHandoff(tampered),
    /differs from its outer authority/u,
  )
} finally {
  rmSync(temporaryRoot, { recursive: true, force: true })
}

console.log('D5 settlement trust handoff contracts: PASS')
