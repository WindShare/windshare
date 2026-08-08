import { describe, expect, it } from 'vitest'

import { encodeBase64Url } from '../../src/crypto/bytes'
import { V2_MAXIMUM_SELECTION_RULE_OVERRIDES } from '../../src/catalog/v2-selection'
import {
  MAX_OUTPUT_BACKEND_ID_BYTES,
  canonicalTransferIntentBytes,
  createTransferIntentDraft,
  createTransferRun,
  freezeTransferIntent,
  snapshotTransferRun,
  transferIntentDigest,
  validateFinalTransferIntent,
} from '../../src/transfer/intent'
import type {
  TransferIntent,
  TransferOutputLocator,
  TransferSelectionRules,
} from '../../src/transfer/intent'

const OUTPUT: TransferOutputLocator = Object.freeze({
  target: identity(41, 32),
  targetKind: 2,
  backend: 'test/browser-output',
  format: 'directory',
})

function identity(seed: number, width = 16): string {
  const bytes = new Uint8Array(width)
  bytes[0] = seed
  return encodeBase64Url(bytes)
}

function draft(selection: TransferSelectionRules) {
  return createTransferIntentDraft({
    shareInstance: identity(1),
    syntheticRoot: identity(2),
    selection,
  })
}

function nodeSelection(): TransferSelectionRules {
  return { mode: 'node-id', defaultSelected: true, rules: [] }
}

describe('transfer intent authority', () => {
  it('keeps independently validated run identities outside durable intent', async () => {
    expect(() => snapshotTransferRun({
      transferJobId: 'run',
      outputSessionId: identity(4),
    })).toThrow(/non-zero 16-byte/)
    expect(() => snapshotTransferRun({
      transferJobId: identity(3),
      outputSessionId: identity(0, 17),
    })).toThrow(/non-zero 16-byte/)
    expect(() => snapshotTransferRun({
      transferJobId: identity(3),
      outputSessionId: identity(3),
    })).toThrow(/independent/)

    const run = createTransferRun()
    expect(run.transferJobId).not.toBe(run.outputSessionId)
    const intent = await freezeTransferIntent(draft(nodeSelection()), OUTPUT)
    expect(Object.hasOwn(intent, 'transferJobId')).toBe(false)
    expect(Object.hasOwn(intent, 'outputSessionId')).toBe(false)
  })

  it('normalizes, deduplicates, and UTF-8 sorts catalog-path targets like Go', () => {
    const created = draft({
      mode: 'catalog-path',
      defaultSelected: false,
      paths: ['zeta', 'docs/re\u0301sume\u0301.txt', 'alpha', 'docs/résumé.txt'],
    })

    expect(created.selection).toEqual({
      mode: 'catalog-path',
      defaultSelected: false,
      paths: ['alpha', 'docs/résumé.txt', 'zeta'],
    })
  })

  it('rejects mixed selection shapes, loose booleans, duplicate node IDs, and excess rules', () => {
    const aliasedPathRules = {
      mode: 'catalog-path', defaultSelected: false, paths: ['docs'], rules: [],
    } as unknown as TransferSelectionRules
    expect(() => draft(aliasedPathRules)).toThrow(/authority shape/)

    const looseBoolean = {
      mode: 'node-id', defaultSelected: 1, rules: [],
    } as unknown as TransferSelectionRules
    expect(() => draft(looseBoolean)).toThrow(/boolean/)

    const duplicate = {
      mode: 'node-id',
      defaultSelected: false,
      rules: [
        { kind: 'directory', id: identity(8), selected: true },
        { kind: 'file', id: identity(8), selected: true },
      ],
    } as const
    expect(() => draft(duplicate)).toThrow(/duplicate/)

    const excessive = {
      mode: 'node-id' as const,
      defaultSelected: false,
      rules: Array.from({ length: V2_MAXIMUM_SELECTION_RULE_OVERRIDES + 1 }, (_, index) => ({
        kind: 'file' as const,
        id: identity((index % 255) + 1),
        selected: true,
      })),
    }
    expect(() => draft(excessive)).toThrow(/authority shape/)
  })

  it('bounds catalog-path inputs before dedupe and their canonical byte total', () => {
    const repeated = Array.from({ length: V2_MAXIMUM_SELECTION_RULE_OVERRIDES + 1 }, () => 'same')
    expect(() => draft({ mode: 'catalog-path', defaultSelected: false, paths: repeated })).toThrow(/authority shape/)

    const segment = 'b'.repeat(250)
    const paths = Array.from({ length: 33 }, (_, index) => [
      `p${index.toString(36).padStart(3, '0')}${'a'.repeat(246)}`,
      ...Array.from({ length: 127 }, () => segment),
    ].join('/'))
    expect(() => draft({ mode: 'catalog-path', defaultSelected: false, paths })).toThrow(/byte limit/)
  })

  it('rejects browser filesystem targets and non-canonical backend identifiers', () => {
    const base = {
      shareInstance: identity(1),
      syntheticRoot: identity(2),
      selection: nodeSelection(),
    }
    expect(() => canonicalTransferIntentBytes({
      ...base,
      output: { ...OUTPUT, targetKind: 1 } as unknown as TransferOutputLocator,
    })).toThrow(/opaque target/)
    expect(() => canonicalTransferIntentBytes({
      ...base,
      output: { ...OUTPUT, backend: '\u2003padded' },
    })).toThrow(/canonical backend/)
    expect(() => canonicalTransferIntentBytes({
      ...base,
      output: { ...OUTPUT, backend: 'x'.repeat(MAX_OUTPUT_BACKEND_ID_BYTES + 1) },
    })).toThrow(/canonical backend/)
  })

  it('recomputes final canonical bytes and digest before trusting caller fields', async () => {
    const final = await freezeTransferIntent(draft(nodeSelection()), OUTPUT)
    const forgedDigest = { ...final, digest: identity(99, 32) } as TransferIntent
    const forgedBytes = {
      ...final,
      canonicalBytes: Uint8Array.from(final.canonicalBytes, (value, index) => index === 0 ? value ^ 1 : value),
    } as TransferIntent

    await expect(validateFinalTransferIntent(forgedDigest)).rejects.toThrow(/digest/)
    await expect(validateFinalTransferIntent(forgedBytes)).rejects.toThrow(/canonical bytes/)
    await expect(transferIntentDigest(forgedDigest)).rejects.toThrow(/digest/)
  })
})
