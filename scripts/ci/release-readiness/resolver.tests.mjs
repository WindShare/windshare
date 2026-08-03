import assert from 'node:assert/strict'
import { existsSync } from 'node:fs'
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'

import {
  parseResolutionManifestJSON,
  releaseWorkflowOutputs,
  serializeResolutionManifest,
} from './contract.mjs'
import {
  GITHUB_ACTIONS_API_VERSION,
  MAXIMUM_COLLECTION_ITEMS,
  MAXIMUM_LIST_PAGES,
  PAGE_SIZE,
  createGitHubActionsClient,
} from './github-actions.mjs'
import {
  ReleaseEvidenceResolutionError,
  resolveReleaseEvidence,
  runResolverCLI,
} from './resolver.mjs'

const REPOSITORY = 'WindShare/windshare'
const DEFAULT_BRANCH = 'main'
const TARGET_SHA = 'a'.repeat(40)
const TOKEN = 'test-token'
const REPOSITORY_ID = 1001
const NO_TRACE = () => {}

const AUTHORITIES = Object.freeze({
  ci: Object.freeze({
    workflowId: 2001,
    workflowFile: 'ci.yml',
    workflowName: 'CI',
    workflowPath: '.github/workflows/ci.yml',
    runId: 3001,
    runAttempt: 1,
    event: 'push',
    createdAt: '2026-08-03T10:00:00Z',
    jobNames: ['CI Required Verdict'],
    artifactRoles: [],
  }),
  full_browser: Object.freeze({
    workflowId: 2002,
    workflowFile: 'browser-full.yml',
    workflowName: 'Full Browser',
    workflowPath: '.github/workflows/browser-full.yml',
    runId: 3002,
    runAttempt: 2,
    event: 'schedule',
    createdAt: '2026-08-03T11:00:00.000Z',
    jobNames: ['Run the token-free full browser orchestrator'],
    artifactRoles: ['browser'],
  }),
  stability: Object.freeze({
    workflowId: 2003,
    workflowFile: 'stability.yml',
    workflowName: 'Native Integration Stability',
    workflowPath: '.github/workflows/stability.yml',
    runId: 3003,
    runAttempt: 1,
    event: 'schedule',
    createdAt: '2026-08-03T12:00:00Z',
    jobNames: [
      'Native integration stability (Linux)',
      'Native integration stability (Windows)',
    ],
    artifactRoles: ['linux', 'windows'],
  }),
})

test('resolves every exact-SHA authority into one immutable canonical manifest', async () => {
  const state = createState()
  // The attempt-scoped route is sufficient proof when GitHub omits this optional field.
  delete state.jobPages.get('3001:1:1').jobs[0].run_attempt
  // Both observed GitHub run.path forms are valid semantic representations.
  state.attemptRuns.get('3002:2').path =
    '.github/workflows/browser-full.yml@main'

  const manifest = await resolve(state)
  assert.deepEqual(manifest, {
    schema_version: 'windshare.release-readiness-resolution/v1',
    target_sha: TARGET_SHA,
    repository: {
      id: String(REPOSITORY_ID),
      full_name: REPOSITORY,
      default_branch: DEFAULT_BRANCH,
    },
    authorities: {
      ci: {
        workflow_id: '2001',
        run_id: '3001',
        run_attempt: 1,
        event: 'push',
        terminal_job_ids: ['4001'],
        artifacts: [],
      },
      full_browser: {
        workflow_id: '2002',
        run_id: '3002',
        run_attempt: 2,
        event: 'schedule',
        terminal_job_ids: ['4002'],
        artifacts: [{
          role: 'browser',
          id: '5001',
          name: `browser-full-${TARGET_SHA}-3002-2`,
          size_in_bytes: 101,
          digest: `sha256:${'b'.repeat(64)}`,
        }],
      },
      stability: {
        workflow_id: '2003',
        run_id: '3003',
        run_attempt: 1,
        event: 'schedule',
        terminal_job_ids: ['4003', '4004'],
        artifacts: [
          {
            role: 'linux',
            id: '5002',
            name: `stability-integration-linux-${TARGET_SHA}-3003-1`,
            size_in_bytes: 102,
            digest: `sha256:${'c'.repeat(64)}`,
          },
          {
            role: 'windows',
            id: '5003',
            name: `stability-integration-windows-${TARGET_SHA}-3003-1`,
            size_in_bytes: 103,
            digest: `sha256:${'d'.repeat(64)}`,
          },
        ],
      },
    },
  })
  assert.equal(Object.isFrozen(manifest), true)
  assert.equal(Object.isFrozen(manifest.authorities.stability.artifacts[0]), true)
  assert.deepEqual(releaseWorkflowOutputs(manifest), {
    ci_run_id: '3001',
    ci_run_attempt: '1',
    browser_run_id: '3002',
    browser_run_attempt: '2',
    browser_artifact_id: '5001',
    stability_run_id: '3003',
    stability_run_attempt: '1',
    stability_linux_artifact_id: '5002',
    stability_windows_artifact_id: '5003',
  })

  const canonical = serializeResolutionManifest(manifest)
  assert.deepEqual(
    parseResolutionManifestJSON(canonical, {
      repository: REPOSITORY,
      targetSha: TARGET_SHA,
    }),
    parseResolutionManifestJSON(canonical),
  )
  assert.ok(canonical.endsWith('\n'))
  assert.equal(state.calls.some(({ url }) =>
    url.pathname === '/repos/WindShare/windshare/actions/runs/3001/artifacts'), false)

  for (const { options } of state.calls) {
    assert.equal(options.method, 'GET')
    assert.equal(options.redirect, 'error')
    assert.equal(options.headers.Accept, 'application/vnd.github+json')
    assert.equal(options.headers.Authorization, `Bearer ${TOKEN}`)
    assert.equal(
      options.headers['X-GitHub-Api-Version'],
      GITHUB_ACTIONS_API_VERSION,
    )
    assert.equal(options.headers['User-Agent'], 'windshare-release-readiness-resolver')
    assert.ok(options.signal instanceof AbortSignal)
  }

  const runQuery = state.calls.find(({ url }) =>
    url.pathname.endsWith('/actions/workflows/2001/runs') &&
    url.searchParams.get('event') === 'push')
  assert.equal(runQuery.url.searchParams.get('branch'), DEFAULT_BRANCH)
  assert.equal(runQuery.url.searchParams.get('head_sha'), TARGET_SHA)
  assert.equal(runQuery.url.searchParams.get('status'), 'success')
  assert.equal(runQuery.url.searchParams.get('per_page'), String(PAGE_SIZE))
  assert.equal(runQuery.url.searchParams.get('page'), '1')
  assert.ok(state.calls.some(({ url }) =>
    url.pathname === '/repos/WindShare/windshare/actions/runs/3002/attempts/2/jobs' &&
    url.search === '?per_page=100&page=1'))
})

test('emits correlation-rich milestones without exposing the API token', async () => {
  const state = createState()
  const records = []
  await resolveReleaseEvidence({
    repository: REPOSITORY,
    defaultBranch: DEFAULT_BRANCH,
    targetSha: TARGET_SHA,
    token: TOKEN,
    fetchImpl: state.fetchImpl,
    traceImpl: (record) => records.push(record),
  })
  for (const authorityKey of Object.keys(AUTHORITIES)) {
    for (const milestone of [
      'workflow-metadata-validated',
      'workflow-run-selected',
      'workflow-run-attempt-validated',
      'terminal-jobs-validated',
      'artifacts-associated',
      'workflow-run-final-state-validated',
    ]) {
      const record = records.find((candidate) =>
        candidate.authority === authorityKey &&
        candidate.milestone === milestone)
      assert.equal(record.target_sha, TARGET_SHA)
      assert.match(record.run_id, /^[1-9][0-9]*$/u)
      assert.ok(Number.isSafeInteger(record.run_attempt))
    }
  }
  assert.equal(JSON.stringify(records).includes(TOKEN), false)
})

test('selects the newest distinct run across allowed events with numeric ID tie-breaking', async () => {
  const state = createState()
  const older = state.runPages.get('2001:push:1').workflow_runs[0]
  older.created_at = '2026-08-03T13:00:00Z'
  const selected = clone(older)
  selected.id = 3010
  selected.event = 'workflow_dispatch'
  selected.created_at = older.created_at
  installRunEvidence(state, 'ci', selected, {
    jobIDs: [4010],
    artifactIDs: [],
  })
  state.runPages.set('2001:workflow_dispatch:1', collection('workflow_runs', [selected]))

  const manifest = await resolve(state)
  assert.equal(manifest.authorities.ci.run_id, '3010')
  assert.equal(manifest.authorities.ci.event, 'workflow_dispatch')
})

test('rejects a duplicate run representation across allowed-event searches', async () => {
  const state = createState()
  const duplicate = clone(state.runPages.get('2001:push:1').workflow_runs[0])
  duplicate.event = 'workflow_dispatch'
  state.runPages.set(
    '2001:workflow_dispatch:1',
    collection('workflow_runs', [duplicate]),
  )
  await rejectsWithCode(() => resolve(state), 'duplicate-run')
})

test('fails with missing-run when an exact-SHA authority has no candidate', async () => {
  const state = createState()
  state.runPages.set('2001:push:1', collection('workflow_runs', []))
  await rejectsWithCode(() => resolve(state), 'missing-run')
})

test('does not fall back when the newest selected run has broken evidence', async () => {
  const state = createState()
  const older = state.runPages.get('2001:push:1').workflow_runs[0]
  older.created_at = '2026-08-03T10:00:00Z'
  const newer = clone(older)
  newer.id = 3011
  newer.created_at = '2026-08-03T10:00:01Z'
  installRunEvidence(state, 'ci', newer, {
    jobIDs: [4011],
    artifactIDs: [],
  })
  state.jobPages.set('3011:1:1', collection('jobs', []))
  state.runPages.set('2001:push:1', collection('workflow_runs', [newer, older]))

  await rejectsWithCode(() => resolve(state), 'missing-terminal-job')
})

for (const [label, mutate] of [
  ['wrong SHA', (run) => { run.head_sha = 'f'.repeat(40) }],
  ['wrong workflow ID', (run) => { run.workflow_id = 9999 }],
  ['wrong workflow name', (run) => { run.name = 'Not CI' }],
  ['wrong workflow path', (run) => { run.path = '.github/workflows/ci.yml@other' }],
  ['wrong branch', (run) => { run.head_branch = 'other' }],
  ['wrong event', (run) => { run.event = 'pull_request' }],
  ['wrong repository', (run) => { run.repository.id = 9999 }],
  ['wrong head repository', (run) => { run.head_repository.id = 9999 }],
  ['non-terminal status', (run) => { run.status = 'in_progress' }],
  ['failed conclusion', (run) => { run.conclusion = 'failure' }],
]) {
  test(`rejects a query row with ${label}`, async () => {
    const state = createState()
    mutate(state.runPages.get('2001:push:1').workflow_runs[0])
    await rejectsWithCode(() => resolve(state), 'invalid-run-identity')
  })
}

test('rejects repository and fixed workflow metadata drift before selecting runs', async () => {
  const repositoryState = createState()
  repositoryState.repository.default_branch = 'other'
  await rejectsWithCode(
    () => resolve(repositoryState),
    'invalid-repository-identity',
  )

  const workflowState = createState()
  workflowState.workflows.get('ci.yml').state = 'disabled_manually'
  await rejectsWithCode(
    () => resolve(workflowState),
    'invalid-workflow-identity',
  )
})

test('rejects attempt metadata drift and a changed final run', async () => {
  const attemptState = createState()
  attemptState.attemptRuns.get('3002:2').run_attempt = 1
  await rejectsWithCode(() => resolve(attemptState), 'invalid-run-identity')

  const finalState = createState()
  finalState.baseRuns.get(3002).run_attempt = 3
  await rejectsWithCode(() => resolve(finalState), 'invalid-run-identity')
})

test('classifies missing and duplicate exact terminal display names before identity checks', async () => {
  const missing = createState()
  missing.jobPages.get('3001:1:1').jobs[0].name = 'CI required verdict'
  await rejectsWithCode(() => resolve(missing), 'missing-terminal-job')

  const duplicate = createState()
  const original = duplicate.jobPages.get('3001:1:1').jobs[0]
  const second = clone(original)
  second.id = 4099
  second.status = 'queued'
  duplicate.jobPages.set(
    '3001:1:1',
    collection('jobs', [original, second]),
  )
  await rejectsWithCode(() => resolve(duplicate), 'duplicate-terminal-job')
})

for (const [label, mutate] of [
  ['stale conclusion', (job) => { job.conclusion = 'stale' }],
  ['non-terminal status', (job) => { job.status = 'in_progress' }],
  ['wrong run', (job) => { job.run_id = 9999 }],
  ['wrong attempt', (job) => { job.run_attempt = 1 }],
  ['null returned attempt', (job) => { job.run_attempt = null }],
  ['wrong SHA', (job) => { job.head_sha = 'e'.repeat(40) }],
  ['wrong branch', (job) => { job.head_branch = 'other' }],
  ['wrong workflow', (job) => { job.workflow_name = 'Other' }],
]) {
  test(`rejects terminal job with ${label}`, async () => {
    const state = createState()
    mutate(state.jobPages.get('3002:2:1').jobs[0])
    await rejectsWithCode(() => resolve(state), 'invalid-terminal-job')
  })
}

test('rejects missing and duplicate exact-case artifact identities', async () => {
  const missing = createState()
  const browserKey = artifactPageKey(
    3002,
    `browser-full-${TARGET_SHA}-3002-2`,
    1,
  )
  missing.artifactPages.get(browserKey).artifacts[0].name =
    `Browser-full-${TARGET_SHA}-3002-2`
  await rejectsWithCode(() => resolve(missing), 'missing-artifact')

  const duplicate = createState()
  const original = duplicate.artifactPages.get(browserKey).artifacts[0]
  const second = clone(original)
  second.id = 5099
  duplicate.artifactPages.set(
    browserKey,
    collection('artifacts', [original, second]),
  )
  await rejectsWithCode(() => resolve(duplicate), 'duplicate-artifact')
})

for (const [label, code, mutate] of [
  ['expired state', 'expired-artifact', (artifact) => { artifact.expired = true }],
  ['missing expiry state', 'expired-artifact', (artifact) => { delete artifact.expired }],
  ['zero size', 'invalid-artifact-size', (artifact) => { artifact.size_in_bytes = 0 }],
  ['unsafe size', 'invalid-artifact-size', (artifact) => {
    artifact.size_in_bytes = Number.MAX_SAFE_INTEGER + 1
  }],
  ['uppercase digest', 'invalid-artifact-digest', (artifact) => {
    artifact.digest = `sha256:${'B'.repeat(64)}`
  }],
  ['wrong digest algorithm', 'invalid-artifact-digest', (artifact) => {
    artifact.digest = `sha512:${'b'.repeat(64)}`
  }],
  ['wrong run association', 'invalid-artifact-association', (artifact) => {
    artifact.workflow_run.id = 9999
  }],
  ['wrong SHA association', 'invalid-artifact-association', (artifact) => {
    artifact.workflow_run.head_sha = 'e'.repeat(40)
  }],
  ['wrong branch association', 'invalid-artifact-association', (artifact) => {
    artifact.workflow_run.head_branch = 'other'
  }],
  ['wrong repository association', 'invalid-artifact-association', (artifact) => {
    artifact.workflow_run.repository_id = 9999
  }],
  ['wrong head repository association', 'invalid-artifact-association', (artifact) => {
    artifact.workflow_run.head_repository_id = 9999
  }],
]) {
  test(`rejects artifact with ${label}`, async () => {
    const state = createState()
    const key = artifactPageKey(
      3002,
      `browser-full-${TARGET_SHA}-3002-2`,
      1,
    )
    mutate(state.artifactPages.get(key).artifacts[0])
    await rejectsWithCode(() => resolve(state), code)
  })
}

test('ignores unrelated artifacts only after validating their structural IDs', async () => {
  const accepted = createState()
  const key = artifactPageKey(
    3002,
    `browser-full-${TARGET_SHA}-3002-2`,
    1,
  )
  const required = accepted.artifactPages.get(key).artifacts[0]
  accepted.artifactPages.set(
    key,
    collection('artifacts', [
      {
        id: 5098,
        name: 'unrelated',
      },
      required,
    ]),
  )
  const manifest = await resolve(accepted)
  assert.equal(manifest.authorities.full_browser.artifacts[0].id, '5001')

  const rejected = createState()
  const rejectedRequired = rejected.artifactPages.get(key).artifacts[0]
  rejected.artifactPages.set(
    key,
    collection('artifacts', [
      {
        id: '5098',
        name: 'unrelated',
      },
      rejectedRequired,
    ]),
  )
  await rejectsWithCode(() => resolve(rejected), 'invalid-pagination')
})

test('rejects duplicate selected artifact IDs across independently paginated roles', async () => {
  const state = createState()
  const windowsKey = artifactPageKey(
    3003,
    `stability-integration-windows-${TARGET_SHA}-3003-1`,
    1,
  )
  state.artifactPages.get(windowsKey).artifacts[0].id = 5002
  await assert.rejects(
    () => resolve(state),
    (error) => error?.code === 'invalid-resolution-contract',
  )
})

test('canonical manifest parser rejects noncanonical or identity-changing bytes', async () => {
  const manifest = await resolve(createState())
  const canonical = serializeResolutionManifest(manifest)
  assert.throws(
    () => parseResolutionManifestJSON(` ${canonical}`),
    /not canonical JSON/u,
  )
  assert.throws(
    () => parseResolutionManifestJSON(canonical, { targetSha: 'f'.repeat(40) }),
    /target SHA does not match/u,
  )
  const changed = JSON.parse(canonical)
  changed.authorities.full_browser.artifacts[0].digest =
    `sha256:${'B'.repeat(64)}`
  assert.throws(
    () => parseResolutionManifestJSON(`${JSON.stringify(changed)}\n`),
    /lowercase sha256 digest/u,
  )
})

test('collection reader accepts bounded multi-page results and exact empty results', async () => {
  const runs = Array.from({ length: 101 }, (_, index) => ({
    id: 10_000 - index,
    created_at: new Date(Date.UTC(2026, 7, 3, 12, 0, 0) - index * 1000)
      .toISOString()
      .replace('.000Z', 'Z'),
  }))
  const fetchImpl = collectionFetch('workflow_runs', new Map([
    [1, { total_count: 101, workflow_runs: runs.slice(0, 100) }],
    [2, { total_count: 101, workflow_runs: runs.slice(100) }],
  ]))
  const client = createClient(fetchImpl)
  assert.equal((await listRuns(client)).length, 101)

  const emptyClient = createClient(collectionFetch(
    'workflow_runs',
    new Map([[1, { total_count: 0, workflow_runs: [] }]]),
  ))
  assert.deepEqual(await listRuns(emptyClient), [])
})

for (const [label, pages, code = 'invalid-pagination'] of [
  [
    'declared count above the collection bound',
    new Map([[1, { total_count: MAXIMUM_COLLECTION_ITEMS + 1, jobs: [] }]]),
  ],
  [
    'an oversized page',
    new Map([[1, {
      total_count: 101,
      jobs: Array.from({ length: 101 }, (_, index) => ({ id: index + 1 })),
    }]]),
  ],
  [
    'a changing total_count',
    new Map([
      [1, { total_count: 2, jobs: [{ id: 1 }] }],
      [2, { total_count: 3, jobs: [{ id: 2 }] }],
    ]),
  ],
  [
    'an overlapping duplicate ID',
    new Map([
      [1, { total_count: 2, jobs: [{ id: 1 }] }],
      [2, { total_count: 2, jobs: [{ id: 1 }] }],
    ]),
    'duplicate-collection-item',
  ],
  [
    'an early empty page',
    new Map([
      [1, { total_count: 2, jobs: [{ id: 1 }] }],
      [2, { total_count: 2, jobs: [] }],
    ]),
  ],
  [
    'accumulation beyond total_count',
    new Map([[1, { total_count: 1, jobs: [{ id: 1 }, { id: 2 }] }]]),
  ],
  [
    'ten partial pages before the declared count',
    new Map(Array.from({ length: MAXIMUM_LIST_PAGES }, (_, index) => [
      index + 1,
      { total_count: 11, jobs: [{ id: index + 1 }] },
    ])),
  ],
]) {
  test(`collection reader rejects ${label}`, async () => {
    const client = createClient(collectionFetch('jobs', pages))
    await rejectsWithCode(
      () => client.listAttemptJobs({ runId: 1, runAttempt: 1 }),
      code,
    )
  })
}

test('collection reader accepts exactly 1,000 items across its ten-page bound', async () => {
  const pages = new Map()
  for (let page = 1; page <= MAXIMUM_LIST_PAGES; page += 1) {
    pages.set(page, {
      total_count: MAXIMUM_COLLECTION_ITEMS,
      jobs: Array.from({ length: PAGE_SIZE }, (_, index) => ({
        id: (page - 1) * PAGE_SIZE + index + 1,
      })),
    })
  }
  const client = createClient(collectionFetch('jobs', pages))
  assert.equal(
    (await client.listAttemptJobs({ runId: 1, runAttempt: 1 })).length,
    MAXIMUM_COLLECTION_ITEMS,
  )
})

test('workflow-run ordering is enforced across page boundaries', async () => {
  const client = createClient(collectionFetch('workflow_runs', new Map([
    [1, {
      total_count: 2,
      workflow_runs: [{ id: 10, created_at: '2026-08-03T10:00:00Z' }],
    }],
    [2, {
      total_count: 2,
      workflow_runs: [{ id: 11, created_at: '2026-08-03T10:00:00Z' }],
    }],
  ])))
  await rejectsWithCode(() => listRuns(client), 'invalid-pagination')
})

test('collection reader rejects malformed envelopes and unrelated invalid IDs', async () => {
  const malformed = createClient(async () => apiResponse([]))
  await rejectsWithCode(() => listRuns(malformed), 'invalid-pagination')

  const invalidID = createClient(collectionFetch('jobs', new Map([
    [1, { total_count: 1, jobs: [{ id: '1', name: 'unrelated' }] }],
  ])))
  await rejectsWithCode(
    () => invalidID.listAttemptJobs({ runId: 1, runAttempt: 1 }),
    'invalid-pagination',
  )
})

test('request layer fails closed on HTTP, malformed JSON, redirect, and timeout', async () => {
  const httpClient = createClient(async () => apiResponse({}, 429))
  await rejectsWithCode(() => httpClient.getRepository(), 'github-request-failed')

  const inconsistentHTTPClient = createClient(async () => ({
    ...apiResponse({}),
    status: 500,
    ok: true,
  }))
  await rejectsWithCode(
    () => inconsistentHTTPClient.getRepository(),
    'github-request-failed',
  )

  const networkClient = createClient(async () => {
    throw new Error('network unavailable')
  })
  await rejectsWithCode(
    () => networkClient.getRepository(),
    'github-request-failed',
  )

  const malformedClient = createClient(async () => ({
    ok: true,
    status: 200,
    redirected: false,
    async json() {
      throw new SyntaxError('bad JSON')
    },
  }))
  await rejectsWithCode(() => malformedClient.getRepository(), 'invalid-github-json')

  const redirectClient = createClient(async () => ({
    ok: true,
    status: 200,
    redirected: true,
    async json() {
      return {}
    },
  }))
  await rejectsWithCode(
    () => redirectClient.getRepository(),
    'invalid-github-response',
  )

  const timeoutClient = createGitHubActionsClient({
    repository: REPOSITORY,
    token: TOKEN,
    fetchImpl: async () => new Promise(() => {}),
    requestTimeoutMilliseconds: 5,
  })
  await rejectsWithCode(
    () => timeoutClient.getRepository(),
    'github-request-timeout',
  )
})

test('CLI publishes only fixed outputs after resolution and re-resolution identity checks', async () => {
  const temporaryDirectory = await mkdtemp(join(tmpdir(), 'windshare-resolver-'))
  try {
    const outputPath = join(temporaryDirectory, 'resolution.json')
    const githubOutputPath = join(temporaryDirectory, 'github-output.txt')
    await writeFile(githubOutputPath, '', 'utf8')
    const firstState = createState()
    const baseArguments = [
      '--repository', REPOSITORY,
      '--default-branch', DEFAULT_BRANCH,
      '--target-sha', TARGET_SHA,
    ]
    const { manifest, outputs } = await runResolverCLI({
      arguments_: [
        ...baseArguments,
        '--output', outputPath,
        '--github-output', githubOutputPath,
      ],
      environment: { GITHUB_TOKEN: TOKEN },
      fetchImpl: firstState.fetchImpl,
      traceImpl: NO_TRACE,
    })
    assert.equal(
      await readFile(outputPath, 'utf8'),
      serializeResolutionManifest(manifest),
    )
    assert.equal(
      await readFile(githubOutputPath, 'utf8'),
      [
        'ci_run_id=3001',
        'ci_run_attempt=1',
        'browser_run_id=3002',
        'browser_run_attempt=2',
        'browser_artifact_id=5001',
        'stability_run_id=3003',
        'stability_run_attempt=1',
        'stability_linux_artifact_id=5002',
        'stability_windows_artifact_id=5003',
        '',
      ].join('\n'),
    )

    const expectedArguments = [
      '--expect-ci-run-id', outputs.ci_run_id,
      '--expect-ci-run-attempt', outputs.ci_run_attempt,
      '--expect-browser-run-id', outputs.browser_run_id,
      '--expect-browser-run-attempt', outputs.browser_run_attempt,
      '--expect-browser-artifact-id', outputs.browser_artifact_id,
      '--expect-stability-run-id', outputs.stability_run_id,
      '--expect-stability-run-attempt', outputs.stability_run_attempt,
      '--expect-stability-linux-artifact-id',
      outputs.stability_linux_artifact_id,
      '--expect-stability-windows-artifact-id',
      outputs.stability_windows_artifact_id,
    ]
    const verifiedOutput = join(temporaryDirectory, 'verified-resolution.json')
    await runResolverCLI({
      arguments_: [
        ...baseArguments,
        '--output', verifiedOutput,
        ...expectedArguments,
      ],
      environment: { GITHUB_TOKEN: TOKEN },
      fetchImpl: createState().fetchImpl,
      traceImpl: NO_TRACE,
    })
    assert.equal(
      await readFile(verifiedOutput, 'utf8'),
      serializeResolutionManifest(manifest),
    )

    const changedOutput = join(temporaryDirectory, 'changed-resolution.json')
    const changedArguments = [...expectedArguments]
    changedArguments[1] = '9999'
    await rejectsWithCode(
      () => runResolverCLI({
        arguments_: [
          ...baseArguments,
          '--output', changedOutput,
          ...changedArguments,
        ],
        environment: { GITHUB_TOKEN: TOKEN },
        fetchImpl: createState().fetchImpl,
        traceImpl: NO_TRACE,
      }),
      'resolution-changed',
    )
    assert.equal(existsSync(changedOutput), false)

    await rejectsWithCode(
      () => runResolverCLI({
        arguments_: [
          ...baseArguments,
          '--output', join(temporaryDirectory, 'partial.json'),
          '--expect-ci-run-id', '3001',
        ],
        environment: { GITHUB_TOKEN: TOKEN },
        fetchImpl: createState().fetchImpl,
        traceImpl: NO_TRACE,
      }),
      'invalid-cli-options',
    )

    await rejectsWithCode(
      () => runResolverCLI({
        arguments_: [
          ...baseArguments,
          '--output', githubOutputPath,
          '--github-output', githubOutputPath,
        ],
        environment: { GITHUB_TOKEN: TOKEN },
        fetchImpl: createState().fetchImpl,
        traceImpl: NO_TRACE,
      }),
      'invalid-cli-options',
    )
  } finally {
    await rm(temporaryDirectory, { recursive: true, force: true })
  }
})

function createState() {
  const state = {
    repository: {
      id: REPOSITORY_ID,
      full_name: REPOSITORY,
      default_branch: DEFAULT_BRANCH,
    },
    workflows: new Map(),
    runPages: new Map(),
    attemptRuns: new Map(),
    baseRuns: new Map(),
    jobPages: new Map(),
    artifactPages: new Map(),
    calls: [],
  }

  let nextJobID = 4001
  let nextArtifactID = 5001
  for (const [authorityKey, descriptor] of Object.entries(AUTHORITIES)) {
    state.workflows.set(descriptor.workflowFile, {
      id: descriptor.workflowId,
      name: descriptor.workflowName,
      path: descriptor.workflowPath,
      state: 'active',
    })
    const run = createRun(descriptor)
    for (const event of allowedEvents(authorityKey)) {
      state.runPages.set(
        `${descriptor.workflowId}:${event}:1`,
        collection('workflow_runs', event === descriptor.event ? [run] : []),
      )
    }
    const jobIDs = descriptor.jobNames.map(() => nextJobID++)
    const artifactIDs = descriptor.artifactRoles.map(() => nextArtifactID++)
    installRunEvidence(state, authorityKey, run, { jobIDs, artifactIDs })
  }

  state.fetchImpl = createRouter(state)
  return state
}

function createRun(descriptor) {
  return {
    id: descriptor.runId,
    run_attempt: descriptor.runAttempt,
    workflow_id: descriptor.workflowId,
    name: descriptor.workflowName,
    path: descriptor.workflowPath,
    head_sha: TARGET_SHA,
    head_branch: DEFAULT_BRANCH,
    event: descriptor.event,
    status: 'completed',
    conclusion: 'success',
    created_at: descriptor.createdAt,
    repository: { id: REPOSITORY_ID },
    head_repository: { id: REPOSITORY_ID },
  }
}

function installRunEvidence(state, authorityKey, run, {
  jobIDs,
  artifactIDs,
}) {
  const descriptor = AUTHORITIES[authorityKey]
  state.attemptRuns.set(`${run.id}:${run.run_attempt}`, clone(run))
  state.baseRuns.set(run.id, clone(run))
  state.jobPages.set(
    `${run.id}:${run.run_attempt}:1`,
    collection('jobs', descriptor.jobNames.map((name, index) => ({
      id: jobIDs[index],
      name,
      run_id: run.id,
      run_attempt: run.run_attempt,
      head_sha: TARGET_SHA,
      head_branch: DEFAULT_BRANCH,
      workflow_name: descriptor.workflowName,
      status: 'completed',
      conclusion: 'success',
    }))),
  )
  descriptor.artifactRoles.forEach((role, index) => {
    const name = artifactName(role, run.id, run.run_attempt)
    state.artifactPages.set(
      artifactPageKey(run.id, name, 1),
      collection('artifacts', [{
        id: artifactIDs[index],
        name,
        size_in_bytes: 100 + artifactIDs[index] - 5000,
        expired: false,
        digest: `sha256:${String.fromCharCode(98 + artifactIDs[index] - 5001).repeat(64)}`,
        workflow_run: {
          id: run.id,
          repository_id: REPOSITORY_ID,
          head_repository_id: REPOSITORY_ID,
          head_branch: DEFAULT_BRANCH,
          head_sha: TARGET_SHA,
        },
      }]),
    )
  })
}

function createRouter(state) {
  return async (rawURL, options) => {
    const url = new URL(rawURL)
    state.calls.push({ url, options })
    const rootPath = '/repos/WindShare/windshare'
    if (url.pathname === rootPath) return apiResponse(state.repository)

    const workflowMetadata = url.pathname.match(
      /^\/repos\/WindShare\/windshare\/actions\/workflows\/([^/]+)$/u,
    )
    if (workflowMetadata !== null) {
      const value = state.workflows.get(decodeURIComponent(workflowMetadata[1]))
      return value === undefined ? apiResponse({}, 404) : apiResponse(value)
    }

    const workflowRuns = url.pathname.match(
      /^\/repos\/WindShare\/windshare\/actions\/workflows\/(\d+)\/runs$/u,
    )
    if (workflowRuns !== null) {
      assert.equal(url.searchParams.get('branch'), DEFAULT_BRANCH)
      assert.equal(url.searchParams.get('status'), 'success')
      assert.equal(url.searchParams.get('head_sha'), TARGET_SHA)
      assert.equal(url.searchParams.get('per_page'), String(PAGE_SIZE))
      const key = `${workflowRuns[1]}:${url.searchParams.get('event')}:${url.searchParams.get('page')}`
      return apiResponse(
        state.runPages.get(key) ?? collection('workflow_runs', []),
      )
    }

    const attemptJobs = url.pathname.match(
      /^\/repos\/WindShare\/windshare\/actions\/runs\/(\d+)\/attempts\/(\d+)\/jobs$/u,
    )
    if (attemptJobs !== null) {
      assert.equal(url.searchParams.has('filter'), false)
      const key = `${attemptJobs[1]}:${attemptJobs[2]}:${url.searchParams.get('page')}`
      return apiResponse(state.jobPages.get(key) ?? collection('jobs', []))
    }

    const attemptRun = url.pathname.match(
      /^\/repos\/WindShare\/windshare\/actions\/runs\/(\d+)\/attempts\/(\d+)$/u,
    )
    if (attemptRun !== null) {
      const value = state.attemptRuns.get(`${attemptRun[1]}:${attemptRun[2]}`)
      return value === undefined ? apiResponse({}, 404) : apiResponse(value)
    }

    const artifacts = url.pathname.match(
      /^\/repos\/WindShare\/windshare\/actions\/runs\/(\d+)\/artifacts$/u,
    )
    if (artifacts !== null) {
      const name = url.searchParams.get('name')
      const key = artifactPageKey(
        Number(artifacts[1]),
        name,
        Number(url.searchParams.get('page')),
      )
      return apiResponse(
        state.artifactPages.get(key) ?? collection('artifacts', []),
      )
    }

    const baseRun = url.pathname.match(
      /^\/repos\/WindShare\/windshare\/actions\/runs\/(\d+)$/u,
    )
    if (baseRun !== null) {
      const value = state.baseRuns.get(Number(baseRun[1]))
      return value === undefined ? apiResponse({}, 404) : apiResponse(value)
    }

    return apiResponse({}, 404)
  }
}

function allowedEvents(authorityKey) {
  return authorityKey === 'ci'
    ? ['push', 'workflow_dispatch']
    : ['schedule', 'workflow_dispatch']
}

function artifactName(role, runID, runAttempt) {
  if (role === 'browser') {
    return `browser-full-${TARGET_SHA}-${runID}-${runAttempt}`
  }
  return `stability-integration-${role}-${TARGET_SHA}-${runID}-${runAttempt}`
}

function artifactPageKey(runID, name, page) {
  return `${runID}:${name}:${page}`
}

function collection(field, values) {
  return {
    total_count: values.length,
    [field]: values,
  }
}

function apiResponse(value, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    redirected: false,
    async json() {
      return clone(value)
    },
  }
}

function clone(value) {
  return structuredClone(value)
}

async function resolve(state) {
  return resolveReleaseEvidence({
    repository: REPOSITORY,
    defaultBranch: DEFAULT_BRANCH,
    targetSha: TARGET_SHA,
    token: TOKEN,
    fetchImpl: state.fetchImpl,
    traceImpl: NO_TRACE,
  })
}

function createClient(fetchImpl) {
  return createGitHubActionsClient({
    repository: REPOSITORY,
    token: TOKEN,
    fetchImpl,
  })
}

function listRuns(client) {
  return client.listWorkflowRuns({
    workflowId: 1,
    defaultBranch: DEFAULT_BRANCH,
    event: 'push',
    targetSha: TARGET_SHA,
  })
}

function collectionFetch(field, pages) {
  return async (rawURL) => {
    const url = new URL(rawURL)
    const page = Number(url.searchParams.get('page'))
    return apiResponse(pages.get(page) ?? {
      total_count: 0,
      [field]: [],
    })
  }
}

async function rejectsWithCode(operation, expectedCode) {
  await assert.rejects(
    operation,
    (error) => {
      assert.ok(error instanceof Error)
      assert.equal(error.code, expectedCode)
      return true
    },
  )
}
