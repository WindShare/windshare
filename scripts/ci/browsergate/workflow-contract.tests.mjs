import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { browserRunPolicy } from '../../../web/scripts/browser-evidence/run-policy.ts'
import { createGithubSuiteJobDeadlinePolicy } from './operation-deadlines.mjs'

const REPOSITORY_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../../..')
const WORKFLOW_PATH = resolve(REPOSITORY_ROOT, '.github/workflows/ci.yml')
const GITHUB_EXPRESSION_OPEN = '${{'
const BROWSER_VERDICT_BUDGET_MINUTES = Object.freeze({
  job: 15,
  checkout: 2,
  download: 2,
  reducer: 3,
  reserve: 5,
})
const CRITICAL_JOB_RESERVE_MINUTES = Object.freeze({
  'browser-contract': 5,
  'browser-main': 10,
  'browser-pion': 10,
  'browser-verdict': BROWSER_VERDICT_BUDGET_MINUTES.reserve,
  'windows-browser-process': 5,
})

const workflow = readFileSync(WORKFLOW_PATH, 'utf8')
validateWorkflow(workflow)
verifyHostileMutations(workflow)

console.log('browser workflow DAG, isolation, artifact, and deadline contracts: PASS')

function validateWorkflow(source) {
  validateTriggers(source)
  validateExecutionSurface(source)

  const jobs = Object.freeze({
    'browser-contract': yamlSection(source, 'browser-contract', 2),
    'browser-main': yamlSection(source, 'browser-main', 2),
    'browser-pion': yamlSection(source, 'browser-pion', 2),
    'browser-verdict': yamlSection(source, 'browser-verdict', 2),
    'windows-browser-process': yamlSection(source, 'windows-browser-process', 2),
  })

  assert.deepEqual(jobNeeds(jobs['browser-contract']), [])
  assert.deepEqual(jobNeeds(jobs['browser-main']), ['browser-contract'])
  assert.deepEqual(jobNeeds(jobs['browser-pion']), ['browser-contract'])
  assert.deepEqual(jobNeeds(jobs['browser-verdict']), ['browser-main', 'browser-pion'])

  for (const [jobName, reserveMinutes] of Object.entries(CRITICAL_JOB_RESERVE_MINUTES)) {
    if (jobName === 'browser-main' || jobName === 'browser-pion') {
      validateSuiteDeadlineLease(
        jobs[jobName],
        jobName === 'browser-main' ? 'main' : 'pion',
      )
    } else {
      validateDeadlineLease(jobs[jobName], reserveMinutes, jobName)
    }
    validateCheckoutIsolation(jobs[jobName], jobName)
  }

  validateContractOwner(jobs['browser-contract'])
  validateSuiteProducer(jobs['browser-main'], 'main')
  validateSuiteProducer(jobs['browser-pion'], 'pion')
  validateVerdict(jobs['browser-verdict'])
  validateTokenIsolation(jobs)
  validateWindowsBoundary(jobs['windows-browser-process'])
}

function validateTriggers(source) {
  const triggers = yamlSection(source, 'on', 0)
  const expectedIgnoredPaths = ['docs/**', '**/*.md']
  assert.deepEqual(
    yamlListValues(yamlSection(triggers, 'push', 2), 'paths-ignore', 4),
    expectedIgnoredPaths,
    'push must exclude documentation-only changes',
  )
  assert.deepEqual(
    yamlListValues(yamlSection(triggers, 'pull_request', 2), 'paths-ignore', 4),
    expectedIgnoredPaths,
    'pull requests must exclude documentation-only changes',
  )
  assert(!/^  schedule:/mu.test(triggers), 'ordinary CI must not schedule a network matrix')
  assert(!/^  workflow_dispatch:/mu.test(triggers), 'ordinary CI must not expose a network dispatch')
}

function validateExecutionSurface(source) {
  assert(!/^\s*container:/gimu.test(source), 'ordinary CI must not add a container authority')
  assert(!/\b(?:docker|podman|oci)\b/iu.test(source), 'ordinary CI must not add an OCI runtime')
  assert(!source.includes('browser-network'), 'conditional network evidence has no ordinary CI job')
  assert(!source.includes('matrix.topology'), 'ordinary CI must not synthesize topology placeholders')
  assert(!source.includes('secrets.'), 'browser jobs must not inherit repository secrets')
}

function validateContractOwner(job) {
  assert.equal(
    countMatches(job, /pnpm -C web run test:browser:evidence:contract/gu),
    1,
    'browser evidence contract must have exactly one workflow owner',
  )
  assert.equal(artifactSteps(job).length, 0, 'contract job must not transfer artifacts')
}

function validateSuiteProducer(job, suite) {
  const producer = stepById(job, 'producer')
  const guard = stepById(job, 'guard')
  const upload = onlyStep(artifactSteps(job, 'upload'), `${suite} sealed upload`)
  const dispose = stepByName(job, `dispose authenticated ${suite === 'main' ? 'main' : 'Pion'} native runtime`)

  assert.match(producer, /node scripts\/ci\/browsergate\/main\.mjs hosted-produce\b/u)
  assert.match(producer, new RegExp('--suite ' + suite + '\\b', 'u'))
  assert.match(guard, /^        if: always\(\)$/mu)
  assert.match(guard, /node scripts\/ci\/browsergate\/main\.mjs guard-suite\b/u)
  assert.match(guard, new RegExp('--suite ' + suite + '\\b', 'u'))
  assert.match(upload, /^        if: always\(\) && steps\.guard\.outputs\.guard_outcome == 'passed'$/mu)
  assert.match(dispose, /^        if: >-$/mu)
  assert.match(dispose, /^          always\(\) &&$/mu)
  assert.match(dispose, /node scripts\/ci\/browsergate\/main\.mjs dispose-runtime\b/u)
  assert(
    !job.includes('continue-on-error:'),
    `${suite} producer must remain blocking across every lifecycle step`,
  )

  validateSuiteLifecycleOrder(job)
  validateSuiteArtifactBoundary(job, suite)
  assert.equal(artifactSteps(job, 'download').length, 0, `${suite} producer must not download artifacts`)
}

function validateSuiteLifecycleOrder(job) {
  const producer = stepById(job, 'producer')
  const guard = stepById(job, 'guard')
  const upload = onlyStep(artifactSteps(job, 'upload'), 'suite sealed upload')
  const dispose = stepSections(job).find((step) => step.includes('main.mjs dispose-runtime'))
  assert.notEqual(dispose, undefined, 'suite runtime disposal step is missing')
  assert(
    job.indexOf(producer) < job.indexOf(guard)
      && job.indexOf(guard) < job.indexOf(upload)
      && job.indexOf(upload) < job.indexOf(dispose),
    'producer, guard, sealed upload, and disposal must retain their failure-safe order',
  )
}

function validateSuiteArtifactBoundary(job, suite) {
  const uploads = artifactSteps(job, 'upload')
  assert.equal(uploads.length, 1, `${suite} must publish exactly one sealed directory`)
  const upload = uploads[0]
  assert.match(upload, new RegExp('^          name: browser-' + suite + '-guarded$', 'mu'))
  assert.match(upload, /^          if-no-files-found: error$/mu)
  assert.match(upload, /^          include-hidden-files: true$/mu)
  assert.deepEqual(
    mappingValues(upload, 'path', 10),
    [githubExpression('steps.guard.outputs.sealed_upload_path')],
    `${suite} upload must consume only the guard-owned sealed path`,
  )
  assert(!containsGlob(mappingValues(upload, 'path', 10)[0]))
}

function validateVerdict(job) {
  assert.match(job, /^    if: always\(\)$/mu)
  assert.equal(
    artifactSteps(job, 'upload').length,
    0,
    'verdict must not add an unguarded publication authority after reduction',
  )
  const steps = stepSections(job)
  assert.equal(
    steps.length,
    4,
    'verdict authority is exactly checkout, two sealed downloads, and the semantic reducer',
  )
  const checkout = steps.find((step) => step.includes('uses: actions/checkout@'))
  assert.notEqual(checkout, undefined, 'verdict checkout is missing')
  const downloads = artifactSteps(job, 'download')
  assert.equal(downloads.length, 2, 'verdict must consume exactly two sealed suite artifacts')
  const expected = Object.freeze([
    Object.freeze({ name: 'browser-main-guarded', path: 'browser-verdict-inputs/main' }),
    Object.freeze({ name: 'browser-pion-guarded', path: 'browser-verdict-inputs/pion' }),
  ])
  const actual = downloads.map((step) => {
    assert.match(step, /^        if: always\(\)$/mu)
    assert.match(step, /^        continue-on-error: true$/mu)
    assert(!step.includes('merge-multiple:'), 'verdict downloads must retain separate namespaces')
    assert(!step.includes('pattern:'), 'verdict downloads must name exact artifacts')
    const names = mappingValues(step, 'name', 10)
    const paths = mappingValues(step, 'path', 10)
    assert.equal(names.length, 1, 'verdict download must name one artifact')
    assert.equal(paths.length, 1, 'verdict download must select one exact path')
    assert(!containsGlob(paths[0]), 'verdict download path cannot contain a glob')
    return Object.freeze({ name: names[0], path: paths[0] })
  })
  assert.deepEqual(actual, expected)
  assert(!pathsOverlap(actual[0].path, actual[1].path), 'verdict inputs must be disjoint')

  const reducer = stepById(job, 'verdict')
  assert.match(reducer, /^        if: always\(\)$/mu)
  assert.match(reducer, /node scripts\/ci\/browsergate\/verdict\.mjs\b/u)
  assert(!reducer.includes('continue-on-error:'), 'reducer exit status must remain the job conclusion')
  assert.equal(steps.at(-1), reducer, 'semantic reducer must remain the final job authority')
  assert(
    job.indexOf(downloads[0]) < job.indexOf(downloads[1])
      && job.indexOf(downloads[1]) < job.indexOf(reducer),
    'verdict must download both sealed inputs before reducing',
  )
  assert.deepEqual(
    Object.freeze({
      job: deadlineSummary(job, 'browser-verdict').jobMinutes,
      checkout: stepTimeoutMinutes(checkout, 'browser-verdict checkout'),
      download: stepTimeoutMinutes(downloads[0], 'browser-verdict main download'),
      reducer: stepTimeoutMinutes(reducer, 'browser-verdict reducer'),
      reserve: CRITICAL_JOB_RESERVE_MINUTES['browser-verdict'],
    }),
    BROWSER_VERDICT_BUDGET_MINUTES,
    'verdict budget must retain the 15-minute hard ceiling and its 2+2+2+3 serial plan',
  )
  assert.equal(
    stepTimeoutMinutes(downloads[1], 'browser-verdict Pion download'),
    BROWSER_VERDICT_BUDGET_MINUTES.download,
  )
}

function validateTokenIsolation(jobs) {
  const criticalSteps = Object.entries(jobs)
    .flatMap(([jobName, job]) => stepSections(job).map((step) => ({ jobName, step })))
  const tokenSteps = criticalSteps.filter(({ step }) => hasCredentialAuthority(step))
  assert.equal(tokenSteps.length, 2, 'only the two suite guards may receive credentials')
  for (const { jobName, step } of tokenSteps) {
    assert(['browser-main', 'browser-pion'].includes(jobName))
    assert.equal(step, stepById(jobs[jobName], 'guard'))
    assert.match(step, /^          GITHUB_TOKEN: \$\{\{ github\.token \}\}$/mu)
    assert.match(step, /^          --secret-env GITHUB_TOKEN$/mu)
    assert(!/\bsecrets\./iu.test(step), 'suite guards must use only the explicit workflow token')
  }
  for (const [jobName, job] of Object.entries(jobs)) {
    const permittedGuard = ['browser-main', 'browser-pion'].includes(jobName)
      ? stepById(job, 'guard')
      : ''
    assert(
      !hasCredentialAuthority(job.replace(permittedGuard, '')),
      `${jobName} grants credentials outside its isolated suite guard`,
    )
  }
}

function hasCredentialAuthority(source) {
  return /\bGITHUB_TOKEN\b|\bgithub\.token\b|\bsecrets\.[A-Za-z0-9_]+\b/iu.test(source)
}

function validateCheckoutIsolation(job, label) {
  const checkouts = stepSections(job).filter((step) => step.includes('uses: actions/checkout@'))
  assert.equal(checkouts.length, 1, `${label} must have one checkout`)
  assert.match(checkouts[0], /^          persist-credentials: false$/mu)
}

function validateWindowsBoundary(job) {
  assert.deepEqual(jobNeeds(job), [], 'Windows process ownership remains an independent gate')
  assert.match(job, /^    runs-on: windows-latest$/mu)
  assert(!/^    if:/mu.test(job), 'Windows process ownership must not be conditionally suppressed')
  assert(!job.includes('continue-on-error:'), 'Windows process ownership must remain blocking')
  validateCheckoutIsolation(job, 'Windows process ownership')
  assert.match(job, /pnpm -C web run test:browser:process/u)
  assert.match(job, /go test \.\/web\/scripts\/browser-evidence\/windowsjob/u)
}

function validateDeadlineLease(job, reserveMinutes, label) {
  const summary = deadlineSummary(job, label)
  assert(
    summary.jobMinutes > summary.serialStepMinutes + reserveMinutes,
    `${label} job timeout must exceed all serial step ceilings plus its ${reserveMinutes}-minute reserve`,
  )
  return summary
}

function validateSuiteDeadlineLease(job, suite) {
  const policy = createGithubSuiteJobDeadlinePolicy(
    suite,
    browserRunPolicy('blocking'),
    'linux',
  )
  const summary = deadlineSummary(job, `browser-${suite}`)
  assert.equal(
    summary.jobMinutes,
    policy.minimumJobTimeoutMinutes,
    `${suite} job timeout must equal the ceiling of its versioned lease graph`,
  )
  assert.equal(
    summary.serialStepMinutes + policy.jobSettlementReserveMs / 60_000,
    policy.minimumJobTimeoutMinutes,
    `${suite} serial steps and settlement reserve must exactly cover the job authority`,
  )
}

function deadlineSummary(job, label) {
  const jobTimeouts = matchingIntegers(job, /^    timeout-minutes: (\d+)$/gmu)
  assert.equal(jobTimeouts.length, 1, `${label} must have one job timeout`)
  const timeouts = stepSections(job).map((step, index) => {
    assert(
      /^(?:      - uses:|        uses:|        run:)/mu.test(step),
      `${label} step ${index + 1} has no executable authority`,
    )
    return stepTimeoutMinutes(step, `${label} step ${index + 1}`)
  })
  return Object.freeze({
    jobMinutes: jobTimeouts[0],
    serialStepMinutes: timeouts.reduce((total, value) => total + value, 0),
  })
}

function stepTimeoutMinutes(step, label) {
  const values = matchingIntegers(step, /^        timeout-minutes: (\d+)$/gmu)
  assert.equal(values.length, 1, `${label} must have one timeout`)
  assert(values[0] > 0, `${label} timeout must be positive`)
  return values[0]
}

function verifyHostileMutations(source) {
  assert.throws(
    () => validateTriggers(source.replace('      - "docs/**"\n', '')),
    /documentation-only changes/u,
  )
  assert.throws(
    () => validateTriggers(source.replace('  pull_request:\n', '  workflow_dispatch:\n  pull_request:\n')),
    /network dispatch/u,
  )

  const contract = yamlSection(source, 'browser-contract', 2)
  const contractSummary = deadlineSummary(contract, 'browser-contract mutation source')
  const reserve = CRITICAL_JOB_RESERVE_MINUTES['browser-contract']
  assert.throws(
    () => validateDeadlineLease(
      replaceJobTimeout(contract, contractSummary.serialStepMinutes + reserve),
      reserve,
      'equality mutation',
    ),
    /must exceed/u,
  )
  assert.throws(
    () => validateDeadlineLease(
      replaceJobTimeout(contract, contractSummary.serialStepMinutes + reserve - 1),
      reserve,
      'underflow mutation',
    ),
    /must exceed/u,
  )
  assert.throws(
    () => validateDeadlineLease(
      contract.replace(/\n        timeout-minutes: \d+/u, ''),
      reserve,
      'missing step timeout mutation',
    ),
    /must have one timeout/u,
  )

  const main = yamlSection(source, 'browser-main', 2)
  const reordered = main
    .replace('id: producer', 'id: temporary-order-marker')
    .replace('id: guard', 'id: producer')
    .replace('id: temporary-order-marker', 'id: guard')
  assert.throws(() => validateSuiteLifecycleOrder(reordered), /failure-safe order/u)
  assert.throws(
    () => validateSuiteArtifactBoundary(
      main + '\n      - uses: actions/upload-artifact@v7\n        timeout-minutes: 1',
      'main',
    ),
    /exactly one sealed directory/u,
  )

  const leakedToken = main.replace(
    '        id: producer',
    '        id: producer\n        env:\n          GITHUB_TOKEN: ' + githubExpression('github.token'),
  )
  assert.throws(
    () => validateTokenIsolation({
      'browser-contract': contract,
      'browser-main': leakedToken,
      'browser-pion': yamlSection(source, 'browser-pion', 2),
      'browser-verdict': yamlSection(source, 'browser-verdict', 2),
      'windows-browser-process': yamlSection(source, 'windows-browser-process', 2),
    }),
    /only the two suite guards/u,
  )

  const leakedExpression = main.replace(
    '        id: producer',
    '        id: producer\n        env:\n          PRODUCER_CREDENTIAL: '
      + githubExpression('github.token'),
  )
  assert.throws(
    () => validateTokenIsolation({
      'browser-contract': contract,
      'browser-main': leakedExpression,
      'browser-pion': yamlSection(source, 'browser-pion', 2),
      'browser-verdict': yamlSection(source, 'browser-verdict', 2),
      'windows-browser-process': yamlSection(source, 'windows-browser-process', 2),
    }),
    /only the two suite guards/u,
  )

  const nonBlockingProducer = main.replace(
    '        id: producer',
    '        id: producer\n        continue-on-error: true',
  )
  assert.throws(
    () => validateSuiteProducer(nonBlockingProducer, 'main'),
    /must remain blocking/u,
  )

  const verdict = yamlSection(source, 'browser-verdict', 2)
  const republishedVerdict = verdict
    + '\n      - uses: actions/upload-artifact@v7\n        timeout-minutes: 1'
  assert.throws(
    () => validateVerdict(republishedVerdict),
    /must not add an unguarded publication authority/u,
  )
  const nonBlockingVerdict = verdict.replace(
    '        id: verdict',
    '        id: verdict\n        continue-on-error: true',
  )
  assert.throws(
    () => validateVerdict(nonBlockingVerdict),
    /must remain the job conclusion/u,
  )
}

function yamlSection(source, key, indentation) {
  const lines = source.split(/\r?\n/u)
  const prefix = ' '.repeat(indentation)
  const header = prefix + key + ':'
  const start = lines.findIndex((line) => line === header)
  assert.notEqual(start, -1, `workflow section ${key} is missing`)
  let end = lines.length
  for (let index = start + 1; index < lines.length; index += 1) {
    const line = lines[index]
    if (line.trim() === '' || line.trimStart().startsWith('#')) continue
    const currentIndentation = line.length - line.trimStart().length
    if (currentIndentation <= indentation) {
      end = index
      break
    }
  }
  return lines.slice(start, end).join('\n')
}

function yamlListValues(source, key, indentation) {
  return yamlSection(source, key, indentation).split(/\r?\n/u).slice(1).flatMap((line) => {
    const match = new RegExp('^' + ' '.repeat(indentation + 2) + '- (.+)$', 'u').exec(line)
    return match === null ? [] : [unquote(match[1])]
  })
}

function jobNeeds(job) {
  const scalar = /^    needs: ([a-z0-9-]+)$/mu.exec(job)
  if (scalar !== null) return [scalar[1]]
  if (!/^    needs:$/mu.test(job)) return []
  return yamlListValues(job, 'needs', 4)
}

function stepSections(job) {
  const lines = job.split(/\r?\n/u)
  const header = lines.findIndex((line) => line === '    steps:')
  assert.notEqual(header, -1, 'workflow job has no steps section')
  const starts = []
  for (let index = header + 1; index < lines.length; index += 1) {
    if (lines[index].startsWith('      - ')) starts.push(index)
  }
  return starts.map((start, index) =>
    lines.slice(start, starts[index + 1] ?? lines.length).join('\n'))
}

function stepById(job, id) {
  const step = stepSections(job).find((candidate) =>
    candidate.split(/\r?\n/u).some((line) => line.trim() === 'id: ' + id))
  assert.notEqual(step, undefined, `workflow step ${id} is missing`)
  return step
}

function stepByName(job, name) {
  const step = stepSections(job).find((candidate) =>
    candidate.split(/\r?\n/u).some((line) => line.trim() === '- name: ' + name))
  assert.notEqual(step, undefined, `workflow step ${name} is missing`)
  return step
}

function artifactSteps(job, direction) {
  const prefix = direction === undefined ? 'actions/' : `actions/${direction}-artifact@`
  return stepSections(job).filter((step) => {
    if (direction === undefined) return /uses: actions\/(?:upload|download)-artifact@/u.test(step)
    return step.includes('uses: ' + prefix)
  })
}

function mappingValues(source, key, indentation) {
  const pattern = new RegExp('^' + ' '.repeat(indentation) + key + ': (.+)$', 'gmu')
  return [...source.matchAll(pattern)].map((match) => unquote(match[1]))
}

function matchingIntegers(source, pattern) {
  return [...source.matchAll(pattern)].map((match) => Number(match[1]))
}

function onlyStep(steps, label) {
  assert.equal(steps.length, 1, `${label} must have exactly one step`)
  return steps[0]
}

function replaceJobTimeout(job, timeoutMinutes) {
  return job.replace(/^    timeout-minutes: \d+$/mu, `    timeout-minutes: ${timeoutMinutes}`)
}

function githubExpression(value) {
  return `${GITHUB_EXPRESSION_OPEN} ${value} }}`
}

function containsGlob(value) {
  return /[*?\[\]]/u.test(value)
}

function pathsOverlap(left, right) {
  return left === right || left.startsWith(right + '/') || right.startsWith(left + '/')
}

function countMatches(source, pattern) {
  return [...source.matchAll(pattern)].length
}

function unquote(value) {
  if (
    value.length >= 2
    && ((value.startsWith('"') && value.endsWith('"'))
      || (value.startsWith("'") && value.endsWith("'")))
  ) return value.slice(1, -1)
  return value
}
