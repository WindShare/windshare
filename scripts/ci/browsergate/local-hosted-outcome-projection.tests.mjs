import assert from 'node:assert/strict'

import { projectLocalHostedJobOutcomes } from './local-hosted-outcome-projection.mjs'

const HOSTED_JOB_OUTCOMES = Object.freeze(['success', 'failure', 'cancelled', 'skipped'])

verifyHappyProjection()
verifyContractDependencyFailuresSkipSuites()
verifySharedSuiteOperationFailuresFailBothJobs()
verifySuitePrerequisiteFailuresRemainSuiteLocal()
verifyProjectionIsClosedOverItsPrerequisites()

process.stdout.write('local/hosted browser outcome projection contracts: PASS\n')

function verifyHappyProjection() {
  assert.deepEqual(projectLocalHostedJobOutcomes(successfulInput()), {
    contractJobOutcome: 'success',
    suites: {
      main: { dependencyOutcome: 'success', jobOutcome: 'success' },
      pion: { dependencyOutcome: 'success', jobOutcome: 'success' },
    },
    verdictDependencies: { main: 'success', pion: 'success' },
  })
}

function verifyContractDependencyFailuresSkipSuites() {
  for (const prerequisite of ['dependencyInstall', 'browserContract']) {
    const input = successfulInput()
    input.contract[prerequisite] = 'failure'
    const projected = projectLocalHostedJobOutcomes(input)
    assert.equal(projected.contractJobOutcome, 'failure', prerequisite)
    assert.deepEqual(projected.suites, {
      main: { dependencyOutcome: 'failure', jobOutcome: 'skipped' },
      pion: { dependencyOutcome: 'failure', jobOutcome: 'skipped' },
    }, prerequisite)
    assert.deepEqual(projected.verdictDependencies, {
      main: 'skipped',
      pion: 'skipped',
    }, prerequisite)
    assertHostedVocabulary(projected)
  }
}

function verifySharedSuiteOperationFailuresFailBothJobs() {
  for (const prerequisite of [
    'runtimeBuild',
    'browserInstall',
    'preflight',
    'runtimeRetirement',
  ]) {
    const input = successfulInput()
    input.suiteShared[prerequisite] = 'failure'
    const projected = projectLocalHostedJobOutcomes(input)
    assert.equal(projected.contractJobOutcome, 'success', prerequisite)
    assert.deepEqual(projected.verdictDependencies, {
      main: 'failure',
      pion: 'failure',
    }, prerequisite)
    assertHostedVocabulary(projected)
  }
}

function verifySuitePrerequisiteFailuresRemainSuiteLocal() {
  for (const suite of ['main', 'pion']) {
    const sibling = suite === 'main' ? 'pion' : 'main'
    for (const prerequisite of ['topology', 'product', 'guard', 'sealedEvidence']) {
      const input = successfulInput()
      input.suites[suite][prerequisite] = 'failure'
      const projected = projectLocalHostedJobOutcomes(input)
      assert.equal(projected.verdictDependencies[suite], 'failure', `${suite} ${prerequisite}`)
      assert.equal(projected.verdictDependencies[sibling], 'success', `${suite} ${prerequisite}`)
      assert.equal(projected.suites[suite].dependencyOutcome, 'success')
      assertHostedVocabulary(projected)
    }
  }
}

function verifyProjectionIsClosedOverItsPrerequisites() {
  const missing = successfulInput()
  delete missing.suites.main.guard
  assert.throws(
    () => projectLocalHostedJobOutcomes(missing),
    /does not have its exact keys/u,
  )

  const extra = successfulInput()
  extra.suiteShared.unmodelledSetup = 'success'
  assert.throws(
    () => projectLocalHostedJobOutcomes(extra),
    /does not have its exact keys/u,
  )

  const invalid = successfulInput()
  invalid.suites.pion.product = 'passed'
  assert.throws(
    () => projectLocalHostedJobOutcomes(invalid),
    /not a hosted status/u,
  )
}

function assertHostedVocabulary(projected) {
  assert(HOSTED_JOB_OUTCOMES.includes(projected.contractJobOutcome))
  for (const suite of ['main', 'pion']) {
    assert(HOSTED_JOB_OUTCOMES.includes(projected.suites[suite].dependencyOutcome))
    assert(HOSTED_JOB_OUTCOMES.includes(projected.suites[suite].jobOutcome))
    assert(HOSTED_JOB_OUTCOMES.includes(projected.verdictDependencies[suite]))
  }
}

function successfulInput() {
  return {
    contract: {
      dependencyInstall: 'success',
      browserContract: 'success',
    },
    suiteShared: {
      runtimeBuild: 'success',
      browserInstall: 'success',
      preflight: 'success',
      runtimeRetirement: 'success',
    },
    suites: {
      main: {
        topology: 'success',
        product: 'success',
        guard: 'success',
        sealedEvidence: 'success',
      },
      pion: {
        topology: 'success',
        product: 'success',
        guard: 'success',
        sealedEvidence: 'success',
      },
    },
  }
}
