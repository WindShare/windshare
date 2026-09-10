import { describe, expect, it } from 'vitest'
import {
  admitDirectZipRuntimeV1,
  DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1,
  type DirectZipRequiredFeatureFactsV1,
  type DirectZipRuntimeCapabilitiesV1,
  type DirectZipRuntimePolicyV1,
} from '../../../../src/output/direct-zip/session'
import { directZipEpochPolicyDigestV2 } from '../../../../src/output/direct-zip/writer'
import { sameRuntimeDirectZipSupport } from '../../../../src/output/planning'

const FEATURES: DirectZipRequiredFeatureFactsV1 = Object.freeze({
  createWritable: 'function',
  handleIsSameEntry: 'function',
  handleQueryPermission: 'function',
  handleRequestPermission: 'function',
  indexedDB: 'object',
  isSecureContext: true,
  locks: 'object',
  showDirectoryPicker: 'function',
})
const CAPABILITIES: DirectZipRuntimeCapabilitiesV1 = Object.freeze({
  featureFacts: FEATURES,
  authority: Object.freeze({
    kind: 'owned-target-session-v1',
    recovery: 'persisted-handle-and-verified-checkpoint',
    replacement: 'coordinated-no-replace',
    cleanup: 'ownership-proof-required',
  }),
})

describe('Direct ZIP runtime admission', () => {
  it('admits the installed session contract without release-machine identity', async () => {
    const result = await admitDirectZipRuntimeV1({ capabilities: CAPABILITIES })

    expect(result.kind).toBe('available')
    if (result.kind !== 'available') return
    expect(result.facts.support).toMatchObject({
      kind: 'runtime-supported',
      authority: CAPABILITIES.authority,
    })
    expect(Object.keys(result.facts.support).sort()).toEqual([
      'authority', 'capabilityDigest', 'kind', 'policies',
    ])
    expect(Object.values(result.facts.support.policies)).toHaveLength(5)
    expect(Object.values(result.facts.support.policies).every(value => value.length === 43)).toBe(true)
    expect(result.facts.automaticEpochPolicy).toEqual(
      DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1.automaticEpochPolicy,
    )
    await expect(directZipEpochPolicyDigestV2(result.facts.automaticEpochPolicy))
      .resolves.toBe(result.facts.support.policies.epoch)
  })

  it.each(Object.keys(FEATURES) as (keyof DirectZipRequiredFeatureFactsV1)[])(
    'requires %s independently of the installed session contract',
    async (feature) => {
      const result = await admitDirectZipRuntimeV1({
        capabilities: {
          ...CAPABILITIES,
          featureFacts: {
            ...FEATURES,
            [feature]: feature === 'isSecureContext' ? false : 'missing',
          },
        },
      })
      expect(result).toEqual({
        kind: 'unavailable',
        support: { kind: 'unavailable', reason: 'required-api-unavailable' },
      })
    },
  )

  it.each([
    'runtime-not-installed',
    'journal-unavailable',
    'handle-persistence-unavailable',
    'coordination-unavailable',
  ] as const)('API presence cannot override %s', async (reason) => {
    const result = await admitDirectZipRuntimeV1({
      capabilities: { featureFacts: FEATURES, authority: { kind: 'unavailable', reason } },
    })
    expect(result).toEqual({
      kind: 'unavailable',
      support: { kind: 'unavailable', reason },
    })
  })

  it.each([
    { recovery: 'handle-locator-only' },
    { replacement: 'atomic-no-replace' },
    { cleanup: 'name-match-sufficient' },
  ])('rejects an authority contract with unsupported semantics: %j', async (invalid) => {
    const capabilities = {
      ...CAPABILITIES,
      authority: { ...CAPABILITIES.authority, ...invalid },
    } as unknown as DirectZipRuntimeCapabilitiesV1

    await expect(admitDirectZipRuntimeV1({ capabilities })).resolves.toEqual({
      kind: 'unavailable',
      support: { kind: 'unavailable', reason: 'authority-contract-unavailable' },
    })
  })

  it('keeps optional ranking and checkpoint-copy policy independent of runtime capability identity', async () => {
    const base = await admitDirectZipRuntimeV1({ capabilities: CAPABILITIES })
    const withoutRanking = await admitDirectZipRuntimeV1({
      capabilities: CAPABILITIES,
      policy: { ...DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1, workspacePeakBytesThreshold: null },
    })
    const changedPolicy = await admitDirectZipRuntimeV1({
      capabilities: CAPABILITIES,
      policy: {
        ...DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1,
        automaticEpochPolicy: {
          ...DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1.automaticEpochPolicy,
          minimumAdvanceBytes: 1n,
        },
      },
    })
    expect([base.kind, withoutRanking.kind, changedPolicy.kind]).toEqual([
      'available', 'available', 'available',
    ])
    if (base.kind !== 'available' || withoutRanking.kind !== 'available' ||
        changedPolicy.kind !== 'available') return
    expect(withoutRanking.facts.recommendationPolicy).toEqual({
      version: 1, kind: 'unavailable', reason: 'workspace-threshold-unavailable',
    })
    expect(sameRuntimeDirectZipSupport(base.facts.support, withoutRanking.facts.support)).toBe(true)
    expect(changedPolicy.facts.support.capabilityDigest).toBe(base.facts.support.capabilityDigest)
    expect(changedPolicy.facts.support.policies.epoch).not.toBe(base.facts.support.policies.epoch)
    expect(sameRuntimeDirectZipSupport(base.facts.support, changedPolicy.facts.support)).toBe(false)
  })

  it('declines malformed optional ranking policy while keeping supported output available', async () => {
    const result = await admitDirectZipRuntimeV1({
      capabilities: CAPABILITIES,
      policy: { ...DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1, workspacePeakBytesThreshold: -1n },
    })
    expect(result.kind).toBe('available')
    if (result.kind !== 'available') return
    expect(result.facts.recommendationPolicy).toEqual({
      version: 1, kind: 'unavailable', reason: 'policy-digest-unavailable',
    })
  })

  it.each([0n, -1n, BigInt(Number.MAX_SAFE_INTEGER) + 1n, 1.5])(
    'rejects an invalid configured checkpoint spacing: %s',
    async (minimumAdvanceBytes) => {
      const policy = {
        ...DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1,
        automaticEpochPolicy: {
          ...DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1.automaticEpochPolicy,
          minimumAdvanceBytes,
        },
      } as DirectZipRuntimePolicyV1
      await expect(admitDirectZipRuntimeV1({ capabilities: CAPABILITIES, policy }))
        .resolves.toEqual({
          kind: 'unavailable',
          support: { kind: 'unavailable', reason: 'policy-digests-unavailable' },
        })
    },
  )

  it('snapshots policy before asynchronous digest computation', async () => {
    const policy = {
      version: 1 as const,
      automaticEpochPolicy: { ...DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1.automaticEpochPolicy },
      workspacePeakBytesThreshold: 1024n,
    }
    const pending = admitDirectZipRuntimeV1({ capabilities: CAPABILITIES, policy })
    policy.automaticEpochPolicy.minimumAdvanceBytes = 0n
    policy.workspacePeakBytesThreshold = -1n
    const result = await pending

    expect(result.kind).toBe('available')
    if (result.kind !== 'available') return
    expect(result.facts.automaticEpochPolicy.minimumAdvanceBytes).toBe(
      DEFAULT_DIRECT_ZIP_RUNTIME_POLICY_V1.automaticEpochPolicy.minimumAdvanceBytes,
    )
    expect(result.facts.recommendationPolicy).toMatchObject({ workspacePeakBytesThreshold: 1024n })
  })
})
