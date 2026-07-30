const SUITES = Object.freeze(['main', 'pion'])
const OPERATION_OUTCOMES = Object.freeze(['success', 'failure', 'skipped'])

/**
 * Project the sequential local runner onto the GitHub job graph before the
 * standard-library verdict consumes any evidence. The projection is strict so
 * adding a local prerequisite cannot silently escape the hosted-equivalent job
 * outcome.
 */
export function projectLocalHostedJobOutcomes(input) {
  exactKeys(input, ['contract', 'suiteShared', 'suites'], 'local hosted outcome input')
  exactKeys(
    input.contract,
    ['dependencyInstall', 'browserContract'],
    'local browser-contract job',
  )
  exactKeys(
    input.suiteShared,
    ['runtimeBuild', 'browserInstall', 'preflight', 'runtimeRetirement'],
    'local shared browser-suite operations',
  )
  exactKeys(input.suites, SUITES, 'local browser-suite outcomes')

  const contractJobOutcome = aggregateStartedJob([
    operationOutcome(input.contract.dependencyInstall, 'dependency install'),
    operationOutcome(input.contract.browserContract, 'browser contract'),
  ])
  const suites = {}
  for (const suite of SUITES) {
    const suiteInput = input.suites[suite]
    exactKeys(
      suiteInput,
      ['topology', 'product', 'guard', 'sealedEvidence'],
      `${suite} local browser-suite outcomes`,
    )
    const dependencyOutcome = contractJobOutcome
    const jobOutcome = dependencyOutcome === 'success'
      ? aggregateStartedJob([
          ...Object.entries(input.suiteShared).map(([name, value]) =>
            operationOutcome(value, `shared ${name}`)),
          ...Object.entries(suiteInput).map(([name, value]) =>
            operationOutcome(value, `${suite} ${name}`)),
        ])
      : 'skipped'
    suites[suite] = Object.freeze({ dependencyOutcome, jobOutcome })
  }

  return Object.freeze({
    contractJobOutcome,
    suites: Object.freeze(suites),
    // `browser-verdict` has `if: always()`: these are the exact values its
    // `needs.browser-{main,pion}.result` expressions would expose.
    verdictDependencies: Object.freeze(Object.fromEntries(SUITES.map((suite) => [
      suite,
      suites[suite].jobOutcome,
    ]))),
  })
}

function aggregateStartedJob(outcomes) {
  return outcomes.every((outcome) => outcome === 'success') ? 'success' : 'failure'
}

function operationOutcome(value, label) {
  if (!OPERATION_OUTCOMES.includes(value)) {
    throw new Error(`${label} outcome is not a hosted status`)
  }
  return value
}

function exactKeys(value, expected, label) {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  const actual = Object.keys(value).sort(compareStrings)
  const canonical = [...expected].sort(compareStrings)
  if (
    actual.length !== canonical.length ||
    actual.some((key, index) => key !== canonical[index])
  ) throw new Error(`${label} does not have its exact keys`)
}

function compareStrings(left, right) {
  if (left === right) return 0
  return left < right ? -1 : 1
}
