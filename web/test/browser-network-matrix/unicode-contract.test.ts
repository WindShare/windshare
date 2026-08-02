import { describe, expect, it } from 'vitest'

import { parseNetworkRuntimeAttestation } from '../../scripts/browser-network-matrix/attestation.ts'
import { loadRegistry, rawAttestation } from './fixtures.ts'

describe('browser network matrix Unicode contract', () => {
  it('rejects an unpaired UTF-16 surrogate in external fixture trust evidence', async () => {
    const registry = await loadRegistry()
    const runId = 'unicode-contract-run'
    const attestation = rawAttestation(
      registry,
      runId,
      'scheduled-public-stun',
      'satisfied',
    )
    const proof = attestation.proof as Record<string, unknown>
    const externalFixtureTrust = proof.externalFixtureTrust as Record<string, unknown>
    const malformed = {
      ...attestation,
      proof: {
        ...proof,
        externalFixtureTrust: {
          ...externalFixtureTrust,
          controllerOrigin: '\uD800',
        },
      },
    }

    expect(() => parseNetworkRuntimeAttestation(malformed, {
      manifest: registry.manifest,
      manifestSha256: registry.manifestSha256,
      runId,
    })).toThrow(/controller origin is invalid/u)
  })
})
