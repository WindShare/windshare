import { describe, expect, it } from 'vitest'

import {
  MAXIMUM_TEST_IDENTIFIER_BYTES,
  MAXIMUM_TEST_SCENARIO_BYTES,
  parseTestIdentity,
} from '../../scripts/browser-evidence/process/test-identity.mjs'

describe('cross-language test identity', () => {
  it('accepts the exact Go testrun boundaries and interior scenario slashes', () => {
    expect(parseTestIdentity({
      runId: 'r'.repeat(MAXIMUM_TEST_IDENTIFIER_BYTES),
      operationId: 'o'.repeat(MAXIMUM_TEST_IDENTIFIER_BYTES),
      scenario: `s/${'x'.repeat(MAXIMUM_TEST_SCENARIO_BYTES - 2)}`,
    })).toEqual({
      runId: 'r'.repeat(MAXIMUM_TEST_IDENTIFIER_BYTES),
      operationId: 'o'.repeat(MAXIMUM_TEST_IDENTIFIER_BYTES),
      scenario: `s/${'x'.repeat(MAXIMUM_TEST_SCENARIO_BYTES - 2)}`,
    })
  })

  it.each([
    ['run above maximum', { runId: 'r'.repeat(129), operationId: 'operation', scenario: 'scenario' }],
    ['operation above maximum', { runId: 'run', operationId: 'o'.repeat(129), scenario: 'scenario' }],
    ['scenario above maximum', { runId: 'run', operationId: 'operation', scenario: 's'.repeat(193) }],
    ['run slash', { runId: 'run/one', operationId: 'operation', scenario: 'scenario' }],
    ['operation space', { runId: 'run', operationId: 'operation one', scenario: 'scenario' }],
    ['scenario leading slash', { runId: 'run', operationId: 'operation', scenario: '/scenario' }],
    ['scenario trailing slash', { runId: 'run', operationId: 'operation', scenario: 'scenario/' }],
    ['leading punctuation', { runId: '-run', operationId: 'operation', scenario: 'scenario' }],
    ['trailing punctuation', { runId: 'run', operationId: 'operation_', scenario: 'scenario' }],
    ['non-ASCII', { runId: 'rún', operationId: 'operation', scenario: 'scenario' }],
  ] as const)('rejects %s before owner launch', (_name, identity) => {
    expect(() => parseTestIdentity(identity)).toThrow(/invalid/u)
  })
})
