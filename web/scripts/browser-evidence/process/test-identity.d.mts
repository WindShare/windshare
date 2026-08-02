export const MAXIMUM_TEST_IDENTIFIER_BYTES: 128
export const MAXIMUM_TEST_SCENARIO_BYTES: 192

export interface TestIdentity {
  readonly runId: string
  readonly operationId: string
  readonly scenario: string
}

export function parseTestIdentity(value: TestIdentity): TestIdentity
