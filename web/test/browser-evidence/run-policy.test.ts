import { describe, expect, it } from 'vitest'

import {
  assertBrowserRunPolicyEqual,
  browserRunPolicy,
  parseBrowserRunPolicy,
  validatePolicySampleIndex,
} from '../../scripts/browser-evidence/run-policy.ts'

describe('browser evidence run policy', () => {
  it.each([
    ['blocking', 1],
    ['closure', 3],
    ['stability', 5],
  ] as const)('binds %s to its versioned sample count', (policyId, sampleCount) => {
    const policy = browserRunPolicy(policyId)
    expect(policy).toEqual({
      schemaVersion: 1,
      policyId,
      policyVersion: 1,
      sampleCount,
    })
    expect(parseBrowserRunPolicy(JSON.parse(JSON.stringify(policy)))).toBe(policy)
    expect(validatePolicySampleIndex(sampleCount, policy)).toBe(sampleCount)
    expect(() => validatePolicySampleIndex(sampleCount + 1, policy)).toThrow(/must be in/u)
  })

  it('rejects count substitution, unknown versions, and additive fields', () => {
    const closure = browserRunPolicy('closure')
    expect(() => parseBrowserRunPolicy({ ...closure, sampleCount: 1 }))
      .toThrow(/sample count/u)
    expect(() => parseBrowserRunPolicy({ ...closure, policyVersion: 2 }))
      .toThrow(/version/u)
    expect(() => parseBrowserRunPolicy({ ...closure, inheritedSampleCount: 3 }))
      .toThrow(/unknown field/u)
  })

  it('rejects policy substitution even when a sample index overlaps both policies', () => {
    expect(() => assertBrowserRunPolicyEqual(
      browserRunPolicy('blocking'),
      browserRunPolicy('closure'),
    )).toThrow(/bound policy authority/u)
  })
})
