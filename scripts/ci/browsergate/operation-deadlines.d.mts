export interface GithubSuiteJobDeadlinePolicy {
  readonly jobSettlementReserveMs: number
  readonly minimumJobTimeoutMinutes: number
}

// Keep deadline arithmetic owned by the runtime module while exposing only the
// stable decision surface consumed by the TypeScript workflow contract.
export function createGithubSuiteJobDeadlinePolicy(
  suite: 'main' | 'pion',
  runPolicy: unknown,
  platform?: 'linux' | 'windows',
): GithubSuiteJobDeadlinePolicy
