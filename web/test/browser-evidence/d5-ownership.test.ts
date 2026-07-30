import { describe, expect, it } from 'vitest'

import {
  D5_SETTLEMENT_OWNERSHIP_ENVIRONMENT_NAMES,
  d5SettlementOwnershipEnvironment,
} from '../../scripts/browser-evidence/process/d5-ownership.ts'

const BROWSER_OWNERSHIP = Object.freeze({
  WINDSHARE_WINDOWS_OS_NETWORK: 'stable-harness-v3',
  WINDSHARE_D5_E2E_LEASE_TOKEN: 'lease-token',
  WINDSHARE_D5_RUNNER_PIPE: 'runner-pipe',
  WINDSHARE_D5_CHILD_MANIFEST: 'child-manifest.json',
})

describe('D5 browser ownership capabilities', () => {
  it('uses only reusable browser-harness authorities', () => {
    expect(D5_SETTLEMENT_OWNERSHIP_ENVIRONMENT_NAMES).toEqual(
      Object.keys(BROWSER_OWNERSHIP),
    )
    expect(d5SettlementOwnershipEnvironment(true, {
      ...BROWSER_OWNERSHIP,
      WINDSHARE_D5_AUTHORIZATION_PIPE: 'one-use-go-test-authority',
    })).toEqual(BROWSER_OWNERSHIP)
  })

  it('fails closed when a browser-harness authority is missing', () => {
    const incomplete: Record<string, string> = { ...BROWSER_OWNERSHIP }
    delete incomplete.WINDSHARE_D5_RUNNER_PIPE
    expect(() => d5SettlementOwnershipEnvironment(true, incomplete)).toThrow(
      'D5 sample lacks ownership capability WINDSHARE_D5_RUNNER_PIPE',
    )
  })
})
