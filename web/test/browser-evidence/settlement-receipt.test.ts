import {
  createHash,
  generateKeyPairSync,
  sign,
  type KeyObject,
} from 'node:crypto'

import { describe, expect, it } from 'vitest'

import {
  PROCESS_SETTLEMENT_SCHEMA_VERSION,
  canonicalProcessSettlementPayloadBytes,
  processSettlementPublicKeyFingerprint,
  processSettlementSampleId,
  requireVerifiedProcessSettlementSet,
  verifyProcessSettlementAttestations,
  type ProcessSettlementAttestation,
  type ProcessSettlementPayload,
  type ProcessSettlementSampleExpectation,
  type ProcessSettlementTrustAnchor,
} from '../../scripts/browser-evidence/artifact/settlement-receipt.ts'
import { browserRunPolicy } from '../../scripts/browser-evidence/run-policy.ts'

const NOW = 1_800_000_000_000
const CHECKOUT_SHA = '1'.repeat(40)
const COMMAND_SHA256 = '2'.repeat(64)
const RESULT_BYTES = Buffer.from('{"result":"settled"}', 'utf8')

describe('process settlement attestation verifier', () => {
  it('returns an opaque capability only for the separately pinned Ed25519 authority', () => {
    const fixture = settlementFixture()
    const verified = verifyProcessSettlementAttestations({
      trust: fixture.trust,
      samples: [fixture.expected],
      attestations: [fixture.attestation],
      nowUnixMs: NOW,
    })
    const expectedInventory = {
      invocationId: fixture.trust.invocationId,
      samples: [fixture.expected],
    }
    expect(requireVerifiedProcessSettlementSet(verified, expectedInventory)).toBe(verified)
    expect(() => requireVerifiedProcessSettlementSet({ sampleKeys: verified.sampleKeys }, expectedInventory))
      .toThrow(/lacks an exact verified/u)
    expect(() => requireVerifiedProcessSettlementSet(verified, {
      invocationId: 'another-invocation',
      samples: [fixture.expected],
    })).toThrow(/another inventory/u)
  })

  it('rejects a receipt-supplied self key and a cosmetic quiescence boolean', () => {
    const fixture = settlementFixture()
    expect(() => verifyFixture(fixture, {
      ...fixture.attestation,
      publicKeySpkiBase64: fixture.trust.publicKeySpkiBase64,
    })).toThrow(/unknown field/u)
    expect(() => verifyFixture(fixture, {
      ...fixture.attestation,
      payload: { ...fixture.attestation.payload, quiescent: true },
    })).toThrow(/unknown field/u)
  })

  it('rejects result, command, and invocation identity swaps', () => {
    const fixture = settlementFixture()
    expect(() => verifyProcessSettlementAttestations({
      trust: fixture.trust,
      samples: [{ ...fixture.expected, resultBytes: Buffer.from('mutated', 'utf8') }],
      attestations: [fixture.attestation],
      nowUnixMs: NOW,
    })).toThrow(/identity differs.*resultSha256/u)
    expect(() => verifyProcessSettlementAttestations({
      trust: { ...fixture.trust, invocationId: 'another-invocation' },
      samples: [fixture.expected],
      attestations: [fixture.attestation],
      nowUnixMs: NOW,
    })).toThrow(/identity differs.*invocationId/u)
    expect(() => verifyProcessSettlementAttestations({
      trust: fixture.trust,
      samples: [{ ...fixture.expected, commandSha256: '5'.repeat(64) }],
      attestations: [fixture.attestation],
      nowUnixMs: NOW,
    })).toThrow(/identity differs.*commandSha256/u)
  })

  it('binds the common owner, input, and cleanup receipt into the signature', () => {
    const fixture = settlementFixture()
    const mutations: readonly ProcessSettlementPayload[] = [
      {
        ...fixture.attestation.payload,
        input: {
          outcome: 'failed',
          failureCode: 'CHILD_STDIN_DELIVERY_FAILED',
          failureMessage: 'mutated input evidence',
        },
      },
      {
        ...fixture.attestation.payload,
        cleanupOutcome: 'failed',
      },
      {
        ...fixture.attestation.payload,
        ownership: {
          ...fixture.attestation.payload.ownership,
          terminationReason: 'stop',
        },
      },
    ]
    for (const payload of mutations) {
      expect(() => verifyFixture(fixture, {
        ...fixture.attestation,
        payload,
      })).toThrow(/signature is invalid/u)
    }
  })

  it('rejects missing, duplicate, and nonce-replayed sample inventories', () => {
    const fixture = settlementFixture('closure')
    const second = settlementFixture('closure', 2, fixture.keys, fixture.trust)
    expect(() => verifyProcessSettlementAttestations({
      trust: fixture.trust,
      samples: [fixture.expected, second.expected],
      attestations: [fixture.attestation],
      nowUnixMs: NOW,
    })).toThrow(/missing, or contains extras/u)
    expect(() => verifyProcessSettlementAttestations({
      trust: fixture.trust,
      samples: [fixture.expected, second.expected],
      attestations: [fixture.attestation, fixture.attestation],
      nowUnixMs: NOW,
    })).toThrow(/duplicated/u)
    const replayed = signedAttestation(
      { ...second.attestation.payload, nonce: fixture.attestation.payload.nonce },
      fixture.keys.privateKey,
    )
    expect(() => verifyProcessSettlementAttestations({
      trust: fixture.trust,
      samples: [fixture.expected, second.expected],
      attestations: [fixture.attestation, replayed],
      nowUnixMs: NOW,
    })).toThrow(/replayed/u)
  })

  it('rejects a client-deadline path even when the owner later reaps its tree', () => {
    const fixture = settlementFixture()
    const timedOut = signedAttestation({
      ...fixture.attestation.payload,
      process: { terminal: 'exited', exitCode: 1 },
      ownership: { ...fixture.attestation.payload.ownership, terminationReason: 'deadline' },
    }, fixture.keys.privateKey)
    expect(() => verifyFixture(fixture, timedOut)).toThrow(/quiescence/u)
  })

  it('rejects signed non-empty tree, cleanup failure, and expired evidence', () => {
    const fixture = settlementFixture()
    const mutations: readonly ProcessSettlementPayload[] = [
      { ...fixture.attestation.payload, treeEmpty: false },
      {
        ...fixture.attestation.payload,
        cleanupOutcome: 'failed',
      },
      {
        ...fixture.attestation.payload,
        issuedAtUnixMs: String(NOW - 10_000),
        expiresAtUnixMs: String(NOW - 1),
      },
    ]
    for (const payload of mutations) {
      expect(() => verifyFixture(
        fixture,
        signedAttestation(payload, fixture.keys.privateKey),
      )).toThrow(/quiescence|expired/u)
    }
  })

  it('rejects a valid signature made by a receipt-selected foreign key', () => {
    const fixture = settlementFixture()
    const foreign = generateKeyPairSync('ed25519')
    const attestation = signedAttestation(fixture.attestation.payload, foreign.privateKey)
    expect(() => verifyFixture(fixture, attestation)).toThrow(/signature is invalid/u)
  })
})

interface SettlementFixture {
  readonly keys: { readonly privateKey: KeyObject; readonly publicKey: KeyObject }
  readonly trust: ProcessSettlementTrustAnchor
  readonly expected: ProcessSettlementSampleExpectation
  readonly attestation: ProcessSettlementAttestation
}

function settlementFixture(
  policy: 'blocking' | 'closure' = 'blocking',
  sampleIndex = 1,
  keys = generateKeyPairSync('ed25519'),
  inheritedTrust?: ProcessSettlementTrustAnchor,
): SettlementFixture {
  const publicKeyBytes = keys.publicKey.export({ format: 'der', type: 'spki' })
  const publicKeySpkiBase64 = publicKeyBytes.toString('base64')
  const trust = inheritedTrust ?? Object.freeze({
    invocationId: 'invocation-123',
    publicKeySpkiBase64,
    publicKeySha256: processSettlementPublicKeyFingerprint(publicKeySpkiBase64),
  })
  const runPolicy = browserRunPolicy(policy)
  const sample = Object.freeze({
    runId: 'run-123',
    runPolicy,
    suite: 'main' as const,
    browser: 'chromium' as const,
    sampleIndex,
    checkoutSha: CHECKOUT_SHA,
  })
  const payload: ProcessSettlementPayload = Object.freeze({
    schemaVersion: PROCESS_SETTLEMENT_SCHEMA_VERSION,
    invocationId: trust.invocationId,
    sampleId: processSettlementSampleId(sample.suite, sample.browser, sample.sampleIndex),
    runId: sample.runId,
    runPolicy,
    suite: sample.suite,
    browser: sample.browser,
    sampleIndex: sample.sampleIndex,
    checkoutSha: sample.checkoutSha,
    commandSha256: COMMAND_SHA256,
    resultSha256: sha256(RESULT_BYTES),
    resultByteLength: String(RESULT_BYTES.byteLength),
    process: Object.freeze({ terminal: 'exited', exitCode: 0 }),
    treeEmpty: true,
    cleanupOutcome: 'completed',
    input: Object.freeze({ outcome: 'delivered', failureCode: '', failureMessage: '' }),
    ownership: Object.freeze({
      kind: 'test-process-owner',
      backend: 'linux_subreaper',
      terminationReason: 'natural',
    }),
    nonce: sampleIndex.toString(16).padStart(64, '0'),
    issuedAtUnixMs: String(NOW - 1_000),
    expiresAtUnixMs: String(NOW + 60_000),
  })
  return Object.freeze({
    keys,
    trust,
    expected: Object.freeze({ sample, resultBytes: RESULT_BYTES, commandSha256: COMMAND_SHA256 }),
    attestation: signedAttestation(payload, keys.privateKey),
  })
}

function signedAttestation(
  payload: ProcessSettlementPayload,
  privateKey: KeyObject,
): ProcessSettlementAttestation {
  return Object.freeze({
    payload,
    signatureBase64: sign(
      null,
      canonicalProcessSettlementPayloadBytes(payload),
      privateKey,
    ).toString('base64'),
  })
}

function verifyFixture(fixture: SettlementFixture, attestation: unknown): void {
  verifyProcessSettlementAttestations({
    trust: fixture.trust,
    samples: [fixture.expected],
    attestations: [attestation],
    nowUnixMs: NOW,
  })
}

function sha256(value: Uint8Array): string {
  return createHash('sha256').update(value).digest('hex')
}
